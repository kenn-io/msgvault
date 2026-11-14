package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func factFixture(t *testing.T, observe ...func(*http.Request)) (*daemonMCPOperations, *store.Store, int64, personfacts.TargetDescriptor) {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	participant, err := st.EnsureParticipant("facts@example.test", "Synthetic Person", "example.test")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	_, err = st.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(t, err)
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(t, err)
	var target personfacts.TargetDescriptor
	for _, item := range catalog.Targets {
		if item.Slug == store.AttributeSlugPrimaryChannel {
			target = item
		}
	}
	require.NotEmpty(t, target.Key)
	return factBackend(t, st, observe...), st, person.ID, target
}

func factBackend(t *testing.T, st *store.Store, observe ...func(*http.Request)) *daemonMCPOperations {
	t.Helper()
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)})
	if len(observe) == 0 {
		return sourceOperationFixture(t, fixture)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { observe[0](r); fixture.Router().ServeHTTP(w, r) }))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	caps, err := client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	return newDaemonMCPOperations(client, caps)
}

func TestPersonFactDisclosureAndPin(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend, st, id, _ := factFixture(t)
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	requirements.NoError(err)
	var target personfacts.TargetDescriptor
	for _, item := range catalog.Targets {
		if item.Slug == "employment" {
			target = item
		}
	}
	requirements.Equal("system:employment", target.Key)
	args := map[string]any{"person_id": id, "kind": string(target.Kind), "key": target.Key, "pinned": true}
	disclosure, err := backend.OperationDisclosure(t.Context(), "set_person_fact_pin", args)
	requirements.NoError(err)
	assertions.Contains(disclosure, `"pinned":true`)
	assertions.Contains(disclosure, "may change profile and vCard projections")
	disclosure, err = backend.OperationDisclosure(t.Context(), "set_person_tracking", map[string]any{"person_id": id, "tracked": false})
	requirements.NoError(err)
	assertions.Contains(disclosure, `"tracked":false`)
	assertions.Contains(disclosure, "removes queued sweep work")
	assertions.Contains(disclosure, "pending enrichment")
	disclosure, err = backend.OperationDisclosure(t.Context(), "set_person_tracking", map[string]any{"person_id": id, "tracked": true})
	requirements.NoError(err)
	assertions.Contains(disclosure, "configured sweep model")
	assertions.Contains(disclosure, "external enrichment")
	result, err := backend.ExecuteOperation(t.Context(), "set_person_fact_pin", args)
	requirements.NoError(err)
	requirements.False(result.IsError)
	pins, err := st.ListPersonFactPinsContext(t.Context(), id)
	requirements.NoError(err)
	assertions.True(slices.ContainsFunc(pins, func(pin personfacts.PinState) bool {
		return pin.Target.Kind == target.Kind && pin.Target.Key == target.Key && pin.Pinned
	}))
	result, err = backend.ExecuteOperation(t.Context(), "list_person_fact_pins", map[string]any{"person_id": id})
	requirements.NoError(err)
	assertions.NotEmpty(operationOutput[generated.PersonFactPinsResponse](t, result).Pins)
	args["pinned"] = false
	result, err = backend.ExecuteOperation(t.Context(), "set_person_fact_pin", args)
	requirements.NoError(err)
	requirements.False(result.IsError)
	pins, err = st.ListPersonFactPinsContext(t.Context(), id)
	requirements.NoError(err)
	assertions.False(slices.ContainsFunc(pins, func(pin personfacts.PinState) bool {
		return pin.Target.Kind == target.Kind && pin.Target.Key == target.Key && pin.Pinned
	}))
	for _, tracked := range []bool{false, true} {
		result, err := backend.ExecuteOperation(t.Context(), "set_person_tracking", map[string]any{"person_id": id, "tracked": tracked})
		requirements.NoError(err)
		requirements.False(result.IsError)
		state, err := st.GetPersonTrackingContext(t.Context(), id)
		requirements.NoError(err)
		assertions.Equal(tracked, state.Tracked)
	}
}

