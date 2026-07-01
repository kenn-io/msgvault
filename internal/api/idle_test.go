package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type idleTrackerFixture struct {
	tracker *IdleTracker
	fired   chan struct{}
}

func newIdleTrackerFixture(t *testing.T, timeout time.Duration) *idleTrackerFixture {
	t.Helper()
	fired := make(chan struct{}, 1)
	return &idleTrackerFixture{
		tracker: NewIdleTracker(timeout, func() { fired <- struct{}{} }),
		fired:   fired,
	}
}

func (f *idleTrackerFixture) run(t *testing.T) {
	t.Helper()
	go f.tracker.Run(t.Context())
}

func (f *idleTrackerFixture) requireNotFiredWithin(t *testing.T, d time.Duration, msg string) {
	t.Helper()
	select {
	case <-f.fired:
		require.FailNow(t, msg)
	case <-time.After(d):
	}
}

func (f *idleTrackerFixture) requireFiredWithin(t *testing.T, d time.Duration, msg string) {
	t.Helper()
	select {
	case <-f.fired:
	case <-time.After(d):
		require.FailNow(t, msg)
	}
}

func serveTrackedNoContent(t *testing.T, tracker *IdleTracker) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	tracker.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec
}

func TestIdleTrackerExternalRequestResetsIdle(t *testing.T) {
	f := newIdleTrackerFixture(t, 40*time.Millisecond)
	f.run(t)

	time.Sleep(25 * time.Millisecond)
	serveTrackedNoContent(t, f.tracker)

	f.requireNotFiredWithin(t, 25*time.Millisecond, "idle fired before reset timeout elapsed")
	f.requireFiredWithin(t, 80*time.Millisecond, "idle did not fire after external activity")
}

func TestIdleTrackerInternalWorkBlocksIdle(t *testing.T) {
	f := newIdleTrackerFixture(t, 20*time.Millisecond)
	done, ok := f.tracker.BeginWork()
	require.True(t, ok, "BeginWork")
	f.run(t)

	f.requireNotFiredWithin(t, 35*time.Millisecond, "idle fired while internal work was active")

	done()
	f.requireFiredWithin(t, 80*time.Millisecond, "idle did not fire after internal work ended")
}

func TestIdleTrackerRejectsRequestsAfterDrainStarts(t *testing.T) {
	f := newIdleTrackerFixture(t, 1*time.Millisecond)
	f.run(t)

	f.requireFiredWithin(t, time.Second, "idle did not fire")

	rec := serveTrackedNoContent(t, f.tracker)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	done, ok := f.tracker.BeginWork()
	assert.False(t, ok, "BeginWork after draining")
	done()
}
