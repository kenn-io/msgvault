package metawebhook

import (
	"context"
	"fmt"
	"os"
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
	assert := assert.New(t)
	require := require.New(t)
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
	require.NoError(err)
	assert.Equal([]string{first, second}, got)
	assert.Equal(2, sum.Deliveries)
	assert.Equal(2, sum.Applied)
	stats, err := s.Stats()
	require.NoError(err)
	assert.Equal(2, stats["9"].Done)
}
func TestDrainGroupsItemsPerHandlerPreservingOrder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, now := testSpool(t)
	accept(t, s, feedDelivery("first"))
	*now = now.Add(time.Second)
	accept(t, s, `{"object":"page","entry":[{"id":"100","changes":[{"field":"mention","value":{}},{"field":"feed","value":{"message":"second"}}]}]}`)
	reg := NewRegistry()
	calls := 0
	reg.Register("page", "feed", func(ctx context.Context, app string, items []Item) []ItemOutcome {
		calls++
		require.Len(items, 2)
		assert.JSONEq(`{"message":"first"}`, string(items[0].Value))
		assert.JSONEq(`{"message":"second"}`, string(items[1].Value))
		return appliedItems(ctx, app, items)
	})
	_, err := NewWorker(WorkerOptions{Spool: s, Registry: reg}).DrainOnce(t.Context())
	require.NoError(err)
	assert.Equal(1, calls)
}

func TestDrainContinuesWhenGroupCannotAcquireGate(t *testing.T) {
	s, _ := testSpool(t)
	accept(t, s, `{"object":"page","entry":[{"id":"100","changes":[{"field":"feed","value":{"message":"ordinary"}},{"field":"messaging_optins","value":{"notification_messages_status":"STOP_NOTIFICATIONS"}}]}]}`)
	reg := NewRegistry()
	var handled []string
	for _, field := range []string{"feed", "messaging_optins"} {
		reg.Register("page", field, func(ctx context.Context, app string, items []Item) []ItemOutcome {
			handled = append(handled, field)
			return appliedItems(ctx, app, items)
		})
	}
	var attempted []string
	w := NewWorker(WorkerOptions{
		Spool: s, Registry: reg,
		BeginApplyGroup: func(_ context.Context, _, field string) (func(), bool) {
			attempted = append(attempted, field)
			return nil, field == "messaging_optins"
		},
	})
	sum, err := w.DrainOnce(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"feed", "messaging_optins"}, attempted)
	assert.Equal(t, []string{"messaging_optins"}, handled)
	assert.Equal(t, 1, sum.Applied)
	assert.Equal(t, 1, sum.Retried)
	due, err := s.Due(1)
	require.NoError(t, err)
	assert.Empty(t, due)
}