func TestPersonFactRefusals(t *testing.T) {
	requirements := require.New(t)
	backend, st, id, target := factFixture(t)
	_, err := st.SetPersonTrackingContext(t.Context(), id, false)
	requirements.NoError(err)
	for _, tc := range []struct {
		name, operation string
		args            map[string]any
		want            string
	}{
		{"untracked pin", "set_person_fact_pin", map[string]any{"person_id": id, "kind": string(target.Kind), "key": target.Key, "pinned": true}, `{"error":"person_fact_conflict","operation_may_have_completed":false}`},
		{"include requires target", "list_person_fact_claims", map[string]any{"person_id": id, "include_evidence": true}, `{"error":"invalid_arguments","operation_may_have_completed":false}`},
		{"invalid claims target", "list_person_fact_claims", map[string]any{"person_id": id, "target": "invalid"}, `{"error":"invalid_target","operation_may_have_completed":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			result, err := backend.ExecuteOperation(t.Context(), tc.operation, tc.args)
			requirements.NoError(err)
			requirements.True(result.IsError)
			data, err := json.Marshal(result.Output)
			requirements.NoError(err)
			assert.JSONEq(t, tc.want, string(data))
		})
	}
}

func TestPersonFactClaimsExcludeSensitiveContent(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	evidenceQuery := make(chan string, 1)
	backend, st, id, publicTarget := factFixture(t, func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/fact-evidence") {
			select {
			case evidenceQuery <- r.URL.Query().Get("visible_only"):
			default:
			}
		}
	})
	definition, err := st.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{UniversalID: "test-retired", ObjectType: store.AttributeObjectPerson, Slug: "retired_fixture", Label: "Retired fixture", Description: new("A synthetic fact"), ValueType: store.AttributeValueText, FieldType: store.AttributeFieldText, Cardinality: store.AttributeCardinalitySingle, Ownership: store.AttributeOwnershipUser, APIMutable: true, IsDeletable: true})
	requirements.NoError(err)
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	requirements.NoError(err)
	var sensitiveTarget, retiredTarget personfacts.TargetDescriptor
	for _, target := range catalog.Targets {
		if target.Slug == store.AttributeSlugReligion {
			sensitiveTarget = target
		}
		if target.Slug == "retired_fixture" {
			retiredTarget = target
		}
	}
	requirements.True(sensitiveTarget.Sensitive)
	requirements.NotEmpty(retiredTarget.Key)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sensitiveEvidence := personfacts.EvidenceInput{PersonID: id, SourceClass: personfacts.EvidencePublic, Directness: personfacts.DirectSelf, Authority: personfacts.AuthorityAuthoritative, SourceURL: "https://example.test/sensitive", SubjectPersonID: &id, SubjectRef: "synthetic", Excerpt: "Fixture evidence", SourceVersion: "v1", EventTime: now, RecordedTime: now, IdentityScore: 990}
	retiredEvidence := sensitiveEvidence
	retiredEvidence.SourceURL = "https://example.test/retired"
	_, err = st.ApplyPersonFactGenerationContext(t.Context(), personfacts.GenerationInput{
		PersonID: id, SourceCursors: []personfacts.SourceCursor{{Lane: "fixture", Start: "start", End: "end"}},
		ProgramID: "fixture", ProgramVersion: "v1", ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: "fixture", Provider: "fixture", ProviderVersion: "v1", Model: "fixture", ModelVersion: "v1", ResolvedAt: now,
		Policy: personfacts.PolicyContext{AllowSensitive: true, ProviderPolicyFingerprint: "fixture"},
		Claims: []personfacts.ProposedClaim{
			{Target: publicTarget, Relation: personfacts.RelationSupport, SubmittedValue: jsontext.Value(`"chat"`), Origin: personfacts.OriginExtraction, Confidence: personfacts.ConfidenceInputs{ReportedScore: 900}, Evidence: []personfacts.EvidenceInput{{PersonID: id, SourceClass: personfacts.EvidencePublic, Directness: personfacts.DirectSelf, Authority: personfacts.AuthorityAuthoritative, SourceURL: "https://example.test/public", SubjectPersonID: &id, SubjectRef: "synthetic", Excerpt: "Fixture evidence", SourceVersion: "v1", EventTime: now, RecordedTime: now, IdentityScore: 990}}},
			{Target: sensitiveTarget, Relation: personfacts.RelationSupport, SubmittedValue: jsontext.Value(`"Sensitive fixture belief"`), Origin: personfacts.OriginExtraction, Confidence: personfacts.ConfidenceInputs{ReportedScore: 900}, Evidence: []personfacts.EvidenceInput{sensitiveEvidence}},
			{Target: retiredTarget, Relation: personfacts.RelationSupport, SubmittedValue: jsontext.Value(`"retired"`), Origin: personfacts.OriginExtraction, Confidence: personfacts.ConfidenceInputs{ReportedScore: 900}, Evidence: []personfacts.EvidenceInput{retiredEvidence}},
		},
	}, nil)
	requirements.NoError(err)
	claims, err := st.ListPersonFactClaimsContext(t.Context(), id, personfacts.ClaimFilter{})
	requirements.NoError(err)
	var sensitiveKey, publicKey, retiredKey string
	for _, claim := range claims {
		switch claim.Target.Key {
		case sensitiveTarget.Key:
			sensitiveKey = claim.ClaimKey
		case publicTarget.Key:
			publicKey = claim.ClaimKey
		case retiredTarget.Key:
			retiredKey = claim.ClaimKey
		}
	}
	requirements.NotEmpty(sensitiveKey)
	requirements.NotEmpty(publicKey)
	requirements.NotEmpty(retiredKey)
	decisions, err := st.ListPersonFactDecisionsContext(t.Context(), id, personfacts.DecisionFilter{})
	requirements.NoError(err)
	requirements.True(slices.ContainsFunc(decisions, func(decision personfacts.Decision) bool { return decision.ClaimKey == sensitiveKey }))
	requirements.True(slices.ContainsFunc(decisions, func(decision personfacts.Decision) bool { return decision.ClaimKey == publicKey }))
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyPersonFacts}, &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})

	publicRef, err := personfacts.EncodeTargetRef(personfacts.TargetRef{Kind: publicTarget.Kind, Key: publicTarget.Key, Revision: publicTarget.Revision})
	requirements.NoError(err)
	sensitiveRef, err := personfacts.EncodeTargetRef(personfacts.TargetRef{Kind: sensitiveTarget.Kind, Key: sensitiveTarget.Key, Revision: sensitiveTarget.Revision})
	requirements.NoError(err)
	_, err = st.UpdateAttributeDefinitionContext(t.Context(), definition.ID, definition.Revision, store.AttributeDefinitionUpdate{IsActive: new(false)})
	requirements.NoError(err)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_fact_claims", Arguments: map[string]any{"person_id": id, "limit": 3}})
	requirements.NoError(err)
	requirements.False(result.IsError)
	data, err := json.Marshal(result.StructuredContent)
	requirements.NoError(err)
	assertions.Contains(string(data), publicKey)
	assertions.NotContains(string(data), sensitiveKey)
	assertions.NotContains(string(data), retiredKey)
	var output mcpserver.PersonFactClaimsOutput
	requirements.NoError(json.Unmarshal(data, &output))
	requirements.NotNil(output.NextOffset)
	assertions.EqualValues(3, *output.NextOffset)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_fact_claims", Arguments: map[string]any{"person_id": id, "target": publicRef, "include_evidence": true, "include_decisions": true}})
	requirements.NoError(err)
	requirements.False(result.IsError)
	data, err = json.Marshal(result.StructuredContent)
	requirements.NoError(err)
	select {
	case value := <-evidenceQuery:
		assertions.Equal("true", value)
	default:
		assertions.Fail("evidence request was not sent")
	}
	assertions.NotContains(string(data), sensitiveKey)
	requirements.NoError(json.Unmarshal(data, &output))
	assertions.True(slices.ContainsFunc(output.Decisions, func(decision generated.PersonFactDecision) bool { return decision.ClaimKey == publicKey }))
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_fact_claims", Arguments: map[string]any{"person_id": id, "target": sensitiveRef}})
	requirements.NoError(err)
	assertions.True(result.IsError)
	data, err = json.Marshal(result.StructuredContent)
	requirements.NoError(err)
	assertions.Contains(string(data), "invalid_fact_target")
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_fact_pin", Arguments: map[string]any{"person_id": id, "kind": string(sensitiveTarget.Kind), "key": sensitiveTarget.Key, "pinned": true}})
	requirements.NoError(err)
	assertions.True(result.IsError)
	data, err = json.Marshal(result.StructuredContent)
	requirements.NoError(err)
	assertions.Contains(string(data), "invalid_fact_target")
	pins, err := st.ListPersonFactPinsContext(t.Context(), id)
	requirements.NoError(err)
	assertions.Empty(pins)
	_, err = st.SetPersonFactPinContext(t.Context(), id, personfacts.TargetRef{Kind: sensitiveTarget.Kind, Key: sensitiveTarget.Key, Revision: sensitiveTarget.Revision}, true, "fixture")
	requirements.NoError(err)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_fact_pins", Arguments: map[string]any{"person_id": id}})
	requirements.NoError(err)
	requirements.False(result.IsError)
	data, err = json.Marshal(result.StructuredContent)
	requirements.NoError(err)
	assertions.NotContains(string(data), sensitiveTarget.Key)
}

func TestPersonFactCapabilitiesRequireExactContracts(t *testing.T) {
	requirements := require.New(t)
	backend := factBackend(t, testutil.NewSQLiteTestStore(t))
	caps, err := backend.client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	for i := range caps.Routes {
		r := &caps.Routes[i]
		if r.OperationID == "listPersonFactEvidence" {
			r.QueryParameters = slices.DeleteFunc(r.QueryParameters, func(v string) bool { return v == "visible_only" })
		}
	}
	assert.NotContains(t, newDaemonMCPOperations(backend.client, caps).capabilities(), "list_person_fact_claims")
}
