package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/freshness"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type loopHTTPFixture struct {
	server                     *api.Server
	store                      *store.Store
	config                     *config.Config
	personID, loopID, sourceID int64
	projection                 *time.Time
}

type rejectingLoopOperationGate struct {
	beginCalls int
}

func (g *rejectingLoopOperationGate) BeginWork() (func(), bool) {
	return g.BeginWorkContext(context.Background())
}

func (g *rejectingLoopOperationGate) BeginWorkContext(context.Context) (func(), bool) {
	g.beginCalls++
	return func() {}, false
}

func newLoopHTTPFixture(t *testing.T) *loopHTTPFixture {
	return newLoopHTTPFixtureWithOperationGate(t, api.NewSerialOperationGate())
}

func newLoopHTTPFixtureWithOperationGate(t *testing.T, operationGate api.OperationGate) *loopHTTPFixture {
	t.Helper()
	f := storetest.New(t)
	now := time.Now().UTC()
	counterpart := f.EnsureParticipant("person@example.test", "Example Person", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(counterpart)
	require.NoError(t, err)
	message := f.CreateMessage("open-loop-api-anchor")
	fixture := &loopHTTPFixture{store: f.Store, personID: person.ID, sourceID: f.Source.ID, projection: &now}
	cfg := config.NewDefaultConfig()
	cfg.Server.APIKey = "loops-test-owner-key"
	cfg.Server.AgentAccess = true
	cfg.Accounts = []config.AccountSchedule{{Email: f.Source.Identifier, Enabled: false}}
	fixture.config = cfg
	service, err := loops.NewService(f.Store, cfg, func(context.Context) (freshness.ProjectionState, error) {
		return freshness.ProjectionState{ComputedAt: fixture.projection}, nil
	})
	require.NoError(t, err)
	participantID := counterpart
	owed := now.Add(-48 * time.Hour)
	require.NoError(t, f.Store.ApplyLoopVerdictContext(t.Context(), store.LoopApply{
		ConversationID: f.ConvID, Now: now,
		Verdict: store.LoopVerdict{Open: &store.LoopOpenDecision{
			TurnKey: fmt.Sprintf("turn:%d:reply_owed:%d", f.ConvID, message),
			Kind:    loops.ReplyOwed, Status: "open", Channel: "email",
			ConversationID: f.ConvID, SourceID: f.Source.ID, AnchorMessageID: message,
			LastActivityMessageID: message, CounterpartyParticipantID: &participantID,
			CounterpartyPersonID: &person.ID, OwedSinceAt: owed, LastActivityAt: owed,
			OpensAt: owed.Add(24 * time.Hour), Summary: "A synthetic question",
			EvidenceJSON:   json.RawMessage(fmt.Sprintf(`[{"message_id":%d,"conversation_id":%d,"occurred_at":%q,"subject":"Synthetic question"}]`, message, f.ConvID, owed.Format(time.RFC3339Nano))),
			RulesetVersion: 1, TurnMessageIDs: []int64{message},
		}},
	}))
	rows, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	fixture.loopID = rows[0].ID
	runner, err := loops.NewRunner(loops.RunnerOptions{
		Context: t.Context(), Run: func(context.Context) error { return nil },
		Rebuild:        func(context.Context) error { return nil },
		StartScheduled: func() (scheduler.JobDisposition, error) { return scheduler.JobStarted, nil },
	})
	require.NoError(t, err)
	fixture.server = api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: f.Store,
		Logger: slog.New(slog.DiscardHandler), LoopService: service, LoopRunner: runner,
		LoopState: f.Store.LoopDerivationStateContext, OperationGate: operationGate,
	})
	return fixture
}

func loopHTTPRequest(t *testing.T, server *api.Server, method, path, body, agentToken string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if agentToken == "" {
		req.Header.Set("X-Api-Key", "loops-test-owner-key")
	} else {
		req.Header.Set(apiprotocol.AgentTokenHeader, agentToken)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, req)
	return response
}

