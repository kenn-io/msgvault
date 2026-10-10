package mcpevents

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Match existing package test assertion aliases.
	Require "github.com/stretchr/testify/require" //nolint:importas // Match existing package test assertion aliases.
)

func TestReconcileRestartsWorkerAfterTransientPostDeliveryGuardFailure(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	subscription, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	appendReceipt(t, s, f, 1)

	var failNext atomic.Bool
	s.opts.WithOperation = func(_ context.Context, fn func() error) error {
		if failNext.CompareAndSwap(true, false) {
			return errors.New("synthetic transient operation gate failure")
		}
		return fn()
	}
	type request struct {
		body                    []byte
		eventID, subscriptionID string
	}
	deliveries := make(chan request, 4)
	var attempts atomic.Int64
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !Assert.NoError(t, err) {
			return
		}
		if attempts.Add(1) == 1 {
			failNext.Store(true)
		}
		select {
		case deliveries <- request{body, r.Header.Get("Webhook-Id"), r.Header.Get("X-Mcp-Subscription-Id")}:
		case <-r.Context().Done():
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		s.workersMu.Lock()
		workers := make([]*worker, 0, len(s.workers))
		for _, w := range s.workers {
			w.cancel()
			workers = append(workers, w)
		}
		s.workersMu.Unlock()
		for _, w := range workers {
			select {
			case <-w.done:
			case <-time.After(15 * time.Second):
				assert.Fail("delivery worker did not stop")
			}
		}
		s.webhook.transport.CloseIdleConnections()
	})
	require.NoError(s.reconcile(ctx))
	s.workersMu.Lock()
	firstWorker := s.workers[subscription.ID]
	s.workersMu.Unlock()
	require.NotNil(firstWorker)
	select {
	case <-firstWorker.done:
	case <-time.After(15 * time.Second):
		require.FailNow("post-delivery guard failure did not stop the first worker")
	}
	var first request
	select {
	case first = <-deliveries:
	case <-time.After(15 * time.Second):
		require.FailNow("first worker stopped without delivering the pending event")
	}
	row, err := s.st.GetMCPSubscription(t.Context(), subscription.ID)
	require.NoError(err)
	require.NotNil(row)
	assert.Equal("active", row.State)
	assert.Zero(row.CursorSeq, "failed authority check must not acknowledge the event")
	assert.Equal(int64(1), row.PendingSeq)
	assert.Equal(first.body, row.PendingEnvelope)
	assert.False(failNext.Load(), "the post-delivery guard must consume the injected failure")

	require.NoError(s.reconcile(ctx))
	select {
	case retry := <-deliveries:
		assert.Equal(first, retry, "recovery must preserve the durable event identity and envelope")
	case <-time.After(15 * time.Second):
		require.FailNow("reconciliation did not restart delivery for the active subscription")
	}
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), subscription.ID)
		return err == nil && row != nil && row.CursorSeq == 1 && row.PendingSeq == 0
	}, 15*time.Second, 10*time.Millisecond)
}
