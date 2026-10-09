package mcpevents

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"path/filepath"
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

type mcpEventsTestGate struct {
	available        chan struct{}
	requestWaiters   atomic.Int32
	operationWaiters atomic.Int32
	requestChanged   chan struct{}
}

func newMCPEventsTestGate() *mcpEventsTestGate {
	g := &mcpEventsTestGate{available: make(chan struct{}, 1), requestChanged: make(chan struct{}, 1)}
	g.available <- struct{}{}
	return g
}

func (g *mcpEventsTestGate) operation(ctx context.Context, request bool, fn func() error) error {
	if request {
		g.requestWaiters.Add(1)
		select {
		case g.requestChanged <- struct{}{}:
		default:
		}
		defer g.requestWaiters.Add(-1)
	}
	g.operationWaiters.Add(1)
	select {
	case g.requestChanged <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		g.operationWaiters.Add(-1)
		return ctx.Err()
	case <-g.available:
		g.operationWaiters.Add(-1)
	}
	defer func() { g.available <- struct{}{} }()
	return fn()
}

func (g *mcpEventsTestGate) hold(ctx context.Context) (func(), bool) {
	select {
	case <-ctx.Done():
		return func() {}, false
	case <-g.available:
	}
	var once sync.Once
	return func() { once.Do(func() { g.available <- struct{}{} }) }, true
}

func (g *mcpEventsTestGate) hasRequestWaiters() bool {
	return g.requestWaiters.Load() > 0
}

func (g *mcpEventsTestGate) waitForRequestWaiter(ctx context.Context) bool {
	for !g.hasRequestWaiters() {
		select {
		case <-ctx.Done():
			return false
		case <-g.requestChanged:
		}
	}
	return true
}

func (g *mcpEventsTestGate) waitForQueuedOperations(ctx context.Context, count int32) bool {
	for g.operationWaiters.Load() < count {
		select {
		case <-ctx.Done():
			return false
		case <-g.requestChanged:
		}
	}
	return true
}

func eventService(t *testing.T) (*Service, *storetest.Fixture, SubscribeRequest) {
	t.Helper()
	return eventServiceWithRetention(t, 7*24*time.Hour)
}

func eventServiceWithRetention(t *testing.T, retention time.Duration) (*Service, *storetest.Fixture, SubscribeRequest) {
	t.Helper()
	f := storetest.New(t)
	s, err := New(t.Context(), f.Store, Options{Enabled: true, Sources: []string{"gmail", "gcal"}, KeyPath: filepath.Join(t.TempDir(), "key"), OwnerKey: "synthetic-owner", Retention: retention})
	Require.NoError(t, err)
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		var v map[string]any
		if !Assert.NoError(t, json.NewDecoder(r.Body).Decode(&v)) {
			return
		}
		if !Assert.NoError(t, json.NewEncoder(rw).Encode(map[string]any{"challenge": v["challenge"]})) {
			return
		}
	})
	return s, f, SubscribeRequest{Name: messageFamily, Arguments: map[string]any{"conversation_id": strconv.FormatInt(f.ConvID, 10)}, Delivery: Delivery{Mode: "webhook", URL: "https://receiver.example.net/events", Secret: "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 32))}}
}

func eventServiceOptionsWithWebhook(opts Options, webhook *webhookClient) Options {
	opts.LookupIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return webhook.resolve(ctx, "ip", host)
	}
	opts.DialContext = webhook.dial
	opts.TLSConfig = webhook.transport.TLSClientConfig.Clone()
	return opts
}

