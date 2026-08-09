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
	"go.kenn.io/msgvault/internal/vector/embed"
)

// fakeCmdVectorBackend satisfies vector.Backend for the init tests. It only
// implements the generation lookups the init-time freshness check performs
// (vector.ResolveActiveForFingerprint); every other method is left to the
// embedded nil interface because these tests never call them. The zero value
// reports "no active generation, none building" so the freshness check
// resolves to ErrNotEnabled and leaves the freshly-installed ready status
// untouched.
type fakeCmdVectorBackend struct {
	vector.Backend

	active    *vector.Generation
	activeErr error
}

func (b *fakeCmdVectorBackend) ActiveGeneration(context.Context) (vector.Generation, error) {
	if b.active != nil {
		return *b.active, nil
	}
	if b.activeErr != nil {
		return vector.Generation{}, b.activeErr
	}
	return vector.Generation{}, vector.ErrNoActiveGeneration
}

func (b *fakeCmdVectorBackend) BuildingGeneration(context.Context) (*vector.Generation, error) {
	// (nil, nil) is the interface's "nothing building" signal, which
	// ResolveActiveForFingerprint checks for explicitly.
	return nil, nil //nolint:nilnil // "no building generation" is a valid nil-value/nil-error result here
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

// TestVectorInitHandleWaitContextPrefersDoneWhenBothReady verifies that when
// both the init has finished and the shutdown context is already expired,
// WaitContext deterministically reports done (true). Without the preference,
// the plain select could pick the ctx case and skip CloseFeatures, leaking
// vectors.db. Iterated so a flaky random-select regression is caught.
func TestVectorInitHandleWaitContextPrefersDoneWhenBothReady(t *testing.T) {
	for range 200 {
		h := &vectorInitHandle{done: make(chan struct{})}
		close(h.done)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.True(t, h.WaitContext(ctx),
			"finished init must report done even when ctx is also ready")
	}
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

func TestStartVectorInitFlagsStaleIndex(t *testing.T) {
	c := config.NewDefaultConfig()
	c.Vector.Enabled = true
	withTestConfig(t, c)

	// Active generation's fingerprint differs from the configured one, so
	// the same check the query path runs (ResolveActiveForFingerprint)
	// reports ErrIndexStale at init completion.
	overrideSetupVectorFeatures(t, func(context.Context, *store.Store, string, bool) (*vectorFeatures, error) {
		return &vectorFeatures{
			Backend: &fakeCmdVectorBackend{
				active: &vector.Generation{ID: 1, Fingerprint: "old-model:384:c6000:e1"},
			},
			Cfg:   c.Vector,
			Close: func() error { return nil },
		}, nil
	})

	srv := newVectorInitTestServer(t)
	h := startVectorInit(context.Background(), nil, "/tmp/msgvault.db", nil, srv, scheduler.New(nil))

	require.True(t, h.WaitTimeout(5*time.Second))
	detail := waitForVectorStatus(t, srv, api.VectorStatusStale)
	assert := assert.New(t)
	assert.Contains(detail, "old-model:384:c6000:e1", "detail names the stored fingerprint")
	assert.Contains(detail, c.Vector.GenerationFingerprint(), "detail names the configured fingerprint")
	assert.Contains(detail, "msgvault embeddings build --full-rebuild", "detail names the rebuild command")
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

func TestNewSchedulerEmbedJobThreadsContextRunnerAndConvergenceChecker(t *testing.T) {
	runner := &embed.ContextWorker{}
	checker := &registeredConvergenceChecker{}
	vf := &vectorFeatures{
		Backend: &fakeCmdVectorBackend{}, Runner: runner, Convergence: checker,
		Cfg: config.NewDefaultConfig().Vector,
	}

	job := newSchedulerEmbedJob(vf, nil)
	assert.Same(t, runner, job.Worker)
	assert.Same(t, checker, job.Convergence)
}

type registeredEmbedJobCapture struct {
	job *scheduler.EmbedJob
}

func (c *registeredEmbedJobCapture) SetEmbedJob(job *scheduler.EmbedJob, _ string, _ bool) error {
	c.job = job
	return nil
}

type registeredContextRunner struct {
	runs []vector.GenerationID
}

func (r *registeredContextRunner) ReclaimStale(context.Context) (int, error) { return 0, nil }

func (r *registeredContextRunner) RunOnce(_ context.Context, gen vector.GenerationID) (embed.RunResult, error) {
	r.runs = append(r.runs, gen)
	return embed.RunResult{}, nil
}

func (r *registeredContextRunner) RunBackstop(context.Context, vector.GenerationID) (embed.RunResult, error) {
	return embed.RunResult{}, nil
}

type registeredConvergenceChecker struct {
	state scheduler.ConvergenceResult
}

func (c registeredConvergenceChecker) CheckConvergence(context.Context, vector.GenerationID) (scheduler.ConvergenceResult, error) {
	return c.state, nil
}

type registeredActivationBackend struct {
	vector.Backend

	building       *vector.Generation
	activateErr    error
	activateCalls  []vector.GenerationID
	activatedCalls []vector.GenerationID
	sequences      []int64
}

func (b *registeredActivationBackend) ActivateGenerationIfConverged(
	ctx context.Context, gen vector.GenerationID, expectedSequence int64,
) error {
	b.sequences = append(b.sequences, expectedSequence)
	return b.ActivateGeneration(ctx, gen, false)
}

func (b *registeredActivationBackend) ActiveGeneration(context.Context) (vector.Generation, error) {
	return vector.Generation{}, vector.ErrNoActiveGeneration
}

func (b *registeredActivationBackend) BuildingGeneration(context.Context) (*vector.Generation, error) {
	return b.building, nil
}

func (b *registeredActivationBackend) ActivateGeneration(_ context.Context, gen vector.GenerationID, force bool) error {
	if force {
		panic("registered daemon activation must never force")
	}
	b.activateCalls = append(b.activateCalls, gen)
	if b.activateErr != nil {
		return b.activateErr
	}
	b.activatedCalls = append(b.activatedCalls, gen)
	return nil
}

func contextualSchedulerConfig() vector.Config {
	c := config.NewDefaultConfig().Vector
	c.Embeddings.APIFormat = vector.APIFormatVoyageContextual
	c.Embeddings.Model = "voyage-context-4"
	c.Embeddings.Dimension = 4
	c.Embed.BackstopInterval = -1
	return c
}

func runRegisteredContextJob(
	t *testing.T,
	state scheduler.ConvergenceResult,
	buildingFingerprint string,
	activateErr error,
) (*registeredActivationBackend, *registeredContextRunner) {
	t.Helper()
	vectorCfg := contextualSchedulerConfig()
	testCfg := config.NewDefaultConfig()
	testCfg.Vector = vectorCfg
	withTestConfig(t, testCfg)
	if buildingFingerprint == "" {
		buildingFingerprint = vectorCfg.GenerationFingerprint()
	}
	backend := &registeredActivationBackend{
		building:    &vector.Generation{ID: 13, State: vector.GenerationBuilding, Fingerprint: buildingFingerprint},
		activateErr: activateErr,
	}
	runner := &registeredContextRunner{}
	capture := &registeredEmbedJobCapture{}
	vf := &vectorFeatures{
		Backend: backend, Runner: runner,
		Convergence: registeredConvergenceChecker{state: state}, Cfg: vectorCfg,
	}
	require.NoError(t, registerEmbedJob(capture, vf, nil))
	require.NotNil(t, capture.job)
	capture.job.Run(t.Context())
	return backend, runner
}

func TestRegisterEmbedJob_ContextualActivationRequiresEveryConvergenceDimension(t *testing.T) {
	rootAssert := assert.New(t)
	complete := scheduler.ConvergenceResult{
		MessageCoverageComplete: true,
		LatestJournalSequence:   5,
		ConsumedJournalSequence: 5,
		ReconciliationComplete:  true,
	}
	tests := []struct {
		name   string
		mutate func(*scheduler.ConvergenceResult)
	}{
		{name: "message coverage incomplete", mutate: func(s *scheduler.ConvergenceResult) {
			s.MessageCoverageComplete = false
			s.MessageCoverageMissing = 1
		}},
		{name: "journal incomplete", mutate: func(s *scheduler.ConvergenceResult) {
			s.ConsumedJournalSequence = 4
		}},
		{name: "reconciliation incomplete", mutate: func(s *scheduler.ConvergenceResult) {
			s.ReconciliationComplete = false
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := assert.New(t)
			state := complete
			tt.mutate(&state)
			backend, runner := runRegisteredContextJob(t, state, "", nil)
			check.Equal([]vector.GenerationID{13}, runner.runs)
			check.Empty(backend.activateCalls)
			check.Empty(backend.activatedCalls)
		})
	}

	backend, runner := runRegisteredContextJob(t, complete, "", nil)
	rootAssert.Equal([]vector.GenerationID{13}, runner.runs)
	rootAssert.Equal([]vector.GenerationID{13}, backend.activateCalls)
	rootAssert.Equal([]vector.GenerationID{13}, backend.activatedCalls)
	rootAssert.Equal([]int64{5}, backend.sequences)
}

func TestRegisterEmbedJob_ContextualLifecycleRefusalsNeverActivateAnotherGeneration(t *testing.T) {
	complete := scheduler.ConvergenceResult{
		MessageCoverageComplete: true,
		LatestJournalSequence:   5,
		ConsumedJournalSequence: 5,
		ReconciliationComplete:  true,
	}
	t.Run("wrong fingerprint", func(t *testing.T) {
		check := assert.New(t)
		backend, runner := runRegisteredContextJob(t, complete, "wrong-contextual-generation", nil)
		check.Empty(runner.runs)
		check.Empty(backend.activateCalls)
		check.Empty(backend.activatedCalls)
	})
	t.Run("retired during activation", func(t *testing.T) {
		check := assert.New(t)
		backend, runner := runRegisteredContextJob(t, complete, "", vector.ErrGenerationRetired)
		check.Equal([]vector.GenerationID{13}, runner.runs)
		check.Equal([]vector.GenerationID{13}, backend.activateCalls)
		check.Equal([]int64{5}, backend.sequences)
		check.Empty(backend.activatedCalls)
	})
}
