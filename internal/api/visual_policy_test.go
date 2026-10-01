package api

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector/visual"
)

func TestVisualExpectedPolicyRequiresPublishedRuntimeBeforeEffects(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	generation, err := st.EnsureVisualGeneration(t.Context(), store.VisualGenerationSpec{Fingerprint: "initialized-generation", Model: "voyage-multimodal-3.5", Dimension: 1024})
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: st, Logger: testLogger()})
	calls := 0
	build := func(ctx context.Context, _ operations.PassScope) error {
		calls++
		return st.ConsentVisualGeneration(ctx, generation.ID, "initialized-policy")
	}
	srv.SetVisualOperations(build, build, func(context.Context, operations.PassScope, int64, string) error { calls++; return nil }, func(ctx context.Context, _ bool) (visual.Status, error) {
		current, err := st.GetVisualGeneration(ctx, generation.ID)
		return visual.Status{Generation: current}, err
	}, func(context.Context) error { calls++; return nil })
	for _, action := range []string{"build", "run", "retry", "retire"} {
		body, err := json.Marshal(map[string]any{"consent": true, "message_id": 1, "blob_hash": "synthetic-hash", "generation_id": generation.ID, "expected_generation_id": generation.ID, "expected_generation_fingerprint": generation.Fingerprint, "expected_policy_fingerprint": "observed-previous-policy"})
		requirements.NoError(err)
		response := doRequest(t, srv.Router(), http.MethodPost, "/api/v1/multimodal/"+action, body, nil)
		assertions.Equal(http.StatusConflict, response.Code, "%s: %s", action, response.Body.String())
		assertions.NotContains(response.Body.String(), "observed-previous-policy")
	}
	assertions.Zero(calls, "unpublished runtime must refuse expected-policy requests before callbacks")
	current, err := st.GetVisualGeneration(t.Context(), generation.ID)
	requirements.NoError(err)
	assertions.False(current.Consented)
}

func TestVisualPolicySnapshotAndExactGuardPreserveStoredConsent(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	generation, err := st.EnsureVisualGeneration(t.Context(), store.VisualGenerationSpec{Fingerprint: "initialized-generation", Model: "voyage-multimodal-3.5", Dimension: 1024})
	requirements.NoError(err)
	requirements.NoError(st.ConsentVisualGeneration(t.Context(), generation.ID, "recorded-policy"))
	srv := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: st, Logger: testLogger()})
	policy := VisualRuntimePolicy{GenerationID: generation.ID, GenerationFingerprint: generation.Fingerprint, CurrentPolicyFingerprint: "installed-policy", Provider: "voyage", Model: generation.Model, SourceIDs: []int64{1}, MessageTypes: []string{"email"}, AuthorizedCapabilities: []string{"query_text"}, RetentionPosture: "unspecified", TrainingPosture: "unspecified"}
	calls := 0
	build := func(ctx context.Context, _ operations.PassScope) error {
		if err := CheckVisualOperationGuard(ctx, generation.ID, generation.Fingerprint, "installed-policy"); err != nil {
			return err
		}
		calls++
		return st.ConsentVisualGeneration(ctx, generation.ID, "installed-policy")
	}
	status := func(ctx context.Context, _ bool) (visual.Status, error) {
		current, err := st.GetVisualGeneration(ctx, generation.ID)
		return visual.Status{Generation: current}, err
	}
	srv.SetVisualOperationsWithPolicy(policy, build, build, nil, status, nil)
	policy.SourceIDs[0] = 99
	policy.MessageTypes[0] = "chat"
	policy.AuthorizedCapabilities[0] = "unreviewed"
	read := doGet(srv, "/api/v1/multimodal/policy")
	requirements.Equal(http.StatusOK, read.Code)
	var currentPolicy VisualRuntimePolicy
	requirements.NoError(json.Unmarshal(read.Body.Bytes(), &currentPolicy))
	assertions.Equal([]int64{1}, currentPolicy.SourceIDs)
	assertions.Equal([]string{"email"}, currentPolicy.MessageTypes)
	assertions.Equal([]string{"query_text"}, currentPolicy.AuthorizedCapabilities)
	assertions.Equal("installed-policy", currentPolicy.CurrentPolicyFingerprint)
	assertions.NotContains(read.Body.String(), "recorded-policy")
	assertions.Zero(calls, "a policy read cannot grant consent")
	for _, guard := range []map[string]any{
		{"expected_generation_id": generation.ID, "expected_generation_fingerprint": generation.Fingerprint, "expected_policy_fingerprint": "recorded-policy"},
		{"expected_generation_id": generation.ID + 1, "expected_generation_fingerprint": generation.Fingerprint, "expected_policy_fingerprint": "installed-policy"},
		{"expected_generation_id": generation.ID, "expected_generation_fingerprint": "other-generation", "expected_policy_fingerprint": "installed-policy"},
		{"expected_policy_fingerprint": "installed-policy"},
		{"expected_generation_id": nil, "expected_generation_fingerprint": nil, "expected_policy_fingerprint": nil},
	} {
		guard["consent"] = true
		body, err := json.Marshal(guard)
		requirements.NoError(err)
		response := doRequest(t, srv.Router(), http.MethodPost, "/api/v1/multimodal/build", body, nil)
		assertions.Equal(http.StatusConflict, response.Code, response.Body.String())
	}
	assertions.Zero(calls)
	stored, err := st.GetVisualGeneration(t.Context(), generation.ID)
	requirements.NoError(err)
	assertions.Equal("recorded-policy", stored.ConsentPolicyFingerprint)
	body, err := json.Marshal(map[string]any{"consent": true, "expected_generation_id": generation.ID, "expected_generation_fingerprint": generation.Fingerprint, "expected_policy_fingerprint": "installed-policy"})
	requirements.NoError(err)
	accepted := doRequest(t, srv.Router(), http.MethodPost, "/api/v1/multimodal/build", body, nil)
	requirements.Equal(http.StatusOK, accepted.Code, accepted.Body.String())
	assertions.Equal(1, calls)
	stored, err = st.GetVisualGeneration(t.Context(), generation.ID)
	requirements.NoError(err)
	assertions.Equal("installed-policy", stored.ConsentPolicyFingerprint)
}