func TestLoopHTTPInvalidActionsNameLoopRequests(t *testing.T) {
	f := newLoopHTTPFixture(t)
	for _, path := range []string{fmt.Sprintf("/api/v1/loops/%d/done", f.loopID), fmt.Sprintf("/api/v1/loops/%d/snooze", f.loopID), "/api/v1/loops/suppressions"} {
		for _, body := range []string{`{"unknown":true}`, `{`, `{} {}`} {
			t.Run(path+"/"+body, func(t *testing.T) {
				response := loopHTTPRequest(t, f.server, http.MethodPost, path, body, "")
				assert.Equal(t, http.StatusBadRequest, response.Code)
				assert.Contains(t, strings.ToLower(response.Body.String()), "loop request")
				assert.NotContains(t, strings.ToLower(response.Body.String()), "person request")
			})
		}
	}
}

func TestLoopHTTPOwnerViewsAndActions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newLoopHTTPFixture(t)
	for _, path := range []string{"/api/v1/loops", "/api/v1/loops/waiting", "/api/v1/loops/status", fmt.Sprintf("/api/v1/loops/%d", f.loopID), fmt.Sprintf("/api/v1/people/%d/loops?limit=5", f.personID)} {
		response := loopHTTPRequest(t, f.server, http.MethodGet, path, "", "")
		assertions.Equal(http.StatusOK, response.Code, "%s: %s", path, response.Body.String())
		assertions.Contains(response.Body.String(), `"freshness"`)
		assertions.NotContains(response.Body.String(), `"body"`)
	}
	for _, action := range []string{"run", "rebuild"} {
		response := loopHTTPRequest(t, f.server, http.MethodPost, "/api/v1/loops/"+action, "", "")
		assertions.Equal(http.StatusOK, response.Code, response.Body.String())
		assertions.Contains(response.Body.String(), `"disposition":"started"`)
	}
	path := fmt.Sprintf("/api/v1/loops/%d/snooze", f.loopID)
	response := loopHTTPRequest(t, f.server, http.MethodPost, path, fmt.Sprintf(`{"until":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)), "")
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	response = loopHTTPRequest(t, f.server, http.MethodDelete, path, "", "")
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	mute := fmt.Sprintf(`{"target_kind":"person","person_id":%d,"dry_run":true}`, f.personID)
	response = loopHTTPRequest(t, f.server, http.MethodPost, "/api/v1/loops/suppressions", mute, "")
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Contains(response.Body.String(), `"would_close":1`)
	path = fmt.Sprintf("/api/v1/loops/%d/done", f.loopID)
	response = loopHTTPRequest(t, f.server, http.MethodPost, path, `{"note":"Handled elsewhere"}`, "")
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Contains(response.Body.String(), "Handled elsewhere")
	response = loopHTTPRequest(t, f.server, http.MethodPost, path, `{}`, "")
	assertions.Equal(http.StatusConflict, response.Code, response.Body.String())
	assertions.Contains(response.Body.String(), "loop_already_closed")
	response = loopHTTPRequest(t, f.server, http.MethodPost, "/api/v1/loops/suppressions", fmt.Sprintf(`{"target_kind":"person","person_id":%d}`, f.personID), "")
	requirements.Equal(http.StatusCreated, response.Code, response.Body.String())
	var muted loops.SuppressionResult
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &muted))
	requirements.NotNil(muted.Suppression)
	response = loopHTTPRequest(t, f.server, http.MethodGet, "/api/v1/loops/suppressions", "", "")
	assertions.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Contains(response.Body.String(), fmt.Sprintf(`"person_id":%d`, f.personID))
	response = loopHTTPRequest(t, f.server, http.MethodDelete, fmt.Sprintf("/api/v1/loops/suppressions/%d", muted.Suppression.ID), "", "")
	assertions.Equal(http.StatusNoContent, response.Code, response.Body.String())
}

func TestLoopHTTPRunnerTriggersBypassOperationGate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	gate := &rejectingLoopOperationGate{}
	f := newLoopHTTPFixtureWithOperationGate(t, gate)

	for _, action := range []string{"run", "rebuild"} {
		response := loopHTTPRequest(t, f.server, http.MethodPost, "/api/v1/loops/"+action, "", "")
		requirements.Equal(http.StatusOK, response.Code, response.Body.String())
		assertions.Contains(response.Body.String(), `"disposition":"started"`)
	}
	assertions.Zero(gate.beginCalls)

	response := loopHTTPRequest(t, f.server, http.MethodPost,
		fmt.Sprintf("/api/v1/loops/%d/done", f.loopID), `{"note":"Handled elsewhere"}`, "")
	assertions.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	assertions.Equal(1, gate.beginCalls)
}

func TestLoopHTTPFreshnessRefusalAndValidation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newLoopHTTPFixture(t)
	f.projection = nil
	response := loopHTTPRequest(t, f.server, http.MethodGet, "/api/v1/loops/waiting", "", "")
	requirements.Equal(http.StatusConflict, response.Code, response.Body.String())
	var stale loops.LoopsStaleResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &stale))
	assertions.Equal("loops_stale", stale.Error)
	assertions.True(stale.Freshness.Stale)
	assertions.Contains(stale.Freshness.StaleReasons, "projection_missing:open_loops")
	response = loopHTTPRequest(t, f.server, http.MethodGet, "/api/v1/loops/waiting?allow_stale=true", "", "")
	assertions.Equal(http.StatusOK, response.Code, response.Body.String())
	for _, path := range []string{"/api/v1/loops?limit=0", "/api/v1/loops?limit=201", "/api/v1/loops?cursor=bad", "/api/v1/loops?status=unknown", "/api/v1/loops?person_id=-1", "/api/v1/loops?source_id=bad", "/api/v1/loops?include_snoozed=bad", "/api/v1/loops/waiting?top=51", "/api/v1/loops/waiting?as_of=tomorrow", "/api/v1/loops/waiting?allow_stale=bad", "/api/v1/loops/0", "/api/v1/people/0/loops"} {
		response = loopHTTPRequest(t, f.server, http.MethodGet, path, "", "")
		assertions.Equal(http.StatusBadRequest, response.Code, "%s: %s", path, response.Body.String())
	}
	response = loopHTTPRequest(t, f.server, http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/snooze", f.loopID), `{"until":"2000-01-01T00:00:00Z"}`, "")
	assertions.Equal(http.StatusBadRequest, response.Code, response.Body.String())
}

func TestLoopHTTPDelegatedDeniedAndUnwiredUnavailable(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newLoopHTTPFixture(t)
	issued := loopHTTPRequest(t, f.server, http.MethodPost, "/api/v1/agent-tokens", fmt.Sprintf(`{"label":"loop-reader-test","permissions":["message.read"],"source_ids":[%d]}`, f.sourceID), "")
	requirements.Equal(http.StatusCreated, issued.Code, issued.Body.String())
	var token struct {
		Secret string `json:"secret"`
	}
	requirements.NoError(json.Unmarshal(issued.Body.Bytes(), &token))
	requirements.NotEmpty(token.Secret)
	secret := token.Secret
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/loops", ""}, {http.MethodGet, "/api/v1/loops/waiting", ""},
		{http.MethodGet, fmt.Sprintf("/api/v1/loops/%d", f.loopID), ""}, {http.MethodGet, "/api/v1/loops/status", ""},
		{http.MethodGet, fmt.Sprintf("/api/v1/people/%d/loops", f.personID), ""},
		{http.MethodPost, "/api/v1/loops/run", ""}, {http.MethodPost, "/api/v1/loops/rebuild", ""},
		{http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/done", f.loopID), `{}`},
		{http.MethodGet, "/api/v1/loops/suppressions", ""}, {http.MethodPost, "/api/v1/loops/suppressions", `{}`},
		{http.MethodDelete, "/api/v1/loops/suppressions/1", ""},
		{http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/snooze", f.loopID), `{}`},
		{http.MethodDelete, fmt.Sprintf("/api/v1/loops/%d/snooze", f.loopID), ""},
	} {
		response := loopHTTPRequest(t, f.server, route.method, route.path, route.body, secret)
		assertions.Equal(http.StatusForbidden, response.Code, "%s %s: %s", route.method, route.path, response.Body.String())
	}
	unwired := api.NewServer(f.config, f.store, nil, slog.New(slog.DiscardHandler))
	response := loopHTTPRequest(t, unwired, http.MethodGet, "/api/v1/loops", "", "")
	assertions.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
}

func TestLoopHTTPDiscoveryAndDisabledService(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newLoopHTTPFixture(t)
	response := loopHTTPRequest(t, f.server, http.MethodGet, "/api/v1/mcp/capabilities", "", "")
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var capabilities apiprotocol.MCPCapabilities
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &capabilities))
	operations := make(map[string]bool)
	for _, route := range capabilities.Routes {
		operations[route.OperationID] = true
	}
	assertions.True(operations["listOpenLoops"])
	assertions.True(operations["closeOpenLoop"])
	cfg := config.NewDefaultConfig()
	cfg.Server.APIKey = "loops-test-owner-key"
	cfg.Loops.Enabled = false
	service, err := loops.NewService(f.store, cfg, nil)
	requirements.NoError(err)
	disabled := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: f.store, LoopService: service, Logger: slog.New(slog.DiscardHandler)})
	response = loopHTTPRequest(t, disabled, http.MethodGet, "/api/v1/loops", "", "")
	assertions.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	response = loopHTTPRequest(t, disabled, http.MethodGet, "/api/v1/mcp/capabilities", "", "")
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.NotContains(response.Body.String(), `"operation_id":"listOpenLoops"`)
	assertions.NotContains(response.Body.String(), `"operation_id":"closeOpenLoop"`)
}

func TestLoopHTTPOpenAPIContract(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	doc := api.OpenAPIDocument()
	path := doc.Paths["/api/v1/loops/waiting"]
	requirements.NotNil(path)
	requirements.NotNil(path.Get)
	assertions.Equal("getWaitingLoops", path.Get.OperationID)
	conflict := path.Get.Responses["409"]
	requirements.NotNil(conflict)
	media := conflict.Content["application/json"]
	requirements.NotNil(media)
	requirements.NotNil(media.Schema)
	assertions.Equal("#/components/schemas/LoopsStaleResponse", media.Schema.Ref)
	person := doc.Paths["/api/v1/people/{id}/loops"]
	requirements.NotNil(person)
	requirements.NotNil(person.Get)
	var hasLimit bool
	for _, parameter := range person.Get.Parameters {
		if parameter.Name == "limit" {
			hasLimit = true
			requirements.NotNil(parameter.Schema.Minimum)
			requirements.NotNil(parameter.Schema.Maximum)
			assertions.InDelta(float64(1), *parameter.Schema.Minimum, 0)
			assertions.InDelta(float64(200), *parameter.Schema.Maximum, 0)
		}
	}
	assertions.True(hasLimit, "generated person client must expose the bounded limit")
}

func TestLoopHTTPNullableObjectSchemas(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	doc := api.OpenAPIDocument()
	schemas := doc.Components.Schemas.Map()
	preview := schemas["LoopSuppressionResult"]
	requirements.NotNil(preview)
	property := preview.Properties["suppression"]
	requirements.NotNil(property)
	var previewAllowsNull bool
	for _, variant := range property.OneOf {
		previewAllowsNull = previewAllowsNull || variant.Type == "null"
	}
	assertions.True(previewAllowsNull, "a dry-run response retains suppression:null")
	report := schemas["FreshnessReport"]
	requirements.NotNil(report)
	projections := report.Properties["projections"]
	requirements.NotNil(projections)
	value, ok := projections.AdditionalProperties.(*huma.Schema)
	requirements.True(ok)
	var valueAllowsNull bool
	for _, variant := range value.OneOf {
		valueAllowsNull = valueAllowsNull || variant.Type == "null"
	}
	assertions.True(valueAllowsNull, "an unwired requested surface retains a null projection map value")
	// Always-present pointer fields are emitted as null by the daemon. Verify
	// Huma's actual schema, including primitive pointers it does not infer.
	for name, wireType := range map[string]reflect.Type{
		"Loop":                reflect.TypeFor[loops.Loop](),
		"LoopCounterparty":    reflect.TypeFor[loops.Counterparty](),
		"LoopSuppression":     reflect.TypeFor[loops.Suppression](),
		"LoopDerivationState": reflect.TypeFor[store.LoopDerivationState](),
		"SourceFreshness":     reflect.TypeFor[freshness.SourceFreshness](),
		"ProjectionFreshness": reflect.TypeFor[freshness.ProjectionFreshness](),
	} {
		for field := range wireType.Fields() {
			tag := field.Tag.Get("json")
			if field.Type.Kind() != reflect.Pointer || strings.Contains(tag, ",omitempty") {
				continue
			}
			propertyName, _, _ := strings.Cut(tag, ",")
			property := schemas[name].Properties[propertyName]
			requirements.NotNil(property, name+"."+propertyName)
			assertions.True(property.Nullable, name+"."+propertyName+" must accept explicit null")
		}
	}
}

func TestLoopGeneratedNullableObjects(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	const body = `{"as_of":"2026-10-17T12:00:00Z","projections":{"open_loops":null},"sources":[],"stale":true,"stale_after_seconds":86400,"stale_reasons":[],"warnings":[]}`
	var report generated.FreshnessReport
	requirements.NoError(json.Unmarshal([]byte(body), &report))
	encoded, err := json.Marshal(report)
	requirements.NoError(err)
	assertions.JSONEq(body, string(encoded), "an unwired surface must remain null through generated client decoding")
	requirements.NotPanics(func() { assert.NoError(t, report.Validate()) }, "generated validation must accept nullable map entries")

	var preview generated.LoopSuppressionResult
	requirements.NoError(json.Unmarshal([]byte(`{"suppression":null,"would_close":0}`), &preview))
	assertions.Nil(preview.Suppression)
}

// These are actual daemon DTOs, encoded exactly as the HTTP handlers encode
// them, then read by the generated public client. Null means unknown, not zero.
func TestLoopGeneratedDTOValuesRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	personID := int64(7)
	for _, tc := range []struct {
		name   string
		domain any
		wire   any
	}{
		{"projection", freshness.ProjectionFreshness{Stale: true, Extra: map[string]any{"queue_depth": 2, "signals_backfill_complete": false}}, &generated.ProjectionFreshness{}},
		{"source", freshness.SourceFreshness{SourceType: "eml", Identifier: "import", Scheduling: "static", Outcome: "never"}, &generated.SourceFreshness{}},
		{"counterparty", loops.Counterparty{ParticipantID: 1}, &generated.LoopCounterparty{}},
		{"suppression", loops.Suppression{ID: 1, TargetKind: "person", PersonID: &personID, CreatedAt: now}, &generated.LoopSuppression{}},
		{"state", store.LoopDerivationState{Singleton: 1, UpdatedAt: now}, &generated.LoopDerivationState{}},
		{"loop", loops.Loop{ID: 1, TurnKey: "turn:1:reply_owed:1", Cycle: 1, Kind: loops.ReplyOwed, Status: "open", Channel: "email", ConversationID: 1, SourceID: 1, Counterparty: loops.Counterparty{ParticipantID: 1}, OwedSinceAt: now.Add(-48 * time.Hour), LastActivityAt: now, OpensAt: now, Evidence: []loops.Evidence{}, Detector: "turn_state", AgendaState: "default"}, &generated.Loop{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			expected, err := json.Marshal(tc.domain)
			requirements.NoError(err)
			requirements.NoError(json.Unmarshal(expected, tc.wire))
			actual, err := json.Marshal(tc.wire)
			requirements.NoError(err)
			assert.JSONEq(t, string(expected), string(actual))
			if validator, ok := tc.wire.(interface{ Validate() error }); ok {
				requirements.NotPanics(func() { assert.NoError(t, validator.Validate()) })
			}
		})
	}
}