func TestCommittedOccurrenceYieldsScheduledSyncForDelivery(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	received := make(chan Envelope, 1)
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !Assert.NoError(t, err) {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		var message struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
		}
		if !Assert.NoError(t, json.Unmarshal(body, &message)) {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		if message.Type == "verification" {
			if !Assert.NoError(t, json.NewEncoder(rw).Encode(map[string]string{"challenge": message.Challenge})) {
				rw.WriteHeader(http.StatusInternalServerError)
			}
			return
		}
		var envelope Envelope
		if !Assert.NoError(t, json.Unmarshal(body, &envelope)) {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case received <- envelope:
		default:
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	var wakeCount atomic.Int32
	f.Store.SetMCPEventsWake(func() {
		wakeCount.Add(1)
		s.wakeAfterCommit()
	})
	backfill := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestBackfill, ObservedAt: time.Now().UTC()})
	_, err := backfill.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().Build()})
	require.NoError(err)
	assert.Zero(wakeCount.Load(), "backfill commits without an occurrence must not promote worker delivery")
	subscription, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	initial, err := s.st.GetMCPSubscription(t.Context(), subscription.ID)
	require.NoError(err)
	require.NotNil(initial)

	gate := newMCPEventsTestGate()
	s.opts.WithOperation = func(ctx context.Context, fn func() error) error {
		return gate.operation(ctx, false, fn)
	}
	s.opts.WithDeliveryOperation = func(ctx context.Context, fn func() error) error {
		return gate.operation(ctx, true, fn)
	}
	runCtx, cancel := context.WithCancel(t.Context())
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runDone:
			require.NoError(err)
		case <-time.After(10 * time.Second):
			require.FailNow("MCP Events worker did not stop after cancellation")
		}
	})
	require.Eventually(func() bool {
		s.workersMu.Lock()
		defer s.workersMu.Unlock()
		return len(s.workers) == 1
	}, 10*time.Second, 10*time.Millisecond, "service should reconcile the active subscription")
	assert.False(gate.hasRequestWaiters(), "idle reconciliation must not preempt a scheduled sync")

	releaseSync, held := gate.hold(t.Context())
	require.True(held)
	t.Cleanup(releaseSync)
	live := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: time.Now().UTC()})
	_, err = live.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().Build()})
	require.NoError(err)
	assert.Equal(int32(1), wakeCount.Load(), "a committed live occurrence should wake delivery exactly once")

	waitCtx, waitCancel := context.WithTimeout(t.Context(), 10*time.Second)
	require.True(gate.waitForRequestWaiter(waitCtx), "commit-triggered delivery should queue as yield-aware work")
	waitCancel()
	releaseSync()

	var envelope Envelope
	select {
	case envelope = <-received:
	case <-time.After(10 * time.Second):
		require.FailNow("committed occurrence was not delivered after scheduled sync yielded")
	}
	assert.NotEmpty(envelope.EventID)
	assert.Equal(messageFamily, envelope.Name)
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), subscription.ID)
		return err == nil && row != nil && row.Generation == initial.Generation && row.CursorSeq > initial.CursorSeq && row.PendingSeq == 0
	}, 10*time.Second, 20*time.Millisecond, "delivery receipt should ACK the cursor without renewing the subscription")
}

