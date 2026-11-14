package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
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
	return factBackend(t, st), st, person.ID, target
}

func factBackend(t *testing.T, st *store.Store) *daemonMCPOperations {
	t.Helper()
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	return sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
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

	result, err := backend.ExecuteOperation(t.Context(), "list_person_fact_claims", map[string]any{"person_id": id, "include_evidence": true, "include_decisions": true})
	requirements.NoError(err)
	requirements.False(result.IsError)
	output := operationOutput[mcpserver.PersonFactClaimsOutput](t, result)
	assertions.Len(output.Claims, 1)
	assertions.Len(output.Evidence, 1)
	assertions.Len(output.Decisions, 1)
	args := map[string]any{"person_id": id, "kind": string(target.Kind), "key": target.Key, "pinned": true}
	disclosure, err := backend.OperationDisclosure(t.Context(), "set_person_fact_pin", args)
	requirements.NoError(err)
	assertions.Contains(disclosure, `"pinned":true`)
	assertions.Contains(disclosure, "may change profile and vCard projections")
	result, err = backend.ExecuteOperation(t.Context(), "set_person_fact_pin", args)
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
}

func TestPersonFactTrackingDisclosure(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backend, _, id, _ := factFixture(t)
	disclosure, err := backend.OperationDisclosure(t.Context(), "set_person_tracking", map[string]any{"person_id": id, "tracked": false})
	requirements.NoError(err)
	assertions.Contains(disclosure, `"tracked":false`)
	assertions.Contains(disclosure, "removes queued sweep work")
	assertions.Contains(disclosure, "pending enrichment")
	disclosure, err = backend.OperationDisclosure(t.Context(), "set_person_tracking", map[string]any{"person_id": id, "tracked": true})
	requirements.NoError(err)
	assertions.Contains(disclosure, "configured sweep model")
	assertions.Contains(disclosure, "external enrichment")
}

func TestPersonFactPinRefusals(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backend, st, id, target := factFixture(t)
	_, err := st.SetPersonTrackingContext(t.Context(), id, false)
	requirements.NoError(err)
	result, err := backend.ExecuteOperation(t.Context(), "set_person_fact_pin", map[string]any{"person_id": id, "kind": string(target.Kind), "key": target.Key, "pinned": true})
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
}

func TestPersonFactClaimsRefusals(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backend, _, id, _ := factFixture(t)
	result, err := backend.ExecuteOperation(t.Context(), "list_person_fact_claims", map[string]any{"person_id": id, "target": "invalid"})
	requirements.NoError(err)
	requirements.True(result.IsError)
	data, err := json.Marshal(result.Output)
	requirements.NoError(err)
	assertions.JSONEq(`{"error":"invalid_target","operation_may_have_completed":false}`, string(data))
}

