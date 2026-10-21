package mcpevents

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
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
	assert.InDelta(time.Now().Add(5*time.Minute).Unix(), row.NextAttemptAt.Unix(), 2)
}

func TestExpiredOccurrenceAdvancesRetentionFloorBeforeDelivery(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventServiceWithRetention(t, time.Minute)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	messageID := appendReceipt(t, s, f, 1)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_log SET recorded_at=? WHERE message_id=?`), time.Now().UTC().Add(-2*time.Minute).Format("2006-01-02T15:04:05.000000000Z"), messageID)
	require.NoError(err)

	delivery, err := s.st.PrepareMCPDelivery(t.Context(), result.ID, 1, time.Now().UTC(), func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		require.FailNow("an expired occurrence must not build a delivery envelope")
		return nil, nil
	})
	require.NoError(err)
	require.NotNil(delivery)
	require.NotNil(delivery.Control, "an expired occurrence is reported as a gap")
	assert.Equal("gap", delivery.Control.Type)
	var floor, head, retained int64
	require.NoError(f.Store.DB().QueryRow(`SELECT pruned_through_seq FROM mcp_event_clock WHERE singleton=1`).Scan(&floor))
	require.NoError(f.Store.DB().QueryRow(`SELECT head_seq FROM mcp_event_clock WHERE singleton=1`).Scan(&head))
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&retained))
	assert.Equal(int64(2), floor)
	assert.Zero(retained)
	sub, err := f.Store.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	require.NotNil(sub)
	assert.Equal("active", sub.State)
	assert.Empty(sub.StopReason)
	assert.Equal(head, sub.CursorSeq, "the reset moves the cursor to the event clock head")
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

	control, err := s.st.FinishMCPDelivery(t.Context(), result.ID, 1, pending.Event.Seq, now, http.StatusNoContent, time.Time{})
	require.NoError(err)
	require.NotNil(control)
	assert.Equal("gap", control.Type)
	sub, err := f.Store.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	require.NotNil(sub)
	assert.Equal("active", sub.State)
	assert.Equal("gap", sub.LastOutcome, "an expired pending event is never acknowledged")
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
	var received, gaps atomic.Int32
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if assert.NoError(readErr) && strings.Contains(string(body), `"type":"gap"`) {
			gaps.Add(1)
		} else {
			received.Add(1)
		}
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
		return err == nil && row != nil && row.LastOutcome == "gap" && gaps.Load() == 1
	}, 10*time.Second, 10*time.Millisecond, "retention must reset a subscription whose pending occurrence expires during resolution")
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
	assert.Equal("active", row.State)
	assert.Empty(row.StopReason)
	assert.Equal("gap", row.LastOutcome, "an expired occurrence is never acknowledged")
	assert.Zero(row.PendingSeq, "retention pruning clears the expired pending receipt")
	var floor int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT pruned_through_seq FROM mcp_event_clock WHERE singleton=1`).Scan(&floor))
	assert.Equal(int64(1), floor)
}

func TestWorkerTerminalStatusContinuesOrStops(t *testing.T) {
	for _, status := range []int{204, 413} {
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
				return err == nil && row.PendingSeq == 0 && row.CursorSeq == 1
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
			assert.Equal("active", row.State)
			assert.Equal(int64(1), row.CursorSeq)
			if status == 413 {
				assert.Equal(1, row.DeadLetterCount)
			} else {
				assert.Zero(row.DeadLetterCount)
			}
		})
	}
}