func TestCoalescedCommittedOccurrencesKeepDeliveryPriority(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	received := make(chan Envelope, 2)
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !Assert.NoError(t, err) {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		var message struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
		}
		if !Assert.NoError(t, json.Unmarshal(body, &message)) {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		if message.Type == "verification" {
			if !Assert.NoError(t, json.NewEncoder(rw).Encode(map[string]string{"challenge": message.Challenge})) {
				rw.WriteHeader(http.StatusInternalServerError)
			}
			return
		}
		var envelope Envelope
		if !Assert.NoError(t, json.Unmarshal(body, &envelope)) {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		received <- envelope
		rw.WriteHeader(http.StatusNoContent)
	})
	subscription, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	initial, err := s.st.GetMCPSubscription(t.Context(), subscription.ID)
	require.NoError(err)
	require.NotNil(initial)

	gate := newMCPEventsTestGate()
	s.opts.WithOperation = func(ctx context.Context, fn func() error) error {
		return gate.operation(ctx, false, fn)
	}
	scheduledAcquired := make(chan struct{})
	releaseScheduled := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseScheduled) }) }
	var scheduleOnce sync.Once
	s.opts.WithDeliveryOperation = func(ctx context.Context, fn func() error) error {
		return gate.operation(ctx, true, func() error {
			if err := fn(); err != nil {
				return err
			}
			row, err := s.st.GetMCPSubscription(ctx, subscription.ID)
			if err != nil {
				return err
			}
			if row != nil && row.CursorSeq >= 1 {
				scheduleOnce.Do(func() {
					go func() {
						_ = gate.operation(ctx, false, func() error {
							close(scheduledAcquired)
							select {
							case <-ctx.Done():
								return ctx.Err()
							case <-releaseScheduled:
								return nil
							}
						})
					}()
					queueCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
					defer cancel()
					if !gate.waitForQueuedOperations(queueCtx, 1) {
						return
					}
				})
			}
			return nil
		})
	}
	workerCtx, cancelWorker := context.WithCancel(t.Context())
	w := &worker{generation: initial.Generation, cancel: cancelWorker, done: make(chan struct{}), wake: make(chan struct{}, 1), deliveryWake: make(chan struct{}, 1)}
	s.workersMu.Lock()
	s.workers[subscription.ID] = w
	s.workersMu.Unlock()
	workerDone := make(chan struct{})
	live := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: time.Now().UTC()})
	for range 2 {
		_, err = live.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().Build()})
		require.NoError(err)
	}
	assert.Len(w.deliveryWake, 1, "two commits before the worker starts must coalesce into one wake")
	go func() {
		defer close(workerDone)
		defer close(w.done)
		s.runWorker(workerCtx, subscription.ID, w)
	}()
	t.Cleanup(func() {
		release()
		cancelWorker()
		select {
		case <-workerDone:
		case <-time.After(10 * time.Second):
			require.FailNow("worker did not stop after cancellation")
		}
		s.workersMu.Lock()
		delete(s.workers, subscription.ID)
		s.workersMu.Unlock()
	})

	var first Envelope
	select {
	case first = <-received:
	case <-time.After(10 * time.Second):
		require.FailNow("first committed occurrence was not delivered")
	}
	select {
	case <-scheduledAcquired:
	case <-time.After(10 * time.Second):
		require.FailNow("scheduled operation did not acquire the gate after the first receipt")
	}
	waitCtx, waitCancel := context.WithTimeout(t.Context(), 10*time.Second)
	requestWaiter := gate.waitForRequestWaiter(waitCtx)
	waitCancel()
	require.True(requestWaiter, "the next committed occurrence must keep its delivery priority while scheduled work competes")
	assert.NotEmpty(first.EventID)
	release()

	var second Envelope
	select {
	case second = <-received:
	case <-time.After(10 * time.Second):
		require.FailNow("second committed occurrence was not delivered")
	}
	assert.NotEmpty(second.EventID)
	assert.NotEqual(first.EventID, second.EventID)
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), subscription.ID)
		return err == nil && row != nil && row.Generation == initial.Generation && row.CursorSeq == 2 && row.PendingSeq == 0
	}, 10*time.Second, 20*time.Millisecond, "both receipts should ACK in sequence without renewing the subscription")
}

