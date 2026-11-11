package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
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
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	return backend, st, person.ID, target
}

func TestPersonFactNativeDiagnosticsAndPin(t *testing.T) {
	backend, st, id, target := factFixture(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), personfacts.GenerationInput{
		PersonID: id, SourceCursors: []personfacts.SourceCursor{{Lane: "fixture", Start: "start", End: "end"}},
		ProgramID: "fixture", ProgramVersion: "v1", ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: "fixture", Provider: "fixture", ProviderVersion: "v1", Model: "fixture", ModelVersion: "v1", ResolvedAt: now,
		Policy: personfacts.PolicyContext{ProviderPolicyFingerprint: "fixture"},
		Claims: []personfacts.ProposedClaim{{Target: target, Relation: personfacts.RelationSupport, SubmittedValue: jsontext.Value(`"chat"`), Origin: personfacts.OriginExtraction, Confidence: personfacts.ConfidenceInputs{ReportedScore: 900}, Evidence: []personfacts.EvidenceInput{{PersonID: id, SourceClass: personfacts.EvidencePublic, Directness: personfacts.DirectSelf, Authority: personfacts.AuthorityAuthoritative, SourceURL: "https://example.test/fact", SubjectPersonID: &id, SubjectRef: "synthetic", Excerpt: "Synthetic evidence", SourceVersion: "v1", EventTime: now, RecordedTime: now, IdentityScore: 990}}}},
	}, nil)
	require.NoError(t, err)
	for _, name := range []string{"list_person_fact_evidence", "list_person_fact_status_events", "list_person_fact_claims", "list_person_fact_decisions"} {
		result, err := backend.ExecuteOperation(t.Context(), name, map[string]any{"person_id": id, "limit": 1, "offset": 0})
		require.NoError(t, err)
		require.False(t, result.IsError, name)
		data, err := json.Marshal(result.Output)
		require.NoError(t, err)
		assert.NotEmpty(t, data)
	}
	evidence, err := backend.ExecuteOperation(t.Context(), "list_person_fact_evidence", map[string]any{"person_id": id, "limit": 1})
	require.NoError(t, err)
	assert.Len(t, operationOutput[generated.PersonFactEvidenceResponse](t, evidence).Evidence, 1)
	claims, err := backend.ExecuteOperation(t.Context(), "list_person_fact_claims", map[string]any{"person_id": id, "limit": 1})
	require.NoError(t, err)
	assert.Len(t, operationOutput[generated.PersonFactClaimsResponse](t, claims).Claims, 1)
	args := map[string]any{"person_id": id, "kind": string(target.Kind), "key": target.Key, "pinned": true}
	disclosure, err := backend.OperationDisclosure(t.Context(), "set_person_fact_pin", args)
	require.NoError(t, err)
	assert.Contains(t, disclosure, `"pinned":true`)
	result, err := backend.ExecuteOperation(t.Context(), "set_person_fact_pin", args)
	require.NoError(t, err)
	require.False(t, result.IsError)
	pins, err := st.ListPersonFactPinsContext(t.Context(), id)
	require.NoError(t, err)
	require.NotEmpty(t, pins)
	assert.True(t, pins[0].Pinned)
	result, err = backend.ExecuteOperation(t.Context(), "list_person_fact_pins", map[string]any{"person_id": id})
	require.NoError(t, err)
	assert.NotEmpty(t, operationOutput[generated.PersonFactPinsResponse](t, result).Pins)
	args["pinned"] = false
	result, err = backend.ExecuteOperation(t.Context(), "set_person_fact_pin", args)
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestPersonFactTrackingAndRefusals(t *testing.T) {
	backend, st, id, _ := factFixture(t)
	disclosure, err := backend.OperationDisclosure(t.Context(), "set_person_tracking", map[string]any{"person_id": id, "tracked": false})
	require.NoError(t, err)
	assert.Contains(t, disclosure, `"tracked":false`)
	result, err := backend.ExecuteOperation(t.Context(), "set_person_tracking", map[string]any{"person_id": id, "tracked": false})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	state, err := st.GetPersonTrackingContext(t.Context(), id)
	require.NoError(t, err)
	assert.False(t, state.Tracked)
	result, err = backend.ExecuteOperation(t.Context(), "get_person_tracking", map[string]any{"person_id": id})
	require.NoError(t, err)
	assert.False(t, operationOutput[generated.PersonTracking](t, result).Tracked)
	for _, args := range []map[string]any{{"person_id": id, "limit": 201}, {"person_id": id, "offset": -1}, {"person_id": id, "unknown": true}} {
		result, err = backend.ExecuteOperation(t.Context(), "list_person_fact_claims", args)
		require.NoError(t, err)
		assert.True(t, result.IsError)
	}
	result, err = backend.ExecuteOperation(t.Context(), "set_person_fact_pin", map[string]any{"person_id": id, "kind": "attribute", "key": "not-active", "pinned": true})
	require.NoError(t, err)
	require.True(t, result.IsError)
	data, err := json.Marshal(result.Output)
	require.NoError(t, err)
	assert.Contains(t, string(data), "invalid_fact_target")
	assert.Contains(t, string(data), `"operation_may_have_completed":false`)
	result, err = backend.ExecuteOperation(t.Context(), "get_person_tracking", map[string]any{"person_id": 999999})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestPersonFactSDKCatalogAndRead(t *testing.T) {
	backend, _, id, _ := factFixture(t)
	session := operationMCPSession(t, backend, nil, nil)
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	names := make(map[string]bool)
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	assert.True(t, names["list_person_fact_targets"])
	assert.True(t, names["list_person_fact_evidence"])
	assert.False(t, names["set_person_fact_pin"])
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_tracking", Arguments: map[string]any{"person_id": id}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.NotNil(t, result.StructuredContent)
	enabled := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyPersonFacts}, nil)
	listed, err = enabled.ListTools(t.Context(), nil)
	require.NoError(t, err)
	names = make(map[string]bool)
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	assert.True(t, names["set_person_fact_pin"])
	assert.True(t, names["set_person_tracking"])
}