func TestPersonFactClaimsExcludeSensitiveContent(t *testing.T) {
	backend, st, id, publicTarget := factFixture(t)
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(t, err)
	var sensitiveTarget personfacts.TargetDescriptor
	for _, target := range catalog.Targets {
		if target.Slug == store.AttributeSlugReligion {
			sensitiveTarget = target
		}
	}
	require.True(t, sensitiveTarget.Sensitive)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = st.ApplyPersonFactGenerationContext(t.Context(), personfacts.GenerationInput{
		PersonID: id, SourceCursors: []personfacts.SourceCursor{{Lane: "fixture", Start: "start", End: "end"}},
		ProgramID: "fixture", ProgramVersion: "v1", ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: "fixture", Provider: "fixture", ProviderVersion: "v1", Model: "fixture", ModelVersion: "v1", ResolvedAt: now,
		Policy: personfacts.PolicyContext{AllowSensitive: true, ProviderPolicyFingerprint: "fixture"},
		Claims: []personfacts.ProposedClaim{
			{Target: publicTarget, Relation: personfacts.RelationSupport, SubmittedValue: jsontext.Value(`"chat"`), Origin: personfacts.OriginExtraction, Confidence: personfacts.ConfidenceInputs{ReportedScore: 900}, Evidence: []personfacts.EvidenceInput{{PersonID: id, SourceClass: personfacts.EvidencePublic, Directness: personfacts.DirectSelf, Authority: personfacts.AuthorityAuthoritative, SourceURL: "https://example.test/public", SubjectPersonID: &id, SubjectRef: "synthetic", Excerpt: "Public fixture evidence", SourceVersion: "v1", EventTime: now, RecordedTime: now, IdentityScore: 990}}},
			{Target: sensitiveTarget, Relation: personfacts.RelationSupport, SubmittedValue: jsontext.Value(`"Sensitive fixture belief"`), Origin: personfacts.OriginExtraction, Confidence: personfacts.ConfidenceInputs{ReportedScore: 900}, Evidence: []personfacts.EvidenceInput{{PersonID: id, SourceClass: personfacts.EvidencePublic, Directness: personfacts.DirectSelf, Authority: personfacts.AuthorityAuthoritative, SourceURL: "https://example.test/sensitive", SubjectPersonID: &id, SubjectRef: "synthetic", Excerpt: "Sensitive fixture evidence", SourceVersion: "v1", EventTime: now, RecordedTime: now, IdentityScore: 990}}},
		},
	}, nil)
	require.NoError(t, err)
	claims, err := st.ListPersonFactClaimsContext(t.Context(), id, personfacts.ClaimFilter{})
	require.NoError(t, err)
	var sensitiveKey, publicKey string
	for _, claim := range claims {
		switch claim.Target.Key {
		case sensitiveTarget.Key:
			sensitiveKey = claim.ClaimKey
		case publicTarget.Key:
			publicKey = claim.ClaimKey
		}
	}
	require.NotEmpty(t, sensitiveKey)
	require.NotEmpty(t, publicKey)
	decisions, err := st.ListPersonFactDecisionsContext(t.Context(), id, personfacts.DecisionFilter{})
	require.NoError(t, err)
	require.True(t, slices.ContainsFunc(decisions, func(decision personfacts.Decision) bool { return decision.ClaimKey == sensitiveKey }))
	require.True(t, slices.ContainsFunc(decisions, func(decision personfacts.Decision) bool { return decision.ClaimKey == publicKey }))
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyPersonFacts}, nil)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_fact_claims", Arguments: map[string]any{"person_id": id, "include_evidence": true, "include_decisions": true}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	assert.Contains(t, string(data), "chat")
	assert.Contains(t, string(data), "Public fixture evidence")
	assert.NotContains(t, string(data), "Sensitive fixture belief")
	assert.NotContains(t, string(data), "Sensitive fixture evidence")
	var output mcpserver.PersonFactClaimsOutput
	require.NoError(t, json.Unmarshal(data, &output))
	assert.True(t, slices.ContainsFunc(output.Decisions, func(decision generated.PersonFactDecision) bool { return decision.ClaimKey == publicKey }))
	assert.NotContains(t, string(data), sensitiveKey)
}

func TestPersonFactCapabilitiesRequireExactContracts(t *testing.T) {
	requirements := require.New(t)
	backend := factBackend(t, testutil.NewSQLiteTestStore(t))
	caps, err := backend.client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	for i := range caps.Routes {
		r := &caps.Routes[i]
		if r.OperationID == "listPersonFactEvidence" {
			r.QueryParameters = slices.DeleteFunc(r.QueryParameters, func(v string) bool { return v == "target" })
		}
	}
	assert.NotContains(t, newDaemonMCPOperations(backend.client, caps).capabilities(), "list_person_fact_claims")
}

func TestPersonFactSDKConfirmsEachWrite(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend, st, id, target := factFixture(t)
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyPersonFacts}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		if approvals == 3 {
			assertions.Contains(request.Params.Message, "set_person_fact_pin")
			assertions.Contains(request.Params.Message, target.Key)
			pins, err := st.ListPersonFactPinsContext(t.Context(), id)
			if err != nil {
				return nil, err
			}
			assertions.False(slices.ContainsFunc(pins, func(pin personfacts.PinState) bool { return pin.Target.Key == target.Key && pin.Pinned }))
		}
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	for i, tracked := range []bool{false, true} {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_tracking", Arguments: map[string]any{"person_id": id, "tracked": tracked}})
		requirements.NoError(err)
		requirements.False(result.IsError)
		state, err := st.GetPersonTrackingContext(t.Context(), id)
		requirements.NoError(err)
		assertions.Equal(tracked, state.Tracked)
		assertions.Equal(i+1, approvals)
	}
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_fact_pin", Arguments: map[string]any{"person_id": id, "kind": string(target.Kind), "key": target.Key, "pinned": true}})
	requirements.NoError(err)
	requirements.False(result.IsError)
	assertions.Equal(3, approvals)
	data, err := json.Marshal(result.StructuredContent)
	requirements.NoError(err)
	var write generated.PersonFactPinWrite
	requirements.NoError(json.Unmarshal(data, &write))
	assertions.True(write.State.Pinned)
	assertions.Equal(target.Key, write.State.Target.Key)
	pins, err := st.ListPersonFactPinsContext(t.Context(), id)
	requirements.NoError(err)
	assertions.True(slices.ContainsFunc(pins, func(pin personfacts.PinState) bool { return pin.Target.Key == target.Key && pin.Pinned }))
}