func TestCommittedOccurrencePromotesReconciliationBeforeWorkerExists(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	received := make(chan Envelope, 1)
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !Assert.NoError(t, err) {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		var message struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
		}
		if !Assert.NoError(t, json.Unmarshal(body, &message)) {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		if message.Type == "verification" {
			if !Assert.NoError(t, json.NewEncoder(rw).Encode(map[string]string{"challenge": message.Challenge})) {
				rw.WriteHeader(http.StatusInternalServerError)
			}
			return
		}
		var envelope Envelope
		if !Assert.NoError(t, json.Unmarshal(body, &envelope)) {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		received <- envelope
		rw.WriteHeader(http.StatusNoContent)
	})
	subscription, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	initial, err := s.st.GetMCPSubscription(t.Context(), subscription.ID)
	require.NoError(err)
	require.NotNil(initial)

	gate := newMCPEventsTestGate()
	scheduledAcquired := make(chan struct{})
	releaseScheduled := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseScheduled) }) }
	var operationCalls atomic.Int32
	s.opts.WithOperation = func(ctx context.Context, fn func() error) error {
		call := operationCalls.Add(1)
		return gate.operation(ctx, false, func() error {
			if err := fn(); err != nil {
				return err
			}
			if call == 2 {
				go func() {
					_ = gate.operation(ctx, false, func() error {
						close(scheduledAcquired)
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-releaseScheduled:
							return nil
						}
					})
				}()
				queueCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				if !gate.waitForQueuedOperations(queueCtx, 1) {
					return queueCtx.Err()
				}
			}
			return nil
		})
	}
	s.opts.WithDeliveryOperation = func(ctx context.Context, fn func() error) error {
		return gate.operation(ctx, true, fn)
	}
	runCtx, cancelRun := context.WithCancel(t.Context())
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(runCtx) }()
	t.Cleanup(func() {
		release()
		cancelRun()
		select {
		case err := <-runDone:
			require.NoError(err)
		case <-time.After(10 * time.Second):
			require.FailNow("service did not stop after cancellation")
		}
	})
	select {
	case <-scheduledAcquired:
	case <-time.After(10 * time.Second):
		require.FailNow("scheduled work did not acquire the gate before reconciliation")
	}
	s.workersMu.Lock()
	assert.Empty(s.workers, "the event commits before reconciliation creates its worker")
	s.workersMu.Unlock()

	live := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: time.Now().UTC()})
	_, err = live.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().Build()})
	require.NoError(err)
	waitCtx, waitCancel := context.WithTimeout(t.Context(), 10*time.Second)
	requestWaiter := gate.waitForRequestWaiter(waitCtx)
	waitCancel()
	require.True(requestWaiter, "a committed event with no worker should promote queued reconciliation")
	release()

	var envelope Envelope
	select {
	case envelope = <-received:
	case <-time.After(10 * time.Second):
		require.FailNow("the committed occurrence was not delivered after reconciliation yielded")
	}
	assert.NotEmpty(envelope.EventID)
	assert.Equal(messageFamily, envelope.Name)
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), subscription.ID)
		return err == nil && row != nil && row.Generation == initial.Generation && row.CursorSeq > initial.CursorSeq && row.PendingSeq == 0
	}, 10*time.Second, 20*time.Millisecond, "the committed occurrence should be receipted without renewing the subscription")
}

func TestSubscribeRefreshRotationReplayAndSafeStatus(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, _, req := eventService(t)
	first, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	initial, err := s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	require.NotNil(initial)
	assert.Equal(int64(1), initial.Generation)
	assert.Equal(int64(1), initial.SecretRevision)
	assert.Equal(int64(1), initial.VerifiedRevision)
	assert.InDelta(time.Now().Add(24*time.Hour).UnixMilli(), first.RefreshBefore, 1000)
	// Durable verification is reused: the callback is deliberately unavailable.
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) { rw.WriteHeader(http.StatusInternalServerError) })
	renew, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	assert.Equal(first.ID, renew.ID)
	after, err := s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	assert.Equal(initial.Generation, after.Generation)
	renewedCiphertext := append([]byte(nil), after.SecretEnc...)
	req.Delivery.Secret = "whsec_" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("r", 32)))
	_, err = s.Subscribe(t.Context(), s.principal, req)
	require.Error(err)
	after, err = s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	assert.Equal(renewedCiphertext, after.SecretEnc, "failed challenge leaves predecessor untouched")
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		var v map[string]any
		if !Assert.NoError(t, json.NewDecoder(r.Body).Decode(&v)) {
			return
		}
		if !Assert.NoError(t, json.NewEncoder(rw).Encode(map[string]any{"challenge": v["challenge"]})) {
			return
		}
	})
	_, err = s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	after, err = s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	assert.Equal(int64(2), after.Generation)
	assert.Equal(int64(2), after.SecretRevision)
	old, err := decryptSecret(s.key, first.ID, "previous", after.PreviousSecretEnc)
	require.NoError(err)
	assert.Equal(make([]byte, 32), old)
	req.Cursor = &first.Cursor
	_, err = s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	after, err = s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	assert.Equal(int64(3), after.Generation)
	status, err := s.Status(t.Context(), s.principal)
	require.NoError(err)
	encoded, err := json.Marshal(status)
	require.NoError(err)
	assert.NotContains(string(encoded), req.Delivery.URL)
	assert.NotContains(string(encoded), req.Delivery.Secret)
	assert.NotContains(string(encoded), "SecretEnc")
	require.NoError(s.Unsubscribe(t.Context(), s.principal, UnsubscribeRequest{Name: req.Name, Arguments: req.Arguments, Delivery: req.Delivery}))
	assert.NoError(s.Unsubscribe(t.Context(), s.principal, UnsubscribeRequest{Name: req.Name, Arguments: req.Arguments, Delivery: req.Delivery}))
	after, err = s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	assert.Equal("unsubscribed", after.State)
}

