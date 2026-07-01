package cmd

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// fakeCmdVectorBackend satisfies vector.Backend without implementing any
// methods; the tests in this file never invoke backend methods, they only
// check that startVectorInit installs/propagates it.
type fakeCmdVectorBackend struct {
	vector.Backend
}

func newVectorInitTestServer(t *testing.T) *api.Server {
	t.Helper()
	return api.NewServerWithOptions(api.ServerOptions{
		Config:       &config.Config{},
		Logger:       slog.New(slog.DiscardHandler),
		VectorStatus: api.VectorStatusInitializing,
	})
}

func overrideSetupVectorFeatures(t *testing.T, fn func(context.Context, *store.Store, string, bool) (*vectorFeatures, error)) {
	t.Helper()
	prev := setupVectorFeaturesForRun
	setupVectorFeaturesForRun = fn
	t.Cleanup(func() { setupVectorFeaturesForRun = prev })
}

func waitForVectorStatus(t *testing.T, srv *api.Server, want api.VectorStatus) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, msg := srv.VectorStatus()
		if status == want {
			return msg
		}
		time.Sleep(5 * time.Millisecond)
	}
	status, _ := srv.VectorStatus()
	require.Equal(t, want, status, "vector status never reached %s", want)
	return ""
}

func TestVectorInitHandleWaitContextReturnsTrueWhenFinished(t *testing.T) {
	h := &vectorInitHandle{done: make(chan struct{})}
	close(h.done)
	assert.True(t, h.WaitContext(context.Background()),
		"finished init should report done")
}

func TestVectorInitHandleWaitContextReturnsFalseWhenCancelled(t *testing.T) {
	h := &vectorInitHandle{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.False(t, h.WaitContext(ctx),
		"a done context must stop the wait before init finishes")
}

func TestStartVectorInitDisabledFinishesImmediately(t *testing.T) {
	c := config.NewDefaultConfig()
	c.Vector.Enabled = false
	withTestConfig(t, c)

	h := startVectorInit(context.Background(), nil, "", nil, nil, nil)
	assert.True(t, h.WaitTimeout(time.Second))
}

func TestStartVectorInitInstallsFeaturesOnSuccess(t *testing.T) {
	c := config.NewDefaultConfig()
	c.Vector.Enabled = true
	withTestConfig(t, c)

	closed := false
	overrideSetupVectorFeatures(t, func(context.Context, *store.Store, string, bool) (*vectorFeatures, error) {
		return &vectorFeatures{
			Backend: &fakeCmdVectorBackend{},
			Close:   func() error { closed = true; return nil },
		}, nil
	})

	srv := newVectorInitTestServer(t)
	sched := scheduler.New(nil)
	h := startVectorInit(context.Background(), nil, "/tmp/msgvault.db", nil, srv, sched)

	require.True(t, h.WaitTimeout(5*time.Second))
	waitForVectorStatus(t, srv, api.VectorStatusReady)
	h.CloseFeatures()
	assert.True(t, closed, "CloseFeatures must close the opened backend")
}

func TestStartVectorInitReportsError(t *testing.T) {
	c := config.NewDefaultConfig()
	c.Vector.Enabled = true
	withTestConfig(t, c)

	overrideSetupVectorFeatures(t, func(context.Context, *store.Store, string, bool) (*vectorFeatures, error) {
		return nil, errors.New("migration exploded")
	})

	srv := newVectorInitTestServer(t)
	h := startVectorInit(context.Background(), nil, "/tmp/msgvault.db", nil, srv, scheduler.New(nil))

	require.True(t, h.WaitTimeout(5*time.Second))
	msg := waitForVectorStatus(t, srv, api.VectorStatusError)
	assert.Contains(t, msg, "migration exploded")
}

func TestStartVectorInitHoldsWorkTracker(t *testing.T) {
	c := config.NewDefaultConfig()
	c.Vector.Enabled = true
	withTestConfig(t, c)

	gate := api.NewSerialOperationGate()
	release := make(chan struct{})
	overrideSetupVectorFeatures(t, func(ctx context.Context, _ *store.Store, _ string, _ bool) (*vectorFeatures, error) {
		<-release
		return nil, ctx.Err()
	})

	srv := newVectorInitTestServer(t)
	h := startVectorInit(context.Background(), nil, "/tmp/msgvault.db", gate, srv, scheduler.New(nil))

	// While init runs, the gate must be held: BeginWorkContext with an
	// already-cancelled context must fail rather than acquire.
	assert.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		done, ok := gate.BeginWorkContext(ctx)
		if ok {
			done()
		}
		return !ok
	}, 2*time.Second, 10*time.Millisecond, "gate should be held during init")

	close(release)
	require.True(t, h.WaitTimeout(5*time.Second))
	done, ok := gate.BeginWork()
	require.True(t, ok, "gate must be released after init")
	done()
}

func TestStartVectorInitAbortsQuietlyOnCancel(t *testing.T) {
	c := config.NewDefaultConfig()
	c.Vector.Enabled = true
	withTestConfig(t, c)

	ctx, cancel := context.WithCancel(context.Background())
	overrideSetupVectorFeatures(t, func(ctx context.Context, _ *store.Store, _ string, _ bool) (*vectorFeatures, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	srv := newVectorInitTestServer(t)
	h := startVectorInit(ctx, nil, "/tmp/msgvault.db", nil, srv, scheduler.New(nil))
	cancel()

	require.True(t, h.WaitTimeout(5*time.Second))
	status, _ := srv.VectorStatus()
	assert.Equal(t, api.VectorStatusInitializing, status,
		"shutdown-cancelled init must not flip status to error")
}