func TestRetryDelaySchedule(t *testing.T) {
	assert := Assert.New(t)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		status  int
		header  string
		attempt int
		floor   time.Duration
		ceil    time.Duration
	}{
		{"attempt 1", 500, "", 1, 30 * time.Second, 33 * time.Second},
		{"attempt 2", 500, "", 2, time.Minute, 66 * time.Second},
		{"attempt 3", 500, "", 3, 3 * time.Minute, 198 * time.Second},
		{"attempt 4", 500, "", 4, 8 * time.Minute, 528 * time.Second},
		{"attempt 5 clamps", 500, "", 5, 8 * time.Minute, 528 * time.Second},
		{"malformed Retry-After", 503, "malformed", 2, time.Minute, 66 * time.Second},
		{"Retry-After ignored on 500", 500, "120", 1, 30 * time.Second, 33 * time.Second},
		{"429 Retry-After", 429, "120", 1, 2 * time.Minute, 2 * time.Minute},
		{"503 Retry-After capped", 503, "3600", 1, 5 * time.Minute, 5 * time.Minute},
		{"429 Retry-After capped", 429, "7200", 1, 5 * time.Minute, 5 * time.Minute},
		{"503 Retry-After date", 503, now.Add(time.Minute).Format(http.TimeFormat), 1, time.Minute, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for range 32 {
				got := retryDelay(tc.status, tc.header, tc.attempt, now)
				assert.GreaterOrEqual(got, tc.floor)
				assert.LessOrEqual(got, tc.ceil)
			}
		})
	}
}

func TestGoneDropsOneDeliveryAndKeepsSubscription(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	appendReceipt(t, s, f, 1)
	_, lastSeq := appendReceiptWithEventSequence(t, s, f, 2)
	var sends atomic.Int32
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		if sends.Add(1) == 1 {
			rw.WriteHeader(http.StatusGone)
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
		return err == nil && row.CursorSeq == lastSeq && row.PendingSeq == 0
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
	assert.Equal("active", row.State)
	assert.Equal("acknowledged", row.LastOutcome)
	assert.Equal(1, row.DeadLetterCount)
	assert.Equal(int32(2), sends.Load())
	var attempts int
	var class string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT attempts,last_status_class FROM mcp_event_dead_letters WHERE subscription_id=? AND seq=1`), result.ID).Scan(&attempts, &class))
	assert.Equal(1, attempts)
	assert.Equal("http_4xx", class)
}

// controlPost is one callback request the receiver saw.
type controlPost struct {
	Header http.Header
	Body   []byte
}

// recordingReceiver answers verification challenges and records every other
// request. answer picks the status for a non-verification request, given its
// zero-based position.
type recordingReceiver struct {
	mu    sync.Mutex
	posts []controlPost
}

func installRecordingReceiver(t *testing.T, s *Service, answer func(index int, body []byte) int) *recordingReceiver {
	t.Helper()
	r := &recordingReceiver{}
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if !Assert.NoError(t, err) {
			return
		}
		var head struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
		}
		if !Assert.NoError(t, json.Unmarshal(body, &head)) {
			return
		}
		if head.Type == "verification" {
			Assert.NoError(t, json.NewEncoder(rw).Encode(map[string]any{"challenge": head.Challenge}))
			return
		}
		r.mu.Lock()
		index := len(r.posts)
		r.posts = append(r.posts, controlPost{Header: req.Header.Clone(), Body: body})
		r.mu.Unlock()
		status := http.StatusNoContent
		if answer != nil {
			status = answer(index, body)
		}
		rw.WriteHeader(status)
	})
	return r
}

func (r *recordingReceiver) snapshot() []controlPost {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]controlPost(nil), r.posts...)
}

func (r *recordingReceiver) waitFor(t *testing.T, count int, message string) []controlPost {
	t.Helper()
	Require.Eventually(t, func() bool { return len(r.snapshot()) >= count }, 10*time.Second, 10*time.Millisecond, message)
	return r.snapshot()
}

// runServiceUntilCleanup starts the worker supervisor and joins it when the
// test ends.
func runServiceUntilCleanup(t *testing.T, s *Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			Assert.NoError(t, err)
		case <-time.After(10 * time.Second):
			Assert.Fail(t, "MCP Events supervisor did not stop")
		}
	})
}

// assertSignedControl checks a control post's identity headers and signature.
func assertSignedControl(t *testing.T, subscriptionID, kind string, post controlPost) {
	t.Helper()
	assert := Assert.New(t)
	assert.True(strings.HasPrefix(post.Header.Get("Webhook-Id"), "msg_"+kind+"_"), "webhook id %q", post.Header.Get("Webhook-Id"))
	assert.Equal(subscriptionID, post.Header.Get("X-Mcp-Subscription-Id"))
	assert.Equal(webhookSignature(make([]byte, 32), post.Header.Get("Webhook-Id"), post.Header.Get("Webhook-Timestamp"), post.Body), post.Header.Get("Webhook-Signature"))
}

func agePrunableReceipt(t *testing.T, s *Service, f *storetest.Fixture, seq int64) {
	t.Helper()
	_, eventSeq := appendReceiptWithEventSequence(t, s, f, seq)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_log SET recorded_at=? WHERE seq=?`), time.Now().UTC().Add(-2*time.Minute).Format("2006-01-02T15:04:05.000000000Z"), eventSeq)
	Require.NoError(t, err)
}

