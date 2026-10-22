package mcpevents

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
)

func TestFifthFailedAttemptDeadLettersAndContinues(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	appendReceipt(t, s, f, 1)
	prepared, err := s.st.PrepareMCPDelivery(t.Context(), result.ID, 1, time.Now(), func(sub store.MCPSubscription, event store.MCPEvent) ([]byte, error) {
		return json.Marshal(s.envelope(sub, event))
	})
	require.NoError(err)
	require.NotNil(prepared)
	// Resume a real persisted pending envelope at the final retry boundary.
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_subscriptions SET attempt_count=4 WHERE id=?`), result.ID)
	require.NoError(err)
	appendReceipt(t, s, f, 3)
	var sends atomic.Int32
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		if sends.Add(1) == 1 {
			rw.Header().Set("Retry-After", "3600")
			rw.WriteHeader(http.StatusServiceUnavailable)
		} else {
			rw.WriteHeader(http.StatusNoContent)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
		return err == nil && row.CursorSeq == 3 && row.PendingSeq == 0
	}, 10*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(err)
	case <-time.After(10 * time.Second):
		require.FailNow("worker did not join")
	}
	row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	assert.Equal(1, row.DeadLetterCount)
	assert.Equal("acknowledged", row.LastOutcome)
	assert.Equal(int32(2), sends.Load())
	var attempts int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT attempts FROM mcp_event_dead_letters WHERE subscription_id=? AND seq=1`), result.ID).Scan(&attempts))
	assert.Equal(store.MCPMaxDeliveryAttempts, attempts)
}

func TestDeliveryDeadLettersAfterFiveAttempts(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	appendReceipt(t, s, f, 1)
	var sends atomic.Int32
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		rw.WriteHeader(http.StatusInternalServerError)
	})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	// Make each scheduled retry due now instead of sleeping through backoff.
	// Rewrite the due time once per failed attempt, only after the worker has
	// scheduled the retry (the count advances while the POST is in flight),
	// so the test does not contend with the worker's own writes.
	var rewritten int
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
		if err != nil {
			return false
		}
		if row.PendingSeq == 0 {
			return row.DeadLetterCount == 1
		}
		if row.AttemptCount > rewritten && row.NextAttemptAt.After(time.Now()) {
			due := time.Now().UTC().Add(-time.Second).Format("2006-01-02T15:04:05.000000000Z")
			res, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_subscriptions SET next_attempt_at=? WHERE id=? AND pending_seq IS NOT NULL AND attempt_count=? AND next_attempt_at>?`), due, result.ID, row.AttemptCount, due)
			if err != nil {
				return false
			}
			if n, _ := res.RowsAffected(); n == 1 {
				rewritten = row.AttemptCount
				s.Wake()
			}
		}
		return false
	}, 20*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(err)
	case <-time.After(10 * time.Second):
		require.FailNow("worker did not join")
	}
	row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	assert.Equal("active", row.State)
	assert.Equal(int64(1), row.CursorSeq)
	assert.Equal(int32(5), sends.Load())
	var attempts int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT attempts FROM mcp_event_dead_letters WHERE subscription_id=? AND seq=1`), result.ID).Scan(&attempts))
	assert.Equal(5, attempts)
}