type replacingVisualPolicyHistoryReader struct {
	*store.Store

	replace func()
}

func (r replacingVisualPolicyHistoryReader) LaneStatus(ctx context.Context, kind operations.Kind) (operations.LaneHistoryStatus, error) {
	r.replace()
	return r.Store.LaneStatus(ctx, kind)
}

func TestVisualPolicyReplacementBeforeCallbackRefusesWithoutConsent(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	generation, err := st.EnsureVisualGeneration(t.Context(), store.VisualGenerationSpec{Fingerprint: "initialized-generation", Model: "voyage-multimodal-3.5", Dimension: 1024})
	requirements.NoError(err)
	policy := VisualRuntimePolicy{GenerationID: generation.ID, GenerationFingerprint: generation.Fingerprint, CurrentPolicyFingerprint: "installed-policy"}
	var srv *Server
	calls := 0
	build := func(ctx context.Context, _ operations.PassScope) error {
		calls++
		return st.ConsentVisualGeneration(ctx, generation.ID, "installed-policy")
	}
	status := func(ctx context.Context, _ bool) (visual.Status, error) {
		current, err := st.GetVisualGeneration(ctx, generation.ID)
		return visual.Status{Generation: current}, err
	}
	reader := replacingVisualPolicyHistoryReader{Store: st, replace: func() {
		policy.CurrentPolicyFingerprint = "replacement-policy"
		srv.SetVisualOperationsWithPolicy(policy, build, build, nil, status, nil)
	}}
	srv = NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: st, OperationHistoryReader: reader, Logger: testLogger()})
	srv.SetVisualOperationsWithPolicy(policy, build, build, nil, status, nil)
	body, err := json.Marshal(map[string]any{"consent": true, "expected_generation_id": generation.ID, "expected_generation_fingerprint": generation.Fingerprint, "expected_policy_fingerprint": "installed-policy"})
	requirements.NoError(err)
	response := doRequest(t, srv.Router(), http.MethodPost, "/api/v1/multimodal/build", body, nil)
	assertions.Equal(http.StatusConflict, response.Code, response.Body.String())
	assertions.Zero(calls)
	stored, err := st.GetVisualGeneration(t.Context(), generation.ID)
	requirements.NoError(err)
	assertions.False(stored.Consented)
}