func TestRetentionGapResetsAndSendsGapEnvelope(t *testing.T) {
	cases := []struct {
		name   string
		breach func(t *testing.T, s *Service, f *storetest.Fixture)
	}{
		{"retention", func(t *testing.T, s *Service, f *storetest.Fixture) {
			t.Helper()
			agePrunableReceipt(t, s, f, 1)
			Require.NoError(t, s.st.PruneMCPEvents(t.Context(), time.Now().UTC(), time.Minute))
		}},
		{"capture_gap", func(t *testing.T, s *Service, f *storetest.Fixture) {
			t.Helper()
			appendReceipt(t, s, f, 1)
			// Turning capture off and on is the production path that starts a
			// new capture epoch.
			cfg := StoreCaptureConfig(s.opts)
			cfg.Enabled = false
			_, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
			Require.NoError(t, err)
			cfg.Enabled = true
			_, err = f.Store.ConfigureMCPEvents(t.Context(), cfg)
			Require.NoError(t, err)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			s, f, req := eventServiceWithRetention(t, time.Minute)
			result, err := s.Subscribe(t.Context(), s.principal, req)
			require.NoError(err)
			before, err := s.st.GetMCPSubscription(t.Context(), result.ID)
			require.NoError(err)
			receiver := installRecordingReceiver(t, s, nil)
			tc.breach(t, s, f)
			runServiceUntilCleanup(t, s)

			posts := receiver.waitFor(t, 1, "the gap envelope must be sent")
			var gap struct {
				Type   string `json:"type"`
				Cursor string `json:"cursor"`
			}
			require.NoError(json.Unmarshal(posts[0].Body, &gap))
			assert.Equal("gap", gap.Type)
			assertSignedControl(t, result.ID, "gap", posts[0])
			var epoch, head int64
			require.NoError(f.Store.DB().QueryRow(`SELECT capture_epoch,head_seq FROM mcp_event_clock WHERE singleton=1`).Scan(&epoch, &head))
			gotEpoch, gotSeq, err := decodeCursor(s.key, result.ID, gap.Cursor)
			require.NoError(err)
			assert.Equal(epoch, gotEpoch)
			assert.Equal(head, gotSeq)

			row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
			require.NoError(err)
			assert.Equal("active", row.State)
			assert.Empty(row.StopReason)
			assert.Greater(row.Generation, before.Generation)
			assert.Equal(epoch, row.CursorEpoch)

			// The same worker keeps serving the new generation.
			s.workersMu.Lock()
			worker := s.workers[result.ID]
			s.workersMu.Unlock()
			require.NotNil(worker)
			appendReceipt(t, s, f, 2)
			posts = receiver.waitFor(t, 2, "the occurrence after the gap must be delivered")
			var delivered Envelope
			require.NoError(json.Unmarshal(posts[1].Body, &delivered))
			assert.NotEmpty(delivered.EventID)
			assert.Len(posts, 2, "exactly one gap envelope precedes the occurrence")
			s.workersMu.Lock()
			assert.Same(worker, s.workers[result.ID], "a gap reset must not replace the worker")
			assert.Equal(row.Generation, worker.generation)
			s.workersMu.Unlock()
		})
	}
}

func TestRefreshAfterGapReset(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventServiceWithRetention(t, time.Minute)
	first, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	receiver := installRecordingReceiver(t, s, nil)
	agePrunableReceipt(t, s, f, 1)
	require.NoError(s.st.PruneMCPEvents(t.Context(), time.Now().UTC(), time.Minute))
	runServiceUntilCleanup(t, s)
	receiver.waitFor(t, 1, "the gap envelope must be sent")

	refreshed, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	assert.False(refreshed.Truncated, "the reset already moved the cursor to the head")
	assert.Len(receiver.snapshot(), 1, "a refresh sends no second gap envelope")

	stale := first.Cursor
	req.Cursor = &stale
	replayed, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	assert.True(replayed.Truncated, "the pre-gap cursor is below the retention floor")
}

