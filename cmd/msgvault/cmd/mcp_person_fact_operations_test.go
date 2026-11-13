package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func factFixture(t *testing.T) (*daemonMCPOperations, *store.Store, int64, personfacts.TargetDescriptor) {
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
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	return backend, st, person.ID, target
}

func TestPersonFactNativeDiagnosticsAndPin(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend, st, id, target := factFixture(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), personfacts.GenerationInput{
		PersonID: id, SourceCursors: []personfacts.SourceCursor{{Lane: "fixture", Start: "start", End: "end"}},
		ProgramID: "fixture", ProgramVersion: "v1", ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: "fixture", Provider: "fixture", ProviderVersion: "v1", Model: "fixture", ModelVersion: "v1", ResolvedAt: now,
		Policy: personfacts.PolicyContext{ProviderPolicyFingerprint: "fixture"},
		Claims: []personfacts.ProposedClaim{{Target: target, Relation: personfacts.RelationSupport, SubmittedValue: jsontext.Value(`"chat"`), Origin: personfacts.OriginExtraction, Confidence: personfacts.ConfidenceInputs{ReportedScore: 900}, Evidence: []personfacts.EvidenceInput{{PersonID: id, SourceClass: personfacts.EvidencePublic, Directness: personfacts.DirectSelf, Authority: personfacts.AuthorityAuthoritative, SourceURL: "https://example.test/fact", SubjectPersonID: &id, SubjectRef: "synthetic", Excerpt: "Synthetic evidence", SourceVersion: "v1", EventTime: now, RecordedTime: now, IdentityScore: 990}}}},
	}, nil)
	requirements.NoError(err)

	result, err := backend.ExecuteOperation(t.Context(), "list_person_fact_claims", map[string]any{"person_id": id, "limit": 1, "include_evidence": true, "include_decisions": true})
	requirements.NoError(err)
	requirements.False(result.IsError)
	output := operationOutput[mcpserver.PersonFactClaimsOutput](t, result)
	assertions.Len(output.Claims, 1)
	assertions.Len(output.Evidence, 1)
	assertions.Len(output.Decisions, 1)
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	requirements.NoError(err)
	var employment personfacts.TargetDescriptor
	for _, item := range catalog.Targets {
		if item.Kind == personfacts.TargetEmployment {
			employment = item
			break
		}
	}
	requirements.NotEmpty(employment.Key)
	for _, item := range []personfacts.TargetDescriptor{target, employment} {
		args := map[string]any{"person_id": id, "kind": string(item.Kind), "key": item.Key, "pinned": true}
		disclosure, err := backend.OperationDisclosure(t.Context(), "set_person_fact_pin", args)
		requirements.NoError(err)
		assertions.Contains(disclosure, `"pinned":true`)
		result, err := backend.ExecuteOperation(t.Context(), "set_person_fact_pin", args)
		requirements.NoError(err)
		requirements.False(result.IsError)
		pins, err := st.ListPersonFactPinsContext(t.Context(), id)
		requirements.NoError(err)
		assertions.True(slices.ContainsFunc(pins, func(pin personfacts.PinState) bool {
			return pin.Target.Kind == item.Kind && pin.Target.Key == item.Key && pin.Pinned
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
			return pin.Target.Kind == item.Kind && pin.Target.Key == item.Key && pin.Pinned
		}))
	}
}

func TestPersonFactPinEscapedKeys(t *testing.T) {
	backend, st, id, _ := factFixture(t)
	description := "Synthetic fact"
	for _, tc := range []struct{ key, slug string }{{"test/slash", "slash_key"}, {"test%2Fkey", "percent_key"}} {
		_, err := st.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{UniversalID: tc.key, ObjectType: store.AttributeObjectPerson, Slug: tc.slug, Label: "Synthetic", Description: &description, ValueType: store.AttributeValueText, FieldType: store.AttributeFieldText, Cardinality: store.AttributeCardinalitySingle, Ownership: store.AttributeOwnershipUser, UICreatable: true, UIEditable: true, APIMutable: true, IsAudited: true, IsDeletable: true})
		require.NoError(t, err)
		result, err := backend.ExecuteOperation(t.Context(), "set_person_fact_pin", map[string]any{"person_id": id, "kind": "attribute", "key": tc.key, "pinned": true})
		require.NoError(t, err)
		require.False(t, result.IsError)
		pins, err := st.ListPersonFactPinsContext(t.Context(), id)
		require.NoError(t, err)
		assert.True(t, slices.ContainsFunc(pins, func(pin personfacts.PinState) bool { return pin.Target.Key == tc.key && pin.Pinned }))
	}
}

