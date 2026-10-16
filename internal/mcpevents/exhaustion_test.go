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

func TestTwelfthFailedAttemptDeadLettersAndContinues(t *testing.T) {
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
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_subscriptions SET attempt_count=11 WHERE id=?`), result.ID)
	require.NoError(err)
	appendReceipt(t, s, f, 2)
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
		return err == nil && row.CursorSeq == 2 && row.PendingSeq == 0
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
	assert.Equal(12, attempts)
}