func mixedDelivery() string {
	return `{"object":"page","entry":[{"id":"100","changes":[{"field":"feed","value":{}}],"messaging":[{"message":{"mid":"synthetic-1","text":"example"}}]}]}`
}
func TestDrainMixedDeliveryHeldWhole(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, _ := testSpool(t)
	id := accept(t, s, mixedDelivery())
	reg := NewRegistry()
	calls := 0
	reg.Register("page", "feed", func(ctx context.Context, app string, items []Item) []ItemOutcome {
		calls++
		return appliedItems(ctx, app, items)
	})
	sum, err := NewWorker(WorkerOptions{Spool: s, Registry: reg}).DrainOnce(t.Context())
	require.NoError(err)
	assert.Equal(1, calls)
	assert.Equal(1, sum.Held)
	var m DeliveryMeta
	require.NoError(readJSON(statePath(s.path("9", StateHeld, id)), &m))
	assert.Equal(1, m.Outcomes["page/messages/unhandled"])
	assert.Equal(1, m.Outcomes["page/feed/applied"])
}
func TestReplayHeldReappliesIdempotently(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
	require.NoError(err)
	reg.Register("page", "messages", appliedItems)
	_, err = s.Requeue(RequeueFilter{States: []State{StateHeld}}, false)
	require.NoError(err)
	_, err = w.DrainOnce(t.Context())
	require.NoError(err)
	assert.Equal(2, calls)
	assert.Equal(map[string]bool{id: true}, seen)
	stats, err := s.Stats()
	require.NoError(err)
	assert.Equal(1, stats["9"].Done)
}
func TestDrainRetryBackoff(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, now := testSpool(t)
	accept(t, s, feedDelivery("retry"))
	reg := NewRegistry()
	reg.Register("page", "feed", func(context.Context, string, []Item) []ItemOutcome {
		return []ItemOutcome{{Class: Retry, ErrorClass: "permission_missing", CountAttempt: true}}
	})
	w := NewWorker(WorkerOptions{Spool: s, Registry: reg})
	for i := 1; i <= 12; i++ {
		sum, err := w.DrainOnce(t.Context())
		require.NoError(err)
		if i < 12 {
			assert.Equal(1, sum.Retried)
			due, err := s.Due(1)
			require.NoError(err)
			assert.Empty(due)
		} else {
			assert.Equal(1, sum.Failed)
		}
		*now = now.Add(time.Hour)
	}
}
func TestDrainInvalidBodyHeld(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, _ := testSpool(t)
	id := accept(t, s, "invalid json")
	sum, err := NewWorker(WorkerOptions{Spool: s, Registry: NewRegistry()}).DrainOnce(t.Context())
	require.NoError(err)
	assert.Equal(1, sum.Held)
	_, err = os.Stat(s.path("9", StateHeld, id))
	assert.NoError(err)
}
func TestRunWakeDebounceAndShutdown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
		require.FailNow("worker did not wake")
	}
	cancel()
	require.NoError(<-done)
	count := 0
	for _, state := range states {
		_, err := os.Stat(s.path("9", state, id))
		if err == nil {
			count++
			var meta DeliveryMeta
			require.NoError(readJSON(statePath(s.path("9", state, id)), &meta))
		}
	}
	assert.Equal(1, count)
}
func TestPruneOnTick(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, now := testSpool(t)
	id := accept(t, s, feedDelivery("prune"))
	require.NoError(s.Settle("9", id, Settlement{State: StateDone}))
	*now = now.Add(2 * time.Hour)
	w := NewWorker(WorkerOptions{Spool: s, Registry: NewRegistry(), Tick: time.Millisecond, Retention: time.Hour})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	assert.Eventually(func() bool { _, err := os.Stat(s.path("9", StateDone, id)); return os.IsNotExist(err) }, 5*time.Second, time.Millisecond)
	cancel()
	require.NoError(<-done)
}
func TestOnAppliedCalledOnlyWhenApplied(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, _ := testSpool(t)
	accept(t, s, feedDelivery("held"))
	reg := NewRegistry()
	var refreshed int
	w := NewWorker(WorkerOptions{Spool: s, Registry: reg, OnApplied: func() { refreshed++ }})
	_, err := w.DrainOnce(t.Context())
	require.NoError(err)
	assert.Zero(refreshed)
	reg.Register("page", "feed", appliedItems)
	accept(t, s, feedDelivery("applied"))
	_, err = w.DrainOnce(t.Context())
	require.NoError(err)
	assert.Equal(1, refreshed)
}
func TestDrainConsumesReplayRequestsFirst(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, _ := testSpool(t)
	id := accept(t, s, feedDelivery("replay"))
	require.NoError(s.Settle("9", id, Settlement{State: StateHeld}))
	request, err := SubmitRequeue(s.dir, RequeueFilter{States: []State{StateHeld}}, false)
	require.NoError(err)
	reg := NewRegistry()
	reg.Register("page", "feed", appliedItems)
	_, err = NewWorker(WorkerOptions{Spool: s, Registry: reg}).DrainOnce(t.Context())
	require.NoError(err)
	n, err := AwaitRequeueResult(t.Context(), s.dir, request)
	require.NoError(err)
	assert.Equal(1, n)
	_, err = os.Stat(s.path("9", StateDone, id))
	assert.NoError(err)
}

func TestDrainYieldPreservesEarlierProviderFailure(t *testing.T) {
	for _, errorClass := range []string{"cancelled_before_dispatch", "cancelled"} {
		t.Run(errorClass, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			s, now := testSpool(t)
			id := accept(t, s, `{"object":"page","entry":[{"id":"100","changes":[{"field":"feed","value":{}},{"field":"feed","value":{}}]}]}`)
			reg := NewRegistry()
			reg.Register("page", "feed", func(ctx context.Context, _ string, _ []Item) []ItemOutcome {
				<-ctx.Done()
				return []ItemOutcome{
					{Class: Retry, ErrorClass: "permission_missing", CountAttempt: true},
					{Class: Retry, ErrorClass: errorClass, CountAttempt: true},
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			_, err := NewWorker(WorkerOptions{Spool: s, Registry: reg, ShouldYield: func() bool { return true }}).DrainOnce(ctx)
			require.NoError(err)
			var meta DeliveryMeta
			require.NoError(readJSON(statePath(s.path("9", StatePending, id)), &meta))
			assert.Equal(1, meta.Attempts, "the earlier provider failure must still count")
			assert.Equal("permission_missing", meta.LastErrorClass)
			assert.Equal(now.Add(30*time.Second), meta.NextAttemptAt)
		})
	}
}