func TestVerificationActivationRequiresExactPredecessor(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, _, req := eventService(t)
	first, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	entered, release := make(chan struct{}), make(chan struct{})
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		var v map[string]any
		if !Assert.NoError(t, json.NewDecoder(r.Body).Decode(&v)) {
			return
		}
		if !Assert.NoError(t, json.NewEncoder(rw).Encode(map[string]any{"challenge": v["challenge"]})) {
			return
		}
	})
	req.Delivery.Secret = "whsec_" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("r", 32)))
	done := make(chan error, 1)
	go func() { _, err := s.Subscribe(t.Context(), s.principal, req); done <- err }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		require.FailNow("challenge did not start")
	}
	require.NoError(s.Unsubscribe(t.Context(), s.principal, UnsubscribeRequest{Name: req.Name, Arguments: req.Arguments, Delivery: req.Delivery}))
	close(release)
	select {
	case err := <-done:
		require.Error(err)
		var eventErr *Error
		require.ErrorAs(err, &eventErr)
		assert.Equal("concurrent_update", eventErr.Reason)
	case <-time.After(10 * time.Second):
		require.FailNow("challenge did not finish")
	}
	after, err := s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	assert.Equal("unsubscribed", after.State)
}

func TestCallbackNetworkRunsOutsideOperationGate(t *testing.T) {
	s, _, req := eventService(t)
	var gated atomic.Bool
	s.opts.WithOperation = func(ctx context.Context, fn func() error) error {
		Require.False(t, gated.Swap(true))
		defer gated.Store(false)
		return fn()
	}
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		Assert.False(t, gated.Load())
		var v map[string]any
		if !Assert.NoError(t, json.NewDecoder(r.Body).Decode(&v)) {
			return
		}
		if !Assert.NoError(t, json.NewEncoder(rw).Encode(map[string]any{"challenge": v["challenge"]})) {
			return
		}
	})
	_, err := s.Subscribe(t.Context(), s.principal, req)
	Require.NoError(t, err)
}

func appendReceipt(t *testing.T, s *Service, f *storetest.Fixture, seq int64) {
	t.Helper()
	var epoch int64
	Require.NoError(t, f.Store.DB().QueryRow(`SELECT capture_epoch FROM mcp_event_clock WHERE singleton=1`).Scan(&epoch))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO mcp_event_log(seq,epoch,family,kind,scope_kind,scope_id,item_key,conversation_id,source_id,from_me,occurred_at,recorded_at,data) VALUES(?,?,'msgvault.message_archived','message','conversation',?,?,?, ?,FALSE,?,?,?)`), seq, epoch, f.ConvID, fmt.Sprintf("synthetic:%d", seq), f.ConvID, f.Source.ID, now, now, `{"kind":"message","from_me":false}`)
	Require.NoError(t, err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_clock SET head_seq=? WHERE singleton=1`), seq)
	Require.NoError(t, err)
	s.Wake()
}

func TestIndependentWorkersAndShutdownJoin(t *testing.T) {
	require := Require.New(t)
	s, f, req := eventService(t)
	first, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	req.Delivery.URL = "https://other.example.net/events"
	second, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	blocked, fast, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		if !Assert.NoError(t, err) {
			return
		}
		if r.Header.Get("X-Mcp-Subscription-Id") == first.ID {
			close(blocked)
			<-r.Context().Done()
			close(joined)
		} else {
			Assert.Equal(t, second.ID, r.Header.Get("X-Mcp-Subscription-Id"))
			close(fast)
			rw.WriteHeader(http.StatusNoContent)
		}
	})
	appendReceipt(t, s, f, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case <-blocked:
	case <-time.After(10 * time.Second):
		require.FailNow("first worker did not dial")
	}
	select {
	case <-fast:
	case <-time.After(10 * time.Second):
		require.FailNow("slow receiver stalled independent worker")
	}
	cancel()
	select {
	case err := <-done:
		require.NoError(err)
	case <-time.After(10 * time.Second):
		require.FailNow("Run did not join workers")
	}
	select {
	case <-joined:
	case <-time.After(10 * time.Second):
		require.FailNow("callback not cancelled")
	}
}

