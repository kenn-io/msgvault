package metawebhook

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func feedDelivery(value string) string {
	return fmt.Sprintf(`{"object":"page","entry":[{"id":"100","time":10,"changes":[{"field":"feed","value":{"message":%q}}]}]}`, value)
}
func appliedItems(_ context.Context, _ string, items []Item) []ItemOutcome {
	out := make([]ItemOutcome, len(items))
	for i := range out {
		out[i].Class = Applied
	}
	return out
}
func TestDrainAppliesInReceivedOrder(t *testing.T) {
	s, now := testSpool(t)
	first := accept(t, s, feedDelivery("first"))
	*now = now.Add(time.Second)
	second := accept(t, s, feedDelivery("second"))
	reg := NewRegistry()
	var got []string
	reg.Register("page", "feed", func(ctx context.Context, app string, items []Item) []ItemOutcome {
		for _, item := range items {
			got = append(got, item.DeliveryID)
		}
		return appliedItems(ctx, app, items)
	})
	w := NewWorker(WorkerOptions{Spool: s, Registry: reg})
	sum, err := w.DrainOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{first, second}, got)
	assert.Equal(t, 2, sum.Deliveries)
	assert.Equal(t, 2, sum.Applied)
	stats, err := s.Stats()
	require.NoError(t, err)
	assert.Equal(t, 2, stats["9"].Done)
}
func TestDrainGroupsItemsPerHandlerPreservingOrder(t *testing.T) {
	s, now := testSpool(t)
	accept(t, s, feedDelivery("first"))
	*now = now.Add(time.Second)
	accept(t, s, `{"object":"page","entry":[{"id":"100","changes":[{"field":"mention","value":{}},{"field":"feed","value":{"message":"second"}}]}]}`)
	reg := NewRegistry()
	calls := 0
	reg.Register("page", "feed", func(ctx context.Context, app string, items []Item) []ItemOutcome {
		calls++
		require.Len(t, items, 2)
		assert.JSONEq(t, `{"message":"first"}`, string(items[0].Value))
		assert.JSONEq(t, `{"message":"second"}`, string(items[1].Value))
		return appliedItems(ctx, app, items)
	})
	_, err := NewWorker(WorkerOptions{Spool: s, Registry: reg}).DrainOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}
