package cmd

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	var events []string
	first := &fakeDaemonWorkTracker{allow: true, name: "first", events: &events}
	second := &fakeDaemonWorkTracker{allow: true, name: "second", events: &events}

	tracker := combineWorkTrackers(nil, first, second)
	done, ok := tracker.BeginWork()
	require.True(t, ok, "BeginWork")
	done()
	done()

	assert.Equal(t, []string{
		"begin:first",
		"begin:second",
		"done:second",
		"done:first",
	}, events, "tracker order")
	firstBegin, firstDone := first.counts()
	secondBegin, secondDone := second.counts()
	assert.Equal(t, 1, firstBegin, "first begin")
	assert.Equal(t, 1, firstDone, "first done")
	assert.Equal(t, 1, secondBegin, "second begin")
	assert.Equal(t, 1, secondDone, "second done")
}

func TestCombineWorkTrackersUnwindsWhenLaterTrackerRejects(t *testing.T) {
	var events []string
	first := &fakeDaemonWorkTracker{allow: true, name: "first", events: &events}
	second := &fakeDaemonWorkTracker{allow: false, name: "second", events: &events}

	tracker := combineWorkTrackers(first, second)
	done, ok := tracker.BeginWork()

	assert.False(t, ok, "BeginWork")
	done()
	assert.Equal(t, []string{
		"begin:first",
		"begin:second",
		"done:first",
	}, events, "tracker order")
	firstBegin, firstDone := first.counts()
	secondBegin, secondDone := second.counts()
	assert.Equal(t, 1, firstBegin, "first begin")
	assert.Equal(t, 1, firstDone, "first done")
	assert.Equal(t, 1, secondBegin, "second begin")
	assert.Equal(t, 0, secondDone, "second done")
}