func TestWorkerRetriesAfterPostDeliveryAuthorizationStorageError(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	firstAckWritten := make(chan struct{})
	releaseFirstAck := make(chan struct{})
	type receivedDelivery struct {
		eventID string
		body    []byte
	}
	deliveries := make(chan receivedDelivery, 2)
	var attempts atomic.Int32
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !Assert.NoError(t, err) {
			return
		}
		deliveries <- receivedDelivery{eventID: r.Header.Get("Webhook-Id"), body: body}
		rw.WriteHeader(http.StatusNoContent)
		if attempts.Add(1) == 1 {
			close(firstAckWritten)
			<-releaseFirstAck
		}
	})
	var failNextOperation atomic.Bool
	injected := make(chan struct{})
	s.opts.WithOperation = func(ctx context.Context, fn func() error) error {
		if failNextOperation.CompareAndSwap(true, false) {
			close(injected)
			return errors.New("synthetic transient operation gate failure")
		}
		return fn()
	}
	appendReceipt(t, s, f, 1)
	before, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	var first receivedDelivery
	select {
	case first = <-deliveries:
	case <-time.After(10 * time.Second):
		require.FailNow("first receiver delivery did not arrive")
	}
	select {
	case <-firstAckWritten:
	case <-time.After(10 * time.Second):
		require.FailNow("receiver did not write the first ACK")
	}
	failNextOperation.Store(true)
	close(releaseFirstAck)
	select {
	case <-injected:
	case <-time.After(10 * time.Second):
		require.FailNow("post-delivery authorization check did not hit the injected gate error")
	}
	var second receivedDelivery
	select {
	case second = <-deliveries:
	case <-time.After(10 * time.Second):
		require.FailNow("worker did not retry the pending delivery")
	}
	assert.Equal(first.eventID, second.eventID, "retry must reuse the event ID")
	assert.Equal(first.body, second.body, "retry must reuse the durable envelope")
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
		return err == nil && row.CursorSeq == 1 && row.PendingSeq == 0
	}, 10*time.Second, 10*time.Millisecond)
	after, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	assert.Equal(before.Generation, after.Generation, "a delivery retry must not renew the subscription")
	assert.Equal(before.ExpiresAt, after.ExpiresAt, "a delivery retry must not extend subscription expiry")
	cancel()
	select {
	case err := <-done:
		require.NoError(err)
	case <-time.After(10 * time.Second):
		require.FailNow("Run did not join the retried worker")
	}
}

func TestWorkerRetryWaitStopsWhenSubscriptionIsRevoked(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	ackWritten := make(chan struct{})
	releaseAck := make(chan struct{})
	deliveryArrived := make(chan struct{}, 1)
	var attempts atomic.Int32
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		if !Assert.NoError(t, err) {
			return
		}
		select {
		case deliveryArrived <- struct{}{}:
		default:
		}
		rw.WriteHeader(http.StatusNoContent)
		if attempts.Add(1) == 1 {
			close(ackWritten)
			<-releaseAck
		}
	})
	var failNextOperation atomic.Bool
	injected := make(chan struct{})
	s.opts.WithOperation = func(ctx context.Context, fn func() error) error {
		if failNextOperation.CompareAndSwap(true, false) {
			close(injected)
			return errors.New("synthetic transient operation gate failure")
		}
		return fn()
	}
	appendReceipt(t, s, f, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case <-deliveryArrived:
	case <-time.After(10 * time.Second):
		require.FailNow("receiver did not get the pending occurrence")
	}
	select {
	case <-ackWritten:
	case <-time.After(10 * time.Second):
		require.FailNow("receiver did not write the ACK")
	}
	failNextOperation.Store(true)
	close(releaseAck)
	select {
	case <-injected:
	case <-time.After(10 * time.Second):
		require.FailNow("post-delivery authorization check did not hit the injected gate error")
	}
	require.NoError(s.Unsubscribe(t.Context(), s.principal, UnsubscribeRequest{Name: req.Name, Arguments: req.Arguments, Delivery: req.Delivery}))
	sub, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	require.NotNil(sub)
	Assert.Equal(t, "unsubscribed", sub.State)
	Assert.Zero(t, sub.PendingSeq)
	s.workersMu.Lock()
	_, workerStillTracked := s.workers[result.ID]
	s.workersMu.Unlock()
	assert.False(workerStillTracked, "revocation must remove and join the subscription worker")
	cancel()
	select {
	case err := <-done:
		require.NoError(err)
	case <-time.After(10 * time.Second):
		require.FailNow("Run did not join after subscription revocation")
	}
}

