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
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
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

	rows, err := st.ListPersonFactEvidenceContext(t.Context(), id, personfacts.EvidenceFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	_, err = st.ApplyPersonFactGenerationContext(t.Context(), personfacts.GenerationInput{
		PersonID: id, SourceCursors: []personfacts.SourceCursor{{Lane: "fixture-status", Start: "off", End: "off-end"}},
		ProgramID: "fixture", ProgramVersion: "v1", ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: "fixture", Provider: "fixture", ProviderVersion: "v1", ResolvedAt: now.Add(time.Minute),
		Policy:                personfacts.PolicyContext{ProviderPolicyFingerprint: "fixture"},
		EvidenceStatusChanges: []personfacts.EvidenceStatusChange{{EvidenceKey: rows[0].Key, SourceVersion: "v1", Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted}},
	}, nil)
	require.NoError(t, err)
	for _, name := range []string{"list_person_fact_evidence", "list_person_fact_status_events", "list_person_fact_claims", "list_person_fact_decisions"} {
		result, err := backend.ExecuteOperation(t.Context(), name, map[string]any{"person_id": id, "limit": 1, "offset": 0})
		require.NoError(t, err)
		require.False(t, result.IsError, name)
		switch name {
		case "list_person_fact_evidence":
			assert.Len(t, operationOutput[generated.PersonFactEvidenceResponse](t, result).Evidence, 1)
		case "list_person_fact_status_events":
			events := operationOutput[generated.PersonFactEvidenceStatusEventsResponse](t, result).Events
			require.Len(t, events, 1)
			assert.Equal(t, rows[0].Key, events[0].EvidenceKey)
		case "list_person_fact_claims":
			assert.Len(t, operationOutput[generated.PersonFactClaimsResponse](t, result).Claims, 1)
		case "list_person_fact_decisions":
			assert.Len(t, operationOutput[generated.PersonFactDecisionsResponse](t, result).Decisions, 1)
		}
	}
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(t, err)
	var employment personfacts.TargetDescriptor
	for _, item := range catalog.Targets {
		if item.Kind == personfacts.TargetEmployment {
			employment = item
			break
		}
	}
	require.NotEmpty(t, employment.Key)
	for _, item := range []personfacts.TargetDescriptor{target, employment} {
		args := map[string]any{"person_id": id, "kind": string(item.Kind), "key": item.Key, "pinned": true}
		disclosure, err := backend.OperationDisclosure(t.Context(), "set_person_fact_pin", args)
		require.NoError(t, err)
		assert.Contains(t, disclosure, `"pinned":true`)
		result, err := backend.ExecuteOperation(t.Context(), "set_person_fact_pin", args)
		require.NoError(t, err)
		require.False(t, result.IsError)
		pins, err := st.ListPersonFactPinsContext(t.Context(), id)
		require.NoError(t, err)
		assert.True(t, slices.ContainsFunc(pins, func(pin personfacts.PinState) bool {
			return pin.Target.Kind == item.Kind && pin.Target.Key == item.Key && pin.Pinned
		}))
		result, err = backend.ExecuteOperation(t.Context(), "list_person_fact_pins", map[string]any{"person_id": id})
		require.NoError(t, err)
		assert.NotEmpty(t, operationOutput[generated.PersonFactPinsResponse](t, result).Pins)
		args["pinned"] = false
		result, err = backend.ExecuteOperation(t.Context(), "set_person_fact_pin", args)
		require.NoError(t, err)
		require.False(t, result.IsError)
		pins, err = st.ListPersonFactPinsContext(t.Context(), id)
		require.NoError(t, err)
		assert.False(t, slices.ContainsFunc(pins, func(pin personfacts.PinState) bool {
			return pin.Target.Kind == item.Kind && pin.Target.Key == item.Key && pin.Pinned
		}))
	}
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

	for _, tc := range []struct {
		args map[string]any
		code string
	}{
		{map[string]any{"person_id": id, "limit": 201}, "invalid_limit"},
		{map[string]any{"person_id": id, "offset": -1}, "invalid_offset"},
		{map[string]any{"person_id": id, "unknown": true}, "invalid_arguments"},
		{map[string]any{"person_id": id, "target": "invalid"}, "invalid_target"},
	} {
		result, err = backend.ExecuteOperation(t.Context(), "list_person_fact_claims", tc.args)
		require.NoError(t, err)
		require.True(t, result.IsError)
		data, err := json.Marshal(result.Output)
		require.NoError(t, err)
		assert.JSONEq(t, `{"error":"`+tc.code+`","operation_may_have_completed":false}`, string(data))
	}
	result, err = backend.ExecuteOperation(t.Context(), "set_person_fact_pin", map[string]any{"person_id": id, "kind": "attribute", "key": "not-active", "pinned": true})
	require.NoError(t, err)
	require.True(t, result.IsError)
	data, err := json.Marshal(result.Output)
	require.NoError(t, err)
	assert.JSONEq(t, `{"error":"invalid_fact_target","operation_may_have_completed":false}`, string(data))
	result, err = backend.ExecuteOperation(t.Context(), "get_person_tracking", map[string]any{"person_id": 999999})
	require.NoError(t, err)
	require.True(t, result.IsError)
	data, err = json.Marshal(result.Output)
	require.NoError(t, err)
	assert.JSONEq(t, `{"error":"person_profile_not_found","operation_may_have_completed":false}`, string(data))
}

