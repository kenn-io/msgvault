package mcpevents

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
)

// logCapture records default-logger output so tests can wait for a specific
// record and inspect what the record exposes.
type logCapture struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	json    slog.Handler
	records []slog.Record
	changed chan struct{}
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{changed: make(chan struct{}, 1)}
	c.json = slog.NewJSONHandler(&c.buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	previous := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return c
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *logCapture) WithGroup(string) slog.Handler            { return c }

func (c *logCapture) Handle(ctx context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r.Clone())
	select {
	case c.changed <- struct{}{}:
	default:
	}
	if err := c.json.Handle(ctx, r); err != nil {
		return fmt.Errorf("write captured log record: %w", err)
	}
	return nil
}

func (c *logCapture) find(message string) (map[string]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r.Message != message {
			continue
		}
		attrs := make(map[string]string)
		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.String()
			return true
		})
		return attrs, true
	}
	return nil, false
}

func (c *logCapture) wait(ctx context.Context, message string) (map[string]string, bool) {
	for {
		if attrs, ok := c.find(message); ok {
			return attrs, true
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-c.changed:
		}
	}
}

func (c *logCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func startWorker(t *testing.T, s *Service, id string, generation int64) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runWorker(ctx, id, &worker{generation: generation, wake: make(chan struct{}, 1), deliveryWake: make(chan struct{}, 1)})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			Require.FailNow(t, "worker did not stop after cancellation")
		}
	})
}

func TestRunRetriesTransientStorageFailures(t *testing.T) {
	require := Require.New(t)
	s, f, req := eventService(t)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	received := make(chan struct{}, 1)
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		Assert.NoError(t, err)
		select {
		case received <- struct{}{}:
		default:
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	var failing atomic.Bool
	failing.Store(true)
	failures := make(chan struct{}, 64)
	s.opts.WithOperation = func(ctx context.Context, fn func() error) error {
		if failing.Load() {
			select {
			case failures <- struct{}{}:
			default:
			}
			return errors.New("synthetic storage outage")
		}
		return fn()
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
			require.FailNow("Run did not stop after cancellation")
		}
	})
	// Startup maintenance and at least two reconciliation passes hit the outage.
	for range 4 {
		select {
		case <-failures:
		case err := <-done:
			require.FailNow("Run returned during a transient storage outage", "error: %v", err)
		case <-time.After(10 * time.Second):
			require.FailNow("Run did not retry Store operations during the outage")
		}
	}
	failing.Store(false)
	select {
	case <-received:
	case err := <-done:
		require.FailNow("Run returned after a transient storage outage", "error: %v", err)
	case <-time.After(10 * time.Second):
		require.FailNow("delivery did not resume after storage recovered")
	}
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
		return err == nil && row != nil && row.CursorSeq == 1 && row.PendingSeq == 0
	}, 10*time.Second, 10*time.Millisecond)
}

func TestStartupMaintenanceYieldsToCommittedOccurrence(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	received := make(chan struct{}, 1)
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		Assert.NoError(t, err)
		select {
		case received <- struct{}{}:
		default:
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	gate := newMCPEventsTestGate()
	s.opts.WithOperation = func(ctx context.Context, fn func() error) error {
		return gate.operation(ctx, false, fn)
	}
	s.opts.WithDeliveryOperation = func(ctx context.Context, fn func() error) error {
		return gate.operation(ctx, true, fn)
	}
	releaseScheduled, held := gate.hold(t.Context())
	require.True(held)
	t.Cleanup(releaseScheduled)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		releaseScheduled()
		cancel()
		select {
		case err := <-done:
			require.NoError(err)
		case <-time.After(10 * time.Second):
			require.FailNow("Run did not stop after cancellation")
		}
	})
	queueCtx, queueCancel := context.WithTimeout(t.Context(), 10*time.Second)
	queued := gate.waitForQueuedOperations(queueCtx, 1)
	queueCancel()
	require.True(queued, "startup maintenance should queue behind scheduled work")
	s.workersMu.Lock()
	assert.Empty(s.workers, "the subscription has no worker while maintenance waits")
	s.workersMu.Unlock()

	live := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: time.Now().UTC()})
	_, err = live.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().Build()})
	require.NoError(err)
	waitCtx, waitCancel := context.WithTimeout(t.Context(), 10*time.Second)
	promoted := gate.waitForRequestWaiter(waitCtx)
	waitCancel()
	require.True(promoted, "a committed occurrence must promote queued maintenance")
	releaseScheduled()
	select {
	case <-received:
	case <-time.After(10 * time.Second):
		require.FailNow("the committed occurrence was not delivered after maintenance yielded")
	}
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
		return err == nil && row != nil && row.CursorSeq > 0 && row.PendingSeq == 0
	}, 10*time.Second, 10*time.Millisecond)
}

