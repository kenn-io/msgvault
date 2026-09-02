package store_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

// overflowUsageProvider reports schema-valid candidates with overflow-scale
// token usage: trustworthy metadata, unaccountable usage numbers. The worker
// must reject such a response without recording a completed usage record, so
// failure finalization conservatively charges the reservation instead of
// failing on unaccountable actuals and stranding the attempt and lease.
type overflowUsageProvider struct{ t *testing.T }

func (p *overflowUsageProvider) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	body, err := json.Marshal(map[string]any{
		"model":   "parity-model-v1",
		"choices": []any{map[string]any{"message": map[string]any{"content": `{"claims":[]}`}}},
		"usage":   map[string]any{"prompt_tokens": 9_000_000_000_000_000, "completion_tokens": 4},
	})
	assert.NoError(p.t, err)
	w.Header().Set("X-Request-ID", "overflow-request-1")
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(body)
	assert.NoError(p.t, err)
}

func TestPersonSweepWorkerUnaccountableUsageFinalizesWithoutPendingLease(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newProductionPersonSweepParityFixture(t)
	provider := httptest.NewServer(&overflowUsageProvider{t: t})
	defer provider.Close()
	target, err := url.Parse(provider.URL)
	require.NoError(err)
	httpClient := provider.Client()
	httpClient.Transport = parityRewriteTransport{base: httpClient.Transport, target: target}
	registry, err := peoplesweep.NewDriverRegistry(httpClient, nil, nil)
	require.NoError(err)
	runner, err := peoplesweep.NewRunner(f.config, f.store, registry,
		peoplesweep.NewCredentialResolver(nil, func(string) (string, bool) {
			return "synthetic-parity-key", true
		}))
	require.NoError(err)
	f.worker.Runner = parityRedactedRunner{StructuredRunner: runner}

	lease := parityClaim(t, f.store, "overflow-worker")
	const runID = "run-parity-overflow"
	_, err = f.store.StartPersonSweepRun(t.Context(), peoplesweep.StartRun{ID: runID,
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		ProgramFingerprint:  peoplesweep.ProgramFingerprint(),
		CatalogFingerprint:  parityCatalogFingerprint(t, f),
		ProviderFingerprint: f.profile.Fingerprint, StartedAt: f.now})
	require.NoError(err)
	_, runErr := f.worker.RunPerson(t.Context(), runID, *lease, peoplesweep.RunIncremental)
	require.ErrorIs(runErr, peoplesweep.ErrBudgetOverflow)

	var attemptID, attemptStatus, failureClass string
	var attemptCompletedAt any
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT id, status, failure_class, completed_at FROM person_sweep_attempts
		WHERE run_id = ?`), runID).Scan(&attemptID, &attemptStatus, &failureClass,
		&attemptCompletedAt))
	assert.Equal(string(peoplesweep.AttemptFailed), attemptStatus,
		"failure finalization must terminate the attempt instead of stranding it until lease expiry")
	assert.Equal(string(peoplesweep.FailureBudget), failureClass)
	assert.NotNil(attemptCompletedAt, "the finalized attempt must carry a completion time")

	var leaseOwner string
	var leaseUntil, availableAt any
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT lease_owner, lease_until, available_at FROM person_sweep_work
		WHERE person_id = ?`), f.personID).Scan(&leaseOwner, &leaseUntil, &availableAt))
	assert.Empty(leaseOwner, "the lease must be released for requeueing")
	assert.Nil(leaseUntil, "the lease must not stay pending until expiry")
	assert.NotNil(availableAt)

	var batchStatus, requestID string
	var actualRequests, actualInput, actualOutput, actualCost int64
	var reservedRequests, reservedInput, reservedOutput, reservedCost int64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT status, provider_request_id,
		       actual_requests, actual_input_tokens, actual_output_tokens,
		       actual_cost_micro_usd,
		       reserved_requests, reserved_input_tokens, reserved_output_tokens,
		       reserved_cost_micro_usd
		FROM person_sweep_batches
		WHERE attempt_id = ? AND batch_ordinal = 0 AND call_ordinal = 0`), attemptID,
	).Scan(&batchStatus, &requestID, &actualRequests, &actualInput, &actualOutput,
		&actualCost, &reservedRequests, &reservedInput, &reservedOutput, &reservedCost))
	assert.Equal("failed", batchStatus)
	assert.Empty(requestID,
		"the rejected call must not record provider identity in durable history")
	assert.Equal(reservedRequests, actualRequests,
		"conservative accounting must charge the reserved request")
	assert.Equal(reservedInput, actualInput,
		"conservative accounting must charge the reserved input tokens")
	assert.Equal(reservedOutput, actualOutput,
		"conservative accounting must charge the reserved output tokens")
	assert.Equal(reservedCost, actualCost,
		"conservative accounting must charge the reserved cost")

	var attemptRequests, attemptInput, attemptOutput int64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT request_count, input_tokens, output_tokens
		FROM person_sweep_attempts WHERE id = ?`), attemptID,
	).Scan(&attemptRequests, &attemptInput, &attemptOutput))
	assert.Equal(reservedRequests, attemptRequests)
	assert.Equal(reservedInput, attemptInput)
	assert.Equal(reservedOutput, attemptOutput)

	reclaimed, err := f.store.ClaimPersonSweep(t.Context(), peoplesweep.ClaimRequest{
		WorkerID: "overflow-reclaimer", LeaseDuration: time.Hour,
		AvailableAt: f.now.Add(time.Minute)})
	require.NoError(err)
	require.NotNil(reclaimed, "a finalized failure must leave the person reclaimable")
}
