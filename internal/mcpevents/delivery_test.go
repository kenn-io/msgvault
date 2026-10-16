package mcpevents

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
)

func TestPendingEnvelopeSurvivesRestartAndRetryAfter(t *testing.T) {
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
	restarted, err := New(t.Context(), f.Store, s.opts)
	require.NoError(err)
	got := make(chan []byte, 1)
	restarted.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !Assert.NoError(t, err) {
			return
		}
		got <- body
		rw.Header().Set("Retry-After", "3600")
		rw.WriteHeader(http.StatusServiceUnavailable)
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- restarted.Run(ctx) }()
	select {
	case body := <-got:
		assert.Equal(prepared.Subscription.PendingEnvelope, body)
	case <-time.After(10 * time.Second):
		require.FailNow("persisted pending delivery not sent")
	}
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
		return err == nil && row.LastOutcome == "retry_http_5xx"
	}, 10*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(err)
	case <-time.After(10 * time.Second):
		require.FailNow("restart worker did not stop")
	}
	row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	assert.Equal(int64(0), row.CursorSeq)
	assert.Equal(2, row.AttemptCount)
	assert.Equal(prepared.Subscription.PendingEnvelope, row.PendingEnvelope)
	assert.InDelta(time.Now().Add(time.Hour).Unix(), row.NextAttemptAt.Unix(), 2)
}

func TestExpiredOccurrenceAdvancesRetentionFloorBeforeDelivery(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventServiceWithRetention(t, time.Minute)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	appendReceipt(t, s, f, 1)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_log SET recorded_at=? WHERE seq=1`), time.Now().UTC().Add(-2*time.Minute).Format("2006-01-02T15:04:05.000000000Z"))
	require.NoError(err)

	delivery, err := s.st.PrepareMCPDelivery(t.Context(), result.ID, 1, time.Now().UTC(), func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		require.FailNow("an expired occurrence must not build a delivery envelope")
		return nil, nil
	})
	require.NoError(err)
	assert.Nil(delivery)
	var floor, retained int64
	require.NoError(f.Store.DB().QueryRow(`SELECT pruned_through_seq FROM mcp_event_clock WHERE singleton=1`).Scan(&floor))
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&retained))
	assert.Equal(int64(1), floor)
	assert.Zero(retained)
	sub, err := f.Store.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	require.NotNil(sub)
	assert.Equal("stopped", sub.State)
	assert.Equal("retention", sub.StopReason)
}

func TestExpiredOccurrenceCannotAdvanceCursorAfterReceiverAck(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventServiceWithRetention(t, time.Minute)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	appendReceipt(t, s, f, 1)
	now := time.Now().UTC()
	pending, err := s.st.PrepareMCPDelivery(t.Context(), result.ID, 1, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"synthetic-event"}`), nil
	})
	require.NoError(err)
	require.NotNil(pending)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_log SET recorded_at=? WHERE seq=1`), now.Add(-2*time.Minute).Format("2006-01-02T15:04:05.000000000Z"))
	require.NoError(err)

	require.NoError(s.st.FinishMCPDelivery(t.Context(), result.ID, 1, pending.Event.Seq, now, http.StatusNoContent, time.Time{}))
	sub, err := f.Store.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	require.NotNil(sub)
	assert.Equal("stopped", sub.State)
	assert.Equal("retention", sub.StopReason)
	assert.Zero(sub.CursorSeq, "an expired pending event must not advance the durable cursor")
	assert.Zero(sub.PendingSeq)
	var floor int64
	require.NoError(f.Store.DB().QueryRow(`SELECT pruned_through_seq FROM mcp_event_clock WHERE singleton=1`).Scan(&floor))
	assert.Equal(int64(1), floor)
}

func TestExpiredOccurrenceDuringCallbackResolutionIsNotDelivered(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventServiceWithRetention(t, time.Minute)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	var received atomic.Int32
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		received.Add(1)
		rw.WriteHeader(http.StatusNoContent)
	})
	aged := atomic.Bool{}
	resolutionErr := make(chan error, 1)
	s.webhook.resolve = func(ctx context.Context, network, host string) ([]netip.Addr, error) {
		if aged.CompareAndSwap(false, true) {
			_, ageErr := f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE mcp_event_log SET recorded_at=? WHERE seq=1`), time.Now().UTC().Add(-2*time.Minute).Format("2006-01-02T15:04:05.000000000Z"))
			resolutionErr <- ageErr
			if ageErr != nil {
				return nil, ageErr
			}
		}
		return []netip.Addr{netip.MustParseAddr("203.0.113.7")}, nil
	}
	appendReceipt(t, s, f, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(err)
		case <-time.After(10 * time.Second):
			require.FailNow("MCP Events worker did not stop after cancellation")
		}
	})
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
		return err == nil && row != nil && row.State == "stopped"
	}, 10*time.Second, 10*time.Millisecond, "retention must stop a subscription whose pending occurrence expires during resolution")
	select {
	case err := <-resolutionErr:
		require.NoError(err)
	case <-time.After(10 * time.Second):
		require.FailNow("callback resolution did not age the pending occurrence")
	}
	assert.Zero(received.Load(), "the callback guard must reject an expired occurrence before dialing")
	row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	require.NotNil(row)
	assert.Equal("retention", row.StopReason)
	assert.Zero(row.CursorSeq, "an expired occurrence cannot advance the receipt cursor")
	assert.Zero(row.PendingSeq, "retention pruning clears the expired pending receipt")
	var floor int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT pruned_through_seq FROM mcp_event_clock WHERE singleton=1`).Scan(&floor))
	assert.Equal(int64(1), floor)
}

func TestWorkerTerminalStatusContinuesOrStops(t *testing.T) {
	for _, status := range []int{204, 410, 413} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			s, f, req := eventService(t)
			result, err := s.Subscribe(t.Context(), s.principal, req)
			require.NoError(err)
			s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) { rw.WriteHeader(status) })
			appendReceipt(t, s, f, 1)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- s.Run(ctx) }()
			require.Eventually(func() bool {
				row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
				return err == nil && row.PendingSeq == 0 && (row.CursorSeq == 1 || row.State == "gone")
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
			if status == 410 {
				assert.Equal("gone", row.State)
			} else {
				assert.Equal("active", row.State)
				assert.Equal(int64(1), row.CursorSeq)
			}
			if status == 413 {
				assert.Equal(1, row.DeadLetterCount)
			} else {
				assert.Zero(row.DeadLetterCount)
			}
		})
	}
}

func TestRetryDelayHonorsOnlyRetryableStatusHeaders(t *testing.T) {
	assert := Assert.New(t)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	assert.Equal(time.Hour, retryDelay(429, "7200", 1, now))
	assert.Equal(120*time.Second, retryDelay(503, "120", 1, now))
	assert.Equal(time.Minute, retryDelay(503, now.Add(time.Minute).Format(http.TimeFormat), 1, now))
	for _, tc := range []struct {
		status      int
		header      string
		attempt     int
		floor, ceil time.Duration
	}{
		{500, "120", 1, time.Second, 1250 * time.Millisecond},
		{503, "malformed", 2, 2 * time.Second, 2500 * time.Millisecond},
		{0, "", 12, 15 * time.Minute, 1125 * time.Second},
	} {
		for range 32 {
			got := retryDelay(tc.status, tc.header, tc.attempt, now)
			assert.GreaterOrEqual(got, tc.floor)
			assert.LessOrEqual(got, tc.ceil)
		}
	}
}