func TestWorkerRetriesFinishAfterAckWithoutRedelivery(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	before, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	require.NotNil(before)

	type receivedDelivery struct {
		eventID string
		body    []byte
	}
	deliveries := make(chan receivedDelivery, 2)
	var receiverAcked atomic.Bool
	var postAckOperations atomic.Int32
	finishFailed := make(chan struct{})
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !Assert.NoError(t, err) {
			return
		}
		deliveries <- receivedDelivery{eventID: r.Header.Get("Webhook-Id"), body: body}
		receiverAcked.Store(true)
		rw.WriteHeader(http.StatusNoContent)
	})
	s.opts.WithOperation = func(ctx context.Context, fn func() error) error {
		if receiverAcked.Load() && postAckOperations.Add(1) == 2 {
			close(finishFailed)
			return errors.New("synthetic transient FinishMCPDelivery gate failure")
		}
		return fn()
	}
	appendReceipt(t, s, f, 1)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runWorker(ctx, result.ID, &worker{generation: before.Generation, wake: make(chan struct{}, 1)})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			require.FailNow("worker did not stop after cancellation")
		}
	})

	var first receivedDelivery
	select {
	case first = <-deliveries:
	case <-time.After(10 * time.Second):
		require.FailNow("receiver did not get the pending occurrence")
	}
	select {
	case <-finishFailed:
	case <-time.After(10 * time.Second):
		require.FailNow("FinishMCPDelivery did not hit the injected gate failure after ACK")
	}
	pending, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	require.NotNil(pending)
	assert.Equal(int64(1), pending.PendingSeq)
	assert.Equal(first.body, pending.PendingEnvelope, "a failed receipt update must preserve the durable envelope")
	assert.Equal(1, pending.AttemptCount, "retrying receipt persistence must not consume a delivery attempt")
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
		return err == nil && row.CursorSeq == 1 && row.PendingSeq == 0
	}, 10*time.Second, 10*time.Millisecond)
	after, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	require.NotNil(after)
	assert.Equal(before.Generation, after.Generation, "a delivery retry must not renew the subscription")
	assert.Equal(before.ExpiresAt, after.ExpiresAt, "a delivery retry must not extend subscription expiry")
	assert.Zero(after.AttemptCount, "a completed receipt clears the pending attempt count")
	assert.Equal("acknowledged", after.LastOutcome)
	select {
	case duplicate := <-deliveries:
		assert.Fail("worker redelivered an event after the receiver ACK while retrying FinishMCPDelivery", "event ID %q", duplicate.eventID)
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		require.FailNow("worker did not join after cursor acknowledgement")
	}
}

func TestWorkerRetryWaitIsCancellationAware(t *testing.T) {
	require := Require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	w := &worker{wake: make(chan struct{}, 1)}
	done := make(chan bool, 1)
	go func() { done <- waitForWorker(ctx, w, time.Hour) }()
	cancel()
	select {
	case continued := <-done:
		require.False(continued, "a cancelled worker must leave its retry wait")
	case <-time.After(time.Second):
		require.FailNow("worker did not leave its retry wait after cancellation")
	}
}

func TestWorkerRetryBackoffWaitIsCancellationAware(t *testing.T) {
	require := Require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan bool, 1)
	go func() { done <- waitForWorkerRetry(ctx, time.Hour) }()
	cancel()
	select {
	case continued := <-done:
		require.False(continued, "a cancelled worker must leave the receipt retry backoff")
	case <-time.After(time.Second):
		require.FailNow("worker did not leave receipt retry backoff after cancellation")
	}
}