func mixedDelivery() string {
	return `{"object":"page","entry":[{"id":"100","changes":[{"field":"feed","value":{}}],"messaging":[{"message":{"mid":"synthetic-1","text":"example"}}]}]}`
}
func TestDrainMixedDeliveryHeldWhole(t *testing.T) {
	s, _ := testSpool(t)
	id := accept(t, s, mixedDelivery())
	reg := NewRegistry()
	calls := 0
	reg.Register("page", "feed", func(ctx context.Context, app string, items []Item) []ItemOutcome {
		calls++
		return appliedItems(ctx, app, items)
	})
	sum, err := NewWorker(WorkerOptions{Spool: s, Registry: reg}).DrainOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.Equal(t, 1, sum.Held)
	var m DeliveryMeta
	require.NoError(t, readJSON(statePath(s.path("9", StateHeld, id)), &m))
	assert.Equal(t, 1, m.Outcomes["page/messages/unhandled"])
	assert.Equal(t, 1, m.Outcomes["page/feed/applied"])
}
func TestReplayHeldReappliesIdempotently(t *testing.T) {
	s, _ := testSpool(t)
	id := accept(t, s, mixedDelivery())
	reg := NewRegistry()
	seen := map[string]bool{}
	calls := 0
	reg.Register("page", "feed", func(ctx context.Context, app string, items []Item) []ItemOutcome {
		calls++
		for _, item := range items {
			seen[item.DeliveryID] = true
		}
		return appliedItems(ctx, app, items)
	})
	w := NewWorker(WorkerOptions{Spool: s, Registry: reg})
	_, err := w.DrainOnce(t.Context())
	require.NoError(t, err)
	reg.Register("page", "messages", appliedItems)
	_, err = s.Requeue(RequeueFilter{States: []State{StateHeld}}, false)
	require.NoError(t, err)
	_, err = w.DrainOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
	assert.Equal(t, map[string]bool{id: true}, seen)
	stats, err := s.Stats()
	require.NoError(t, err)
	assert.Equal(t, 1, stats["9"].Done)
}
func TestDrainRetryBackoff(t *testing.T) {
	s, now := testSpool(t)
	accept(t, s, feedDelivery("retry"))
	reg := NewRegistry()
	reg.Register("page", "feed", func(context.Context, string, []Item) []ItemOutcome {
		return []ItemOutcome{{Class: Retry, ErrorClass: "permission_missing", CountAttempt: true}}
	})
	w := NewWorker(WorkerOptions{Spool: s, Registry: reg})
	for i := 1; i <= 12; i++ {
		sum, err := w.DrainOnce(t.Context())
		require.NoError(t, err)
		if i < 12 {
			assert.Equal(t, 1, sum.Retried)
			due, err := s.Due(1)
			require.NoError(t, err)
			assert.Empty(t, due)
		} else {
			assert.Equal(t, 1, sum.Failed)
		}
		*now = now.Add(time.Hour)
	}
}
func TestDrainInvalidBodyHeld(t *testing.T) {
	s, _ := testSpool(t)
	id := accept(t, s, "invalid json")
	sum, err := NewWorker(WorkerOptions{Spool: s, Registry: NewRegistry()}).DrainOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Held)
	_, err = os.Stat(s.path("9", StateHeld, id))
	assert.NoError(t, err)
}
func TestRunWakeDebounceAndShutdown(t *testing.T) {
	s, _ := testSpool(t)
	id := accept(t, s, feedDelivery("shutdown"))
	reg := NewRegistry()
	entered := make(chan struct{})
	reg.Register("page", "feed", func(ctx context.Context, _ string, _ []Item) []ItemOutcome {
		close(entered)
		<-ctx.Done()
		return []ItemOutcome{{Class: Retry}}
	})
	w := NewWorker(WorkerOptions{Spool: s, Registry: reg, Debounce: time.Millisecond, Tick: time.Hour})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	w.Wake()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "worker did not wake")
	}
	cancel()
	require.NoError(t, <-done)
	count := 0
	for _, state := range states {
		_, err := os.Stat(s.path("9", state, id))
		if err == nil {
			count++
			var meta DeliveryMeta
			require.NoError(t, readJSON(statePath(s.path("9", state, id)), &meta))
		}
	}
	assert.Equal(t, 1, count)
}
func TestPruneOnTick(t *testing.T) {
	s, now := testSpool(t)
	id := accept(t, s, feedDelivery("prune"))
	require.NoError(t, s.Settle("9", id, Settlement{State: StateDone}))
	*now = now.Add(2 * time.Hour)
	w := NewWorker(WorkerOptions{Spool: s, Registry: NewRegistry(), Tick: time.Millisecond, Retention: time.Hour})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	assert.Eventually(t, func() bool { _, err := os.Stat(s.path("9", StateDone, id)); return os.IsNotExist(err) }, 5*time.Second, time.Millisecond)
	cancel()
	require.NoError(t, <-done)
}
func TestOnAppliedCalledOnlyWhenApplied(t *testing.T) {
	s, _ := testSpool(t)
	accept(t, s, feedDelivery("held"))
	reg := NewRegistry()
	var refreshed int
	w := NewWorker(WorkerOptions{Spool: s, Registry: reg, OnApplied: func() { refreshed++ }})
	_, err := w.DrainOnce(t.Context())
	require.NoError(t, err)
	assert.Zero(t, refreshed)
	reg.Register("page", "feed", appliedItems)
	accept(t, s, feedDelivery("applied"))
	_, err = w.DrainOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, refreshed)
}
func TestDrainConsumesReplayRequestsFirst(t *testing.T) {
	s, _ := testSpool(t)
	id := accept(t, s, feedDelivery("replay"))
	require.NoError(t, s.Settle("9", id, Settlement{State: StateHeld}))
	request, err := SubmitRequeue(s.dir, RequeueFilter{States: []State{StateHeld}}, false)
	require.NoError(t, err)
	reg := NewRegistry()
	reg.Register("page", "feed", appliedItems)
	_, err = NewWorker(WorkerOptions{Spool: s, Registry: reg}).DrainOnce(t.Context())
	require.NoError(t, err)
	n, err := AwaitRequeueResult(t.Context(), s.dir, request)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	_, err = os.Stat(s.path("9", StateDone, id))
	assert.NoError(t, err)
}
func TestRunWritesReceiverSnapshot(t *testing.T) {
	s, _ := testSpool(t)
	var snapshots atomic.Int32
	w := NewWorker(WorkerOptions{Spool: s, Registry: NewRegistry(), Tick: time.Millisecond, Snapshot: func() ReceiverSnapshot {
		snapshots.Add(1)
		return ReceiverSnapshot{Listener: "listening", Addr: "127.0.0.1:12345"}
	}})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	assert.Eventually(t, func() bool {
		snap, err := ReadReceiverSnapshot(s.dir)
		return err == nil && snap.Listener == "listening" && !snap.WrittenAt.IsZero()
	}, 5*time.Second, time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	assert.GreaterOrEqual(t, snapshots.Load(), int32(1))
	files, err := filepath.Glob(filepath.Join(s.dir, ".tmp-*"))
	require.NoError(t, err)
	assert.Empty(t, files)
}