func TestPersonFactCapabilitiesRequireExactContracts(t *testing.T) {
	backend, _, _, _ := factFixture(t)
	caps, err := backend.client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	for _, tc := range []struct {
		id, removed, name string
		query             bool
	}{
		{"listPersonFactEvidence", "target", "list_person_fact_evidence", true},
		{"setPersonFactPin", "pinned", "set_person_fact_pin", false},
	} {
		copyCaps := *caps
		copyCaps.Routes = slices.Clone(caps.Routes)
		for i := range copyCaps.Routes {
			r := &copyCaps.Routes[i]
			if r.OperationID != tc.id {
				continue
			}
			if tc.query {
				r.QueryParameters = slices.DeleteFunc(slices.Clone(r.QueryParameters), func(v string) bool { return v == tc.removed })
			} else {
				r.RequestProperties = slices.DeleteFunc(slices.Clone(r.RequestProperties), func(v string) bool { return v == tc.removed })
			}
		}
		has := func(id, method, path string, query ...string) bool {
			for _, r := range copyCaps.Routes {
				if r.OperationID == id && r.Method == method && r.Path == path {
					for _, q := range query {
						if !slices.Contains(r.QueryParameters, q) {
							return false
						}
					}
					return true
				}
			}
			return false
		}
		assert.NotContains(t, personFactMCPCapabilities(has, &copyCaps), tc.name)
	}
}

func TestPersonFactEmploymentPinAndSensitiveTargets(t *testing.T) {
	backend, st, _, _ := factFixture(t)
	description := "Synthetic sensitive fact"
	definition, err := st.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{UniversalID: "test-sensitive", ObjectType: store.AttributeObjectPerson, Slug: "test_sensitive", Label: "Synthetic sensitive", Description: &description, ValueType: store.AttributeValueText, FieldType: store.AttributeFieldText, Cardinality: store.AttributeCardinalitySingle, Ownership: store.AttributeOwnershipUser, UICreatable: true, UIEditable: true, APIMutable: true, IsAudited: true, IsDeletable: true})
	require.NoError(t, err)
	sensitive := true
	_, err = st.UpdateAttributeDefinitionContext(t.Context(), definition.ID, definition.Revision, store.AttributeDefinitionUpdate{IsSensitive: &sensitive})
	require.NoError(t, err)
	for _, tc := range []struct {
		args map[string]any
		want bool
	}{{nil, false}, {map[string]any{"include_sensitive": false}, false}, {map[string]any{"include_sensitive": true}, true}} {
		result, err := backend.ExecuteOperation(t.Context(), "list_person_fact_targets", tc.args)
		require.NoError(t, err)
		require.False(t, result.IsError)
		got := operationOutput[generated.Catalog](t, result)
		assert.Equal(t, tc.want, slices.ContainsFunc(got.Targets, func(target generated.TargetDescriptor) bool { return target.Sensitive }))
	}
}

func TestPersonFactSDKConfirmsEachTrackingWrite(t *testing.T) {
	backend, _, id, _ := factFixture(t)
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyPersonFacts}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		if approvals == 1 {
			assert.Contains(t, request.Params.Message, "removes queued sweep work")
		} else {
			assert.Contains(t, request.Params.Message, "configured sweep model")
		}
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	read, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_tracking", Arguments: map[string]any{"person_id": id}})
	require.NoError(t, err)
	require.False(t, read.IsError)
	assert.NotNil(t, read.StructuredContent)
	for _, tracked := range []bool{false, true} {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_tracking", Arguments: map[string]any{"person_id": id, "tracked": tracked}})
		require.NoError(t, err)
		require.False(t, result.IsError)
	}
	assert.Equal(t, 2, approvals)
}