func TestPersonFactCapabilitiesRequireExactContracts(t *testing.T) {
	backend, _, _, _ := factFixture(t)
	assert.Contains(t, backend.capabilities(), "list_person_fact_evidence")
	assert.Contains(t, backend.capabilities(), "set_person_tracking")
	missing := newDaemonMCPOperations(backend.client, nil)
	assert.Empty(t, missing.capabilities())
	result, err := missing.ExecuteOperation(t.Context(), "get_person_tracking", map[string]any{"person_id": 1})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestPersonFactEmploymentPinAndSensitiveTargets(t *testing.T) {
	backend, st, id, _ := factFixture(t)
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(t, err)
	var employment personfacts.TargetDescriptor
	for _, target := range catalog.Targets {
		if target.Kind == personfacts.TargetEmployment {
			employment = target
			break
		}
	}
	require.NotEmpty(t, employment.Key)
	result, err := backend.ExecuteOperation(t.Context(), "set_person_fact_pin", map[string]any{"person_id": id, "kind": "employment", "key": employment.Key, "pinned": true})
	require.NoError(t, err)
	require.False(t, result.IsError)
	pins, err := st.ListPersonFactPinsContext(t.Context(), id)
	require.NoError(t, err)
	found := false
	for _, pin := range pins {
		if pin.Target.Kind == personfacts.TargetEmployment && pin.Target.Key == employment.Key {
			found = pin.Pinned
		}
	}
	assert.True(t, found)
	for _, sensitive := range []bool{false, true} {
		result, err = backend.ExecuteOperation(t.Context(), "list_person_fact_targets", map[string]any{"include_sensitive": sensitive})
		require.NoError(t, err)
		got := operationOutput[generated.Catalog](t, result)
		if !sensitive {
			for _, target := range got.Targets {
				assert.False(t, target.Sensitive)
			}
		}
	}
}

func TestPersonFactSDKConfirmsEachTrackingWrite(t *testing.T) {
	backend, st, id, _ := factFixture(t)
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyPersonFacts}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		assert.Contains(t, request.Params.Message, "tracked")
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	for _, tracked := range []bool{false, true} {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_tracking", Arguments: map[string]any{"person_id": id, "tracked": tracked}})
		require.NoError(t, err)
		require.False(t, result.IsError)
		state, err := st.GetPersonTrackingContext(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, tracked, state.Tracked)
	}
	assert.Equal(t, 2, approvals)
}
