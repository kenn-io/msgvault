package mcpevents

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
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