func TestPersonFactTrackingAndRefusals(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend, st, id, target := factFixture(t)
	disclosure, err := backend.OperationDisclosure(t.Context(), "set_person_tracking", map[string]any{"person_id": id, "tracked": false})
	requirements.NoError(err)
	assertions.Contains(disclosure, `"tracked":false`)
	result, err := backend.ExecuteOperation(t.Context(), "set_person_tracking", map[string]any{"person_id": id, "tracked": false})
	requirements.NoError(err)
	assertions.False(result.IsError)
	state, err := st.GetPersonTrackingContext(t.Context(), id)
	requirements.NoError(err)
	assertions.False(state.Tracked)

	for _, key := range []string{"", " k"} {
		_, err := backend.OperationDisclosure(t.Context(), "set_person_fact_pin", map[string]any{"person_id": id, "kind": "attribute", "key": key, "pinned": true})
		var refusal *mcpserver.OperationRefusalError
		requirements.ErrorAs(err, &refusal)
		assertions.Equal("invalid_arguments", refusal.Code)
	}

	for _, tc := range []struct {
		args map[string]any
		code string
	}{
		{map[string]any{"person_id": id, "limit": 201}, "invalid_limit"},
		{map[string]any{"person_id": id, "offset": -1}, "invalid_offset"},
		{map[string]any{"person_id": id, "target": "invalid"}, "invalid_target"},
	} {
		result, err = backend.ExecuteOperation(t.Context(), "list_person_fact_claims", tc.args)
		requirements.NoError(err)
		requirements.True(result.IsError)
		data, err := json.Marshal(result.Output)
		requirements.NoError(err)
		assertions.JSONEq(`{"error":"`+tc.code+`","operation_may_have_completed":false}`, string(data))
	}
	result, err = backend.ExecuteOperation(t.Context(), "set_person_fact_pin", map[string]any{"person_id": id, "kind": string(target.Kind), "key": target.Key, "pinned": true})
	requirements.NoError(err)
	requirements.True(result.IsError)
	data, err := json.Marshal(result.Output)
	requirements.NoError(err)
	assertions.JSONEq(`{"error":"person_fact_conflict","operation_may_have_completed":false}`, string(data))
	result, err = backend.ExecuteOperation(t.Context(), "set_person_fact_pin", map[string]any{"person_id": id, "kind": "attribute", "key": "not-active", "pinned": true})
	requirements.NoError(err)
	requirements.True(result.IsError)
	data, err = json.Marshal(result.Output)
	requirements.NoError(err)
	assertions.JSONEq(`{"error":"invalid_fact_target","operation_may_have_completed":false}`, string(data))

	stub := &daemonMCPOperations{client: newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal(http.MethodPut, r.Method)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"server_busy"}`))
	}), supported: []string{"set_person_tracking"}}
	result, err = stub.ExecuteOperation(t.Context(), "set_person_tracking", map[string]any{"person_id": id, "tracked": false})
	requirements.NoError(err)
	requirements.True(result.IsError)
	data, err = json.Marshal(result.Output)
	requirements.NoError(err)
	assertions.JSONEq(`{"error":"server_busy","operation_may_have_completed":false}`, string(data))
}

func TestPersonFactCapabilitiesRequireExactContracts(t *testing.T) {
	backend, _, _, _ := factFixture(t)
	for _, tc := range []struct {
		id, removed, name string
		query             bool
	}{
		{"listPersonFactEvidence", "target", "list_person_fact_claims", true},
		{"setPersonFactPin", "pinned", "set_person_fact_pin", false},
	} {
		caps, err := backend.client.MCPCapabilities(t.Context())
		require.NoError(t, err)
		for i := range caps.Routes {
			r := &caps.Routes[i]
			if r.OperationID != tc.id {
				continue
			}
			if tc.query {
				r.QueryParameters = slices.DeleteFunc(r.QueryParameters, func(v string) bool { return v == tc.removed })
			} else {
				r.RequestProperties = slices.DeleteFunc(r.RequestProperties, func(v string) bool { return v == tc.removed })
			}
		}
		assert.NotContains(t, newDaemonMCPOperations(backend.client, caps).capabilities(), tc.name)
	}
}

func TestPersonFactClaimsRejectEmptyTarget(t *testing.T) {
	backend, _, id, _ := factFixture(t)
	session := operationMCPSession(t, backend, nil, nil)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_fact_claims", Arguments: map[string]any{"person_id": id, "target": ""}})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_fact_claims", Arguments: map[string]any{"person_id": id}})
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestPersonFactSDKConfirmsEachWrite(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend, st, id, target := factFixture(t)
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyPersonFacts}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		if approvals == 1 {
			assertions.Contains(request.Params.Message, "removes queued sweep work")
			assertions.Contains(request.Params.Message, "pending enrichment")
		} else if approvals == 2 {
			assertions.Contains(request.Params.Message, "configured sweep model")
			assertions.Contains(request.Params.Message, "external enrichment")
		} else {
			assertions.Contains(request.Params.Message, "set_person_fact_pin")
			assertions.Contains(request.Params.Message, target.Key)
			pins, err := st.ListPersonFactPinsContext(t.Context(), id)
			assertions.NoError(err)
			assertions.False(slices.ContainsFunc(pins, func(pin personfacts.PinState) bool { return pin.Target.Key == target.Key && pin.Pinned }))
		}
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	read, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_fact_claims", Arguments: map[string]any{"person_id": id, "include_evidence": true, "include_decisions": true}})
	requirements.NoError(err)
	requirements.False(read.IsError)
	data, err := json.Marshal(read.StructuredContent)
	requirements.NoError(err)
	assertions.JSONEq(`{"claims":[],"evidence":[],"decisions":[]}`, string(data))
	read, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_fact_claims", Arguments: map[string]any{"person_id": id}})
	requirements.NoError(err)
	requirements.False(read.IsError)
	data, err = json.Marshal(read.StructuredContent)
	requirements.NoError(err)
	assertions.JSONEq(`{"claims":[]}`, string(data))
	for _, tracked := range []bool{false, true} {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_tracking", Arguments: map[string]any{"person_id": id, "tracked": tracked}})
		requirements.NoError(err)
		requirements.False(result.IsError)
	}
	assertions.Equal(2, approvals)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_fact_pin", Arguments: map[string]any{"person_id": id, "kind": string(target.Kind), "key": target.Key, "pinned": true}})
	requirements.NoError(err)
	requirements.False(result.IsError)
	assertions.Equal(3, approvals)
	data, err = json.Marshal(result.StructuredContent)
	requirements.NoError(err)
	var write generated.PersonFactPinWrite
	requirements.NoError(json.Unmarshal(data, &write))
	assertions.True(write.State.Pinned)
	assertions.Equal(target.Key, write.State.Target.Key)
}