func TestGapEnvelopeFailureIsNotRetried(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventServiceWithRetention(t, time.Minute)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	receiver := installRecordingReceiver(t, s, func(index int, _ []byte) int {
		if index == 0 {
			return http.StatusInternalServerError
		}
		return http.StatusNoContent
	})
	agePrunableReceipt(t, s, f, 1)
	require.NoError(s.st.PruneMCPEvents(t.Context(), time.Now().UTC(), time.Minute))
	runServiceUntilCleanup(t, s)
	receiver.waitFor(t, 1, "the gap envelope must be sent")
	_, lastSeq := appendReceiptWithEventSequence(t, s, f, 2)
	posts := receiver.waitFor(t, 2, "the occurrence must follow a failed gap envelope")
	assert.Contains(string(posts[0].Body), `"type":"gap"`)
	assert.Contains(string(posts[1].Body), `"eventId"`)
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
		return err == nil && row.CursorSeq == lastSeq && row.PendingSeq == 0
	}, 10*time.Second, 10*time.Millisecond)
	assert.Len(receiver.snapshot(), 2, "the gap envelope is sent once")
}

func TestLiveRefreshWithCurrentCursorIsNoOp(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	first, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	receiver := installRecordingReceiver(t, s, func(int, []byte) int { return http.StatusInternalServerError })
	runServiceUntilCleanup(t, s)
	appendReceipt(t, s, f, 1)
	receiver.waitFor(t, 1, "the occurrence must be attempted")
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), first.ID)
		return err == nil && row.PendingSeq == 1 && row.AttemptCount == 1 && !row.NextAttemptAt.IsZero()
	}, 10*time.Second, 10*time.Millisecond)
	_, err = f.Store.DB().Exec(`UPDATE mcp_event_subscriptions SET attempt_count=2`)
	require.NoError(err)
	before, err := s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	require.Equal(2, before.AttemptCount)

	req.Cursor = &first.Cursor
	refreshed, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	assert.False(refreshed.Truncated)
	assert.Equal(first.Cursor, refreshed.Cursor, "the result carries the server's settled cursor")
	after, err := s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	assert.Equal(before.Generation, after.Generation)
	assert.Equal(2, after.AttemptCount)
	assert.Equal(int64(1), after.PendingSeq)
	assert.True(before.NextAttemptAt.Equal(after.NextAttemptAt))
	assert.Never(func() bool { return len(receiver.snapshot()) > 1 }, 300*time.Millisecond, 20*time.Millisecond, "the refresh must not cause another POST")
}

func TestLiveRefreshWithOlderCursorInCoverageIsNoOp(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	first, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	epoch, _, err := decodeCursor(s.key, first.ID, first.Cursor)
	require.NoError(err)
	receiver := installRecordingReceiver(t, s, nil)
	runServiceUntilCleanup(t, s)
	_, firstSeq := appendReceiptWithEventSequence(t, s, f, 1)
	_, lastSeq := appendReceiptWithEventSequence(t, s, f, 2)
	receiver.waitFor(t, 2, "both occurrences must be delivered")
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), first.ID)
		return err == nil && row.CursorSeq == lastSeq && row.PendingSeq == 0
	}, 10*time.Second, 10*time.Millisecond)
	before, err := s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)

	behind := encodeCursor(s.key, first.ID, epoch, firstSeq)
	req.Cursor = &behind
	refreshed, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	assert.False(refreshed.Truncated)
	assert.Equal(encodeCursor(s.key, first.ID, epoch, lastSeq), refreshed.Cursor)
	after, err := s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	assert.Equal(before.Generation, after.Generation)
	assert.Never(func() bool { return len(receiver.snapshot()) > 2 }, 300*time.Millisecond, 20*time.Millisecond, "events behind the settled cursor are not delivered again")
}
