package cmd

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/scheduler"
)

type fakeDaemonWorkTracker struct {
	mu     sync.Mutex
	allow  bool
	begin  int
	done   int
	events *[]string
	name   string
}

func (t *fakeDaemonWorkTracker) BeginWork() (func(), bool) {
	return t.BeginWorkContext(context.Background())
}

func (t *fakeDaemonWorkTracker) BeginWorkContext(ctx context.Context) (func(), bool) {
	if ctx != nil && ctx.Err() != nil {
		return func() {}, false
	}
	t.mu.Lock()
	t.begin++
	if t.events != nil {
		*t.events = append(*t.events, "begin:"+t.name)
	}
	allow := t.allow
	t.mu.Unlock()
	if !allow {
		return func() {}, false
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			t.done++
			if t.events != nil {
				*t.events = append(*t.events, "done:"+t.name)
			}
			t.mu.Unlock()
		})
	}, true
}

func (t *fakeDaemonWorkTracker) counts() (int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.begin, t.done
}

func TestCombineWorkTrackersBeginsAndReleasesAll(t *testing.T) {
	assert := assert.New(
		t)

	var events []string
	first := &fakeDaemonWorkTracker{allow: true, name: "first", events: &events}
	second := &fakeDaemonWorkTracker{allow: true, name: "second", events: &events}

	tracker := combineWorkTrackers(nil, first, second)
	done, ok := tracker.BeginWork()
	require.True(t, ok, "BeginWork")
	done()
	done()
	assert.Equal([]string{
		"begin:first",
		"begin:second",
		"done:second",
		"done:first",
	}, events, "tracker order")
	firstBegin, firstDone := first.counts()
	secondBegin, secondDone := second.counts()
	assert.Equal(1, firstBegin, "first begin")
	assert.Equal(1, firstDone, "first done")
	assert.Equal(1, secondBegin, "second begin")
	assert.Equal(1, secondDone, "second done")
}

func TestCombineWorkTrackersUnwindsWhenLaterTrackerRejects(t *testing.T) {
	assert := assert.New(
		t)

	var events []string
	first := &fakeDaemonWorkTracker{allow: true, name: "first", events: &events}
	second := &fakeDaemonWorkTracker{allow: false, name: "second", events: &events}

	tracker := combineWorkTrackers(first, second)
	done, ok := tracker.BeginWork()
	assert.False(ok, "BeginWork")
	done()
	assert.Equal([]string{
		"begin:first",
		"begin:second",
		"done:first",
	}, events, "tracker order")
	firstBegin, firstDone := first.counts()
	secondBegin, secondDone := second.counts()
	assert.Equal(1, firstBegin, "first begin")
	assert.Equal(1, firstDone, "first done")
	assert.Equal(1, secondBegin, "second begin")
	assert.Equal(0, secondDone, "second done")
}

func TestServeSchedulerReportsActualGateHolder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		gate := api.NewSerialOperationGate()
		sched, media := newServeSchedulers(nil, testDiscardLogger(), &fakeDaemonWorkTracker{allow: true}, gate)
		defer func() { <-sched.Stop().Done(); <-media.Stop().Done() }()
		release := make(chan struct{})
		require.NoError(sched.AddJob(scheduler.Job{Name: "activity-projection", Schedule: "0 0 1 1 *", Run: func(context.Context) error {
			<-release
			return nil
		}}))
		require.NoError(sched.StartJob("activity-projection"))
		synctest.Wait()
		label, since, busy := gate.Holder()
		assert.True(busy)
		assert.False(since.IsZero())
		assert.Equal("activity-projection", label)
		close(release)
		synctest.Wait()
	})
}