func TestOrphanedDialDoesNotTouchWorkerState(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	first, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	req.Delivery.URL = "https://receiver.example.net/second"
	second, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	delivered := make(chan string, 2)
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		Assert.NoError(t, err)
		delivered <- r.Header.Get("X-Mcp-Subscription-Id")
		rw.WriteHeader(http.StatusNoContent)
	})
	// The first lookup stalls. The other worker's connection is handed to the
	// stalled request, so its dial outlives the request that started it.
	releaseLookup := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseLookup) }) }
	t.Cleanup(release)
	var lookups atomic.Int32
	s.webhook.resolve = func(context.Context, string, string) ([]netip.Addr, error) {
		if lookups.Add(1) == 1 {
			<-releaseLookup
		}
		return []netip.Addr{netip.MustParseAddr("203.0.113.7")}, nil
	}
	dials := make(chan error, 4)
	dial := s.webhook.transport.DialContext
	s.webhook.transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		dials <- err
		return conn, err
	}
	appendReceipt(t, s, f, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		release()
		cancel()
		select {
		case err := <-done:
			require.NoError(err)
		case <-time.After(10 * time.Second):
			require.FailNow("Run did not stop after cancellation")
		}
	})
	got := make(map[string]bool)
	for range 2 {
		select {
		case id := <-delivered:
			got[id] = true
		case <-time.After(10 * time.Second):
			require.FailNow("both subscriptions should deliver over the shared connection")
		}
	}
	assert.Equal(map[string]bool{first.ID: true, second.ID: true}, got)
	// Resume the stalled lookup while the workers record receipts. Its dial
	// guard runs on a transport goroutine; under -race, any access to worker
	// state from that goroutine fails the test.
	release()
	for range 2 {
		select {
		case <-dials:
		case <-time.After(10 * time.Second):
			require.FailNow("dial did not finish")
		}
	}
	require.Eventually(func() bool {
		for _, id := range []string{first.ID, second.ID} {
			row, err := s.st.GetMCPSubscription(t.Context(), id)
			if err != nil || row == nil || row.CursorSeq != 1 {
				return false
			}
		}
		return true
	}, 10*time.Second, 10*time.Millisecond)
}

func TestRefusedDialDoesNotCountUnsentAttempt(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	before, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	require.NotNil(before)
	type observed struct {
		attempts int
		outcome  string
	}
	received := make(chan observed, 2)
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		Assert.NoError(t, err)
		row, err := s.st.GetMCPSubscription(r.Context(), result.ID)
		if Assert.NoError(t, err) && Assert.NotNil(t, row) {
			received <- observed{attempts: row.AttemptCount, outcome: row.LastOutcome}
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	resolve := s.webhook.resolve
	var resolved, refuse atomic.Bool
	refuse.Store(true)
	s.webhook.resolve = func(ctx context.Context, network, host string) ([]netip.Addr, error) {
		resolved.Store(true)
		return resolve(ctx, network, host)
	}
	refused := make(chan struct{})
	s.opts.WithOperation = func(ctx context.Context, fn func() error) error {
		// The first Store operation after callback resolution is the dial guard.
		if resolved.CompareAndSwap(true, false) && refuse.CompareAndSwap(true, false) {
			close(refused)
			return errors.New("synthetic transient gate failure")
		}
		return fn()
	}
	appendReceipt(t, s, f, 1)
	startWorker(t, s, result.ID, before.Generation)
	select {
	case <-refused:
	case <-time.After(10 * time.Second):
		require.FailNow("dial guard did not hit the injected failure")
	}
	var got observed
	select {
	case got = <-received:
	case <-time.After(10 * time.Second):
		require.FailNow("worker did not send after the dial guard recovered")
	}
	assert.Equal(1, got.attempts, "an attempt refused before dialing must not be counted again")
	assert.Empty(got.outcome, "an unsent attempt must not be recorded as a transport failure")
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
		return err == nil && row != nil && row.CursorSeq == 1 && row.PendingSeq == 0
	}, 10*time.Second, 10*time.Millisecond)
	select {
	case duplicate := <-received:
		assert.Fail("worker sent the occurrence twice", "%+v", duplicate)
	default:
	}
}

func TestUndecryptableSecretParksWorker(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	before, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	require.NotNil(before)
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		assert.Fail("an undecryptable secret must not sign a delivery")
		rw.WriteHeader(http.StatusNoContent)
	})
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_subscriptions SET secret_enc=? WHERE id=?`), []byte("synthetic-corrupt-secret"), result.ID)
	require.NoError(err)
	logs := captureLogs(t)
	appendReceipt(t, s, f, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runWorker(ctx, result.ID, &worker{generation: before.Generation, wake: make(chan struct{}, 1), deliveryWake: make(chan struct{}, 1)})
	}()
	t.Cleanup(cancel)
	waitCtx, waitCancel := context.WithTimeout(t.Context(), 10*time.Second)
	attrs, logged := logs.wait(waitCtx, "MCP Events delivery stopped")
	waitCancel()
	require.True(logged, "an undecryptable secret must be reported")
	assert.Equal("events_key_unavailable", attrs["reason"])
	select {
	case <-done:
		assert.Fail("the worker exited at an unchanged generation, so reconciliation would never replace it")
	default:
	}
	row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	require.NotNil(row)
	assert.Equal(1, row.AttemptCount, "a parked worker must not consume more attempts")
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		require.FailNow("parked worker did not stop after cancellation")
	}
}

func TestDeliveryFailureLogsSafeReason(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	before, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	require.NotNil(before)
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusNoContent)
	})
	// Without the receiver's test certificate, verification fails.
	s.webhook.transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	logs := captureLogs(t)
	appendReceipt(t, s, f, 1)
	startWorker(t, s, result.ID, before.Generation)
	waitCtx, waitCancel := context.WithTimeout(t.Context(), 10*time.Second)
	attrs, logged := logs.wait(waitCtx, "MCP Events delivery failed")
	waitCancel()
	require.True(logged, "a failed delivery must be logged: %s", logs.text())
	assert.Equal("tls_error", attrs["reason"])
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
		return err == nil && row != nil && row.LastOutcome == "retry_transport_error"
	}, 10*time.Second, 10*time.Millisecond)
	assert.NotContains(logs.text(), "receiver.example.net", "logs must not include callback destinations")
}

func TestAckReceiptRetryDoesNotWaitForFailureBackoff(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	before, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	require.NotNil(before)
	appendReceipt(t, s, f, 1)
	// Eight failed attempts make the next failure backoff several minutes.
	now := time.Now().UTC()
	for range 8 {
		pending, err := s.st.PrepareMCPDelivery(t.Context(), result.ID, before.Generation, now, func(sub store.MCPSubscription, event store.MCPEvent) ([]byte, error) {
			return json.Marshal(s.envelope(sub, event))
		})
		require.NoError(err)
		require.NotNil(pending)
		require.NoError(s.st.FinishMCPDelivery(t.Context(), result.ID, before.Generation, 1, now, http.StatusServiceUnavailable, now))
	}
	var receiverAcked atomic.Bool
	deliveries := make(chan struct{}, 2)
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		Assert.NoError(t, err)
		deliveries <- struct{}{}
		receiverAcked.Store(true)
		rw.WriteHeader(http.StatusNoContent)
	})
	var postAckOperations atomic.Int32
	finishFailed := make(chan struct{})
	s.opts.WithOperation = func(ctx context.Context, fn func() error) error {
		// After the ACK, the worker rechecks authorization and then records the receipt.
		if receiverAcked.Load() && postAckOperations.Add(1) == 2 {
			close(finishFailed)
			return errors.New("synthetic transient FinishMCPDelivery gate failure")
		}
		return fn()
	}
	startWorker(t, s, result.ID, before.Generation)
	select {
	case <-finishFailed:
	case <-time.After(10 * time.Second):
		require.FailNow("FinishMCPDelivery did not hit the injected failure after the ACK")
	}
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
		return err == nil && row != nil && row.CursorSeq == 1 && row.PendingSeq == 0
	}, 10*time.Second, 10*time.Millisecond, "an acknowledged receipt must retry promptly, not after failure backoff")
	row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	assert.Equal("acknowledged", row.LastOutcome)
	assert.Len(deliveries, 1, "retrying the receipt must not resend the occurrence")
}
