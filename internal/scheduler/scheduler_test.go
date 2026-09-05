package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

func TestNew(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	require.NotNil(t, s, "New()")
	assert.NotNil(t, s.cron, "cron")
	assert.NotNil(t, s.jobs, "jobs map")
}

func TestAddAccount(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	// Valid cron expression
	require.NoError(t, s.AddAccount("test@gmail.com", "0 2 * * *"), "AddAccount() with valid cron")

	// Check job was added
	s.mu.RLock()
	_, exists := s.jobs["test@gmail.com"]
	s.mu.RUnlock()

	assert.True(t, exists, "job was not added to jobs map")
}

func TestAddAccountInvalidCron(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	err := s.AddAccount("test@gmail.com", "invalid cron")
	assert.Error(t, err, "AddAccount() with invalid cron")
}

func TestAddAccountReplacesExisting(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	// Add initial schedule
	require.NoError(t, s.AddAccount("test@gmail.com", "0 2 * * *"), "AddAccount()")

	s.mu.RLock()
	firstID := s.jobs["test@gmail.com"]
	s.mu.RUnlock()

	// Replace with new schedule
	require.NoError(t, s.AddAccount("test@gmail.com", "0 3 * * *"), "AddAccount() replacement")

	s.mu.RLock()
	secondID := s.jobs["test@gmail.com"]
	s.mu.RUnlock()

	assert.NotEqual(t, firstID, secondID, "job ID was not updated after replacement")
}

func TestRemoveAccount(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	require.NoError(t, s.AddAccount("test@gmail.com", "0 2 * * *"), "AddAccount")
	s.RemoveAccount("test@gmail.com")

	s.mu.RLock()
	_, exists := s.jobs["test@gmail.com"]
	s.mu.RUnlock()

	assert.False(t, exists, "job still exists after RemoveAccount()")
}

func TestRemoveAccountNonExistent(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	// Should not panic
	s.RemoveAccount("nonexistent@gmail.com")
}

func TestAddAccountsFromConfig(t *testing.T) {
	assert := assert.New(t)
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	cfg := &config.Config{
		Accounts: []config.AccountSchedule{
			{Email: "user1@gmail.com", Schedule: "0 1 * * *", Enabled: true},
			{Email: "user2@gmail.com", Schedule: "0 2 * * *", Enabled: true},
			{Email: "disabled@gmail.com", Schedule: "0 3 * * *", Enabled: false},
			{Email: "noschedule@gmail.com", Schedule: "", Enabled: true},
		},
	}

	scheduled, errs := s.AddAccountsFromConfig(cfg)

	assert.Empty(errs, "AddAccountsFromConfig() errors")
	assert.Equal(2, scheduled, "AddAccountsFromConfig() scheduled")

	// Check only enabled accounts with schedules were added
	s.mu.RLock()
	defer s.mu.RUnlock()

	assert.Contains(s.jobs, "user1@gmail.com", "user1@gmail.com should be scheduled")
	assert.Contains(s.jobs, "user2@gmail.com", "user2@gmail.com should be scheduled")
	assert.NotContains(s.jobs, "disabled@gmail.com", "disabled@gmail.com should not be scheduled")
	assert.NotContains(s.jobs, "noschedule@gmail.com", "noschedule@gmail.com should not be scheduled")
}

func TestAddAccountsFromConfigWithErrors(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	cfg := &config.Config{
		Accounts: []config.AccountSchedule{
			{Email: "valid@gmail.com", Schedule: "0 1 * * *", Enabled: true},
			{Email: "invalid@gmail.com", Schedule: "not a cron", Enabled: true},
		},
	}

	scheduled, errs := s.AddAccountsFromConfig(cfg)

	assert.Equal(t, 1, scheduled, "scheduled")
	assert.Len(t, errs, 1, "errs")
}

func TestSchedulerGenericJobStatus(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var ran int
	s := New(func(context.Context, string) error { return nil })
	err := s.AddJob(Job{
		Name:     "synctech-sms:pixel",
		Schedule: "30 4 * * *",
		Run: func(ctx context.Context) error {
			ran++
			return nil
		},
	})
	require.NoError(err, "AddJob")
	require.True(s.IsJobScheduled("synctech-sms:pixel"), "job not scheduled")
	require.NoError(s.TriggerJob("synctech-sms:pixel"), "TriggerJob")
	assert.Equal(1, ran, "ran")
	status := s.JobStatus()
	require.Len(status, 1, "status")
	assert.Equal("synctech-sms:pixel", status[0].Name, "status[0].Name")
	assert.Equal("30 4 * * *", status[0].Schedule, "status[0].Schedule")
}

// TestStartJobIsAsync proves StartJob returns before the job body finishes,
// unlike TriggerJob which blocks for the job's full duration. This is the
// property handleTriggerSync depends on to avoid self-deadlocking on the
// daemon's operation gate: an HTTP handler holding that gate must be able to
// return (and release the gate) before the job's beginWork tries to acquire
// it, which only happens if the job runs in its own goroutine.
func TestStartJobIsAsync(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	started := make(chan struct{})
	release := make(chan struct{})
	var ran atomic.Int32
	s := New(func(context.Context, string) error { return nil })
	require.NoError(s.AddJob(Job{
		Name:     "granola:default",
		Schedule: "0 0 1 1 *",
		Run: func(context.Context) error {
			close(started)
			<-release
			ran.Add(1)
			return nil
		},
	}), "AddJob")

	returned := make(chan error, 1)
	go func() { returned <- s.StartJob("granola:default") }()

	select {
	case err := <-returned:
		require.NoError(err, "StartJob")
	case <-time.After(time.Second):
		require.FailNow("StartJob did not return; it is blocking like TriggerJob")
	}

	// StartJob returned, but the job body must still be blocked in Run,
	// proving the return happened before (not because of) job completion.
	select {
	case <-started:
	case <-time.After(time.Second):
		require.FailNow("job never started")
	}
	assert.Equal(int32(0), ran.Load(), "job body must still be blocked after StartJob returns")

	status := s.JobStatus()
	require.Len(status, 1)
	assert.True(status[0].Running, "job should be recorded as running while blocked")
	assert.True(s.IsJobScheduled("granola:default"), "job remains scheduled while running")

	// A second StartJob while the first is still running must be a no-op:
	// no error, and it must not spawn a second concurrent run.
	require.NoError(s.StartJob("granola:default"), "second StartJob while running")

	close(release)
	require.Eventually(func() bool {
		return !s.JobStatus()[0].Running
	}, time.Second, time.Millisecond, "job finishes after release")
	assert.Equal(int32(1), ran.Load(), "job body must run exactly once")
}

func TestStartStop(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	s.Start()
	ctx := s.Stop()

	// Wait for stop
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		assert.Fail(t, "Stop() did not complete in time")
	}
}

func TestIsRunning(t *testing.T) {
	assert := assert.New(t)
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	// Not running before Start
	assert.False(s.IsRunning(), "IsRunning() before Start()")

	s.Start()

	// Running after Start
	assert.True(s.IsRunning(), "IsRunning() after Start()")

	ctx := s.Stop()

	// Not running after Stop
	assert.False(s.IsRunning(), "IsRunning() after Stop()")

	// Wait for stop
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		assert.Fail("Stop() did not complete in time")
	}
}

func TestStopCancelsRunningSync(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	syncStarted := make(chan struct{})
	s := New(func(ctx context.Context, email string) error {
		close(syncStarted)
		<-ctx.Done()
		return ctx.Err()
	})

	require.NoError(s.AddAccount("test@gmail.com", "0 0 1 1 *"), "AddAccount")

	// Trigger sync
	require.NoError(s.TriggerSync("test@gmail.com"), "TriggerSync")

	// Wait for sync to start
	select {
	case <-syncStarted:
	case <-time.After(time.Second):
		require.Fail("sync did not start")
	}

	// Stop should cancel the running sync
	ctx := s.Stop()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		assert.Fail("Stop() did not complete after cancelling sync")
	}

	// Verify the error was recorded
	statuses := s.Status()
	for _, status := range statuses {
		if status.Email == "test@gmail.com" {
			assert.NotEmpty(status.LastError, "expected error after cancelled sync")
			return
		}
	}
}

func TestTriggerSync(t *testing.T) {
	require := require.
		New(t)

	assert := assert.New(t)
	var called atomic.Int32
	s := New(func(ctx context.Context, email string) error {
		called.Add(1)
		time.Sleep(50 * time.Millisecond)
		return nil
	})
	require.NoError(
		s.AddAccount("test@gmail.com", "0 0 1 1 *"), "AddAccount")

	// Trigger manually
	err := s.TriggerSync("test@gmail.com")
	require.NoError(
		err, "TriggerSync()")

	// Wait for sync to start
	time.Sleep(10 * time.Millisecond)

	// Second trigger should fail (already running)
	err = s.TriggerSync("test@gmail.com")
	require.Error(err, "TriggerSync() while running")

	// Wait for completion
	time.Sleep(100 * time.Millisecond)

	assert.Equal(int32(1), called.Load(), "syncFunc called times")
}

func TestScheduler_WorkTrackerWrapsTriggeredSync(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tracker := &fakeWorkTracker{}
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	s := New(func(ctx context.Context, email string) error {
		startedOnce.Do(func() { close(started) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}).WithWorkTracker(tracker)

	require.NoError(s.AddAccount("test@gmail.com", "0 0 1 1 *"), "AddAccount")
	s.Start()
	defer func() {
		ctx := s.Stop()
		<-ctx.Done()
	}()

	require.NoError(s.TriggerSync("test@gmail.com"), "TriggerSync")

	select {
	case <-started:
	case <-time.After(time.Second):
		require.FailNow("sync did not start")
	}
	assert.Equal(1, tracker.active(), "active work while sync runs")

	close(release)
	require.Eventually(func() bool {
		return tracker.active() == 0
	}, time.Second, time.Millisecond, "active work after sync exits")
}

type blockingContextWorkTracker struct {
	begin   chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingContextWorkTracker() *blockingContextWorkTracker {
	return &blockingContextWorkTracker{
		begin:   make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (t *blockingContextWorkTracker) signalBegin() {
	t.once.Do(func() { close(t.begin) })
}

func (t *blockingContextWorkTracker) BeginWork() (func(), bool) {
	t.signalBegin()
	<-t.release
	return func() {}, false
}

func (t *blockingContextWorkTracker) BeginWorkContext(ctx context.Context) (func(), bool) {
	t.signalBegin()
	select {
	case <-t.release:
		return func() {}, false
	case <-ctx.Done():
		return func() {}, false
	}
}

func TestSchedulerStopCancelsWorkTrackerWait(t *testing.T) {
	require := require.
		New(t)

	tracker := newBlockingContextWorkTracker()
	s := New(func(context.Context, string) error {
		require.FailNow("sync function must not run when gate wait is canceled")
		return nil
	}).WithWorkTracker(tracker)
	require.NoError(
		s.AddAccount("test@example.com", "* * * * *"), "AddAccount")

	require.NoError(
		s.TriggerSync("test@example.com"), "TriggerSync")

	select {
	case <-tracker.begin:
	case <-time.After(500 * time.Millisecond):
		require.FailNow("sync did not start waiting on tracker")
	}

	stopCtx := s.Stop()
	select {
	case <-stopCtx.Done():
	case <-time.After(500 * time.Millisecond):
		close(tracker.release)
		require.FailNow("Stop did not cancel work tracker wait")
	}
}

func TestSyncPreventsDoubleRun(t *testing.T) {
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32

	s := New(func(ctx context.Context, email string) error {
		c := concurrent.Add(1)
		if c > maxConcurrent.Load() {
			maxConcurrent.Store(c)
		}
		time.Sleep(50 * time.Millisecond)
		concurrent.Add(-1)
		return nil
	})

	require.NoError(t, s.AddAccount("test@gmail.com", "0 0 1 1 *"), "AddAccount")

	// Try to trigger multiple times concurrently
	for range 5 {
		_ = s.TriggerSync("test@gmail.com")
	}

	time.Sleep(200 * time.Millisecond)

	assert.LessOrEqual(t, maxConcurrent.Load(), int32(1), "max concurrent")
}

func TestStatus(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	require.NoError(s.AddAccount("test@gmail.com", "0 2 * * *"), "AddAccount")
	require.NoError(s.AddAccount("other@gmail.com", "0 3 * * *"), "AddAccount")
	s.Start()
	defer s.Stop()

	statuses := s.Status()

	assert.Len(statuses, 2, "Status()")

	// Find test@gmail.com status
	var found bool
	for _, status := range statuses {
		if status.Email == "test@gmail.com" {
			found = true
			assert.False(status.Running, "status.Running")
			assert.False(status.NextRun.IsZero(), "status.NextRun is zero")
			break
		}
	}
	assert.True(found, "test@gmail.com not found in status")
}

func TestStatusAfterSyncSuccess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	require.NoError(s.AddAccount("test@gmail.com", "0 0 1 1 *"), "AddAccount")
	require.NoError(s.TriggerSync("test@gmail.com"), "TriggerSync")

	time.Sleep(50 * time.Millisecond)

	statuses := s.Status()
	for _, status := range statuses {
		if status.Email == "test@gmail.com" {
			assert.False(status.LastRun.IsZero(), "LastRun should be set after successful sync")
			assert.Empty(status.LastError, "LastError")
			return
		}
	}
	assert.Fail("test@gmail.com not found in status")
}

func TestStatusAfterSyncError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s := New(func(ctx context.Context, email string) error {
		return errors.New("sync failed")
	})

	require.NoError(s.AddAccount("test@gmail.com", "0 0 1 1 *"), "AddAccount")
	require.NoError(s.TriggerSync("test@gmail.com"), "TriggerSync")

	time.Sleep(50 * time.Millisecond)

	statuses := s.Status()
	for _, status := range statuses {
		if status.Email == "test@gmail.com" {
			assert.NotEmpty(status.LastError, "LastError should be set after failed sync")
			return
		}
	}
	assert.Fail("test@gmail.com not found in status")
}

func TestTriggerSyncAfterStop(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	require.NoError(t, s.AddAccount("test@gmail.com", "0 0 1 1 *"), "AddAccount")

	ctx := s.Stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		require.Fail(t, "Stop() did not complete in time")
	}

	err := s.TriggerSync("test@gmail.com")
	assert.Error(t, err, "TriggerSync() after Stop()")
}

type fakeWorkTracker struct {
	mu         sync.Mutex
	activeWork int
}

func (t *fakeWorkTracker) BeginWork() (func(), bool) {
	return t.BeginWorkContext(context.Background())
}

func (t *fakeWorkTracker) BeginWorkContext(ctx context.Context) (func(), bool) {
	if ctx != nil && ctx.Err() != nil {
		return func() {}, false
	}
	t.mu.Lock()
	t.activeWork++
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			if t.activeWork > 0 {
				t.activeWork--
			}
			t.mu.Unlock()
		})
	}, true
}

func (t *fakeWorkTracker) active() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.activeWork
}

// ---------- fakes for EmbedJob tests ----------

// fakeBackend implements vector.Backend. Only ActiveGeneration,
// BuildingGeneration, and ActivateGeneration are meaningfully populated;
// the rest panic to catch accidental usage.
type fakeBackend struct {
	active        vector.Generation
	activeErr     error
	building      *vector.Generation
	buildErr      error
	yieldActive   context.CancelCauseFunc
	yieldBuilding context.CancelCauseFunc
	// activateErr is what ActivateGeneration returns. activateCalls
	// records the gen IDs the EmbedJob asked to activate.
	activateErr       error
	yieldActivate     context.CancelCauseFunc
	mu                sync.Mutex
	activateCallIDs   []vector.GenerationID
	activateSequences []int64

	activeCalls   atomic.Int32
	buildingCalls atomic.Int32
}

func (f *fakeBackend) ActiveGeneration(ctx context.Context) (vector.Generation, error) {
	f.activeCalls.Add(1)
	if f.yieldActive != nil {
		f.yieldActive(jobctx.ErrYieldedToWaiter)
	}
	return f.active, f.activeErr
}

func (f *fakeBackend) BuildingGeneration(ctx context.Context) (*vector.Generation, error) {
	f.buildingCalls.Add(1)
	if f.yieldBuilding != nil {
		f.yieldBuilding(jobctx.ErrYieldedToWaiter)
	}
	return f.building, f.buildErr
}

func (f *fakeBackend) CreateGeneration(ctx context.Context, model string, dim int, fp string) (vector.GenerationID, error) {
	panic("unexpected: CreateGeneration")
}
func (f *fakeBackend) ActivateGeneration(ctx context.Context, gen vector.GenerationID, _ bool) error {
	f.mu.Lock()
	f.activateCallIDs = append(f.activateCallIDs, gen)
	f.mu.Unlock()
	if f.yieldActivate != nil {
		f.yieldActivate(jobctx.ErrYieldedToWaiter)
	}
	return f.activateErr
}
func (f *fakeBackend) ActivateGenerationIfConverged(ctx context.Context, gen vector.GenerationID, expectedSequence int64) error {
	f.mu.Lock()
	f.activateSequences = append(f.activateSequences, expectedSequence)
	f.mu.Unlock()
	return f.ActivateGeneration(ctx, gen, false)
}
func (f *fakeBackend) activations() []vector.GenerationID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]vector.GenerationID(nil), f.activateCallIDs...)
}
func (f *fakeBackend) RetireGeneration(ctx context.Context, gen vector.GenerationID, _ bool) error {
	panic("unexpected: RetireGeneration")
}
func (f *fakeBackend) Upsert(ctx context.Context, gen vector.GenerationID, chunks []vector.Chunk) error {
	panic("unexpected: Upsert")
}
func (f *fakeBackend) Search(ctx context.Context, gen vector.GenerationID, q []float32, k int, fl vector.Filter) ([]vector.Hit, error) {
	panic("unexpected: Search")
}
func (f *fakeBackend) Delete(ctx context.Context, gen vector.GenerationID, ids []int64) error {
	panic("unexpected: Delete")
}
func (f *fakeBackend) Stats(ctx context.Context, gen vector.GenerationID) (vector.Stats, error) {
	panic("unexpected: Stats")
}
func (f *fakeBackend) LoadVector(ctx context.Context, messageID int64) ([]float32, error) {
	panic("unexpected: LoadVector")
}
func (f *fakeBackend) ResetWatermarkBelow(ctx context.Context, minID int64) error {
	panic("unexpected: ResetWatermarkBelow")
}
func (f *fakeBackend) EmbeddedMessageCount(ctx context.Context, gen vector.GenerationID) (int64, error) {
	panic("unexpected: EmbeddedMessageCount")
}
func (f *fakeBackend) Close() error { return nil }

// fakeRunner records calls to satisfy EmbedRunner.
type fakeRunner struct {
	mu             sync.Mutex
	reclaimErr     error
	reclaimCalls   int
	runErr         error
	runCalls       int
	lastRunGen     vector.GenerationID
	runOnceResult  embed.RunResult
	backstopErr    error
	backstopCalls  int
	lastBackstop   vector.GenerationID
	backstopResult embed.RunResult
	runDoneOnce    sync.Once
	runDone        chan struct{} // optional: closed after first RunOnce
	onRunOnce      func(vector.GenerationID)
	yieldCancel    context.CancelCauseFunc
	yieldBackstop  context.CancelCauseFunc
	yieldReclaim   context.CancelCauseFunc
	personErr      error
	personCalls    []vector.GenerationID
	onPerson       func(vector.GenerationID)
	runScopes      []operations.PassScope
	backstopScopes []operations.PassScope
	personScopes   []operations.PassScope
	// onBackstop, if set, is invoked from RunBackstop (after recording the
	// call) to let tests model a side effect of the backstop pass, e.g. a
	// straggler becoming covered. Called while r.mu is held.
	onBackstop func()
}

func (r *fakeRunner) RunPersonsOnce(
	_ context.Context, gen vector.GenerationID, scope operations.PassScope,
) (embed.RunResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.personCalls = append(r.personCalls, gen)
	r.personScopes = append(r.personScopes, scope)
	if r.onPerson != nil && r.personErr == nil {
		r.onPerson(gen)
	}
	return embed.RunResult{}, r.personErr
}

func (r *fakeRunner) ReclaimStale(ctx context.Context) (int, error) {
	r.mu.Lock()
	r.reclaimCalls++
	reclaimErr := r.reclaimErr
	yieldCancel := r.yieldReclaim
	r.mu.Unlock()
	if yieldCancel != nil {
		yieldCancel(jobctx.ErrYieldedToWaiter)
	}
	return 0, reclaimErr
}

func (r *fakeRunner) RunOnce(
	ctx context.Context, gen vector.GenerationID, scope operations.PassScope,
) (embed.RunResult, error) {
	r.mu.Lock()
	r.runCalls++
	r.lastRunGen = gen
	r.runScopes = append(r.runScopes, scope)
	if r.onRunOnce != nil && r.runErr == nil {
		r.onRunOnce(gen)
	}
	ch := r.runDone
	res := r.runOnceResult
	err := r.runErr
	r.mu.Unlock()
	if r.yieldCancel != nil {
		r.yieldCancel(jobctx.ErrYieldedToWaiter)
	}
	if ch != nil {
		r.runDoneOnce.Do(func() { close(ch) })
	}
	return res, err
}

func (r *fakeRunner) RunBackstop(
	ctx context.Context, gen vector.GenerationID, scope operations.PassScope,
) (embed.RunResult, error) {
	r.mu.Lock()
	r.backstopCalls++
	r.lastBackstop = gen
	r.backstopScopes = append(r.backstopScopes, scope)
	if r.onBackstop != nil && r.backstopErr == nil {
		r.onBackstop()
	}
	backstopErr := r.backstopErr
	backstopResult := r.backstopResult
	yieldCancel := r.yieldBackstop
	r.mu.Unlock()
	if yieldCancel != nil {
		yieldCancel(jobctx.ErrYieldedToWaiter)
	}
	return backstopResult, backstopErr
}

func (r *fakeRunner) calls() (reclaim, run int, lastGen vector.GenerationID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reclaimCalls, r.runCalls, r.lastRunGen
}

func (r *fakeRunner) backstops() (n int, lastGen vector.GenerationID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.backstopCalls, r.lastBackstop
}

func (r *fakeRunner) persons() []vector.GenerationID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]vector.GenerationID(nil), r.personCalls...)
}

func (r *fakeRunner) passScopes() (run, backstop []operations.PassScope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]operations.PassScope(nil), r.runScopes...),
		append([]operations.PassScope(nil), r.backstopScopes...)
}

// ---------- EmbedJob tests ----------

func TestEmbedJob_Run_YieldedCleanRunStopsBeforeCompletionWork(t *testing.T) {
	assert := assert.New(t)
	building := &vector.Generation{ID: 77, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{activeErr: vector.ErrNoActiveGeneration, building: building}
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	runner := &fakeRunner{yieldCancel: cancel}
	coverage := &fakeCoverage{}
	var logs bytes.Buffer
	job := &EmbedJob{
		Worker:      runner,
		Backend:     backend,
		Store:       coverage,
		Fingerprint: "m:768",
		Log:         slog.New(slog.NewTextHandler(&logs, nil)),
	}

	job.Run(ctx)

	_, run, _ := runner.calls()
	assert.Equal(1, run, "RunOnce calls")
	n, _ := runner.backstops()
	assert.Equal(0, n, "RunBackstop calls")
	assert.Equal(0, coverage.calls, "coverage queries")
	assert.Empty(backend.activations(), "activation calls")
	assert.NotContains(logs.String(), "embed run complete", "yield must not log completion")
}

func TestEmbedJob_Run_YieldedReclaimStopsWithoutWarning(t *testing.T) {
	assert := assert.New(t)
	backend := &fakeBackend{active: vector.Generation{ID: 5, State: vector.GenerationActive}}
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	runner := &fakeRunner{
		reclaimErr:   errors.New("reclaim operation failed"),
		yieldReclaim: cancel,
	}
	var logs bytes.Buffer
	job := &EmbedJob{
		Worker:  runner,
		Backend: backend,
		Log:     slog.New(slog.NewTextHandler(&logs, nil)),
	}

	job.Run(ctx)

	_, run, _ := runner.calls()
	assert.Equal(0, run, "RunOnce calls after yielded reclaim")
	assert.NotContains(logs.String(), "embed reclaim failed", "yield must not log a reclaim warning")
}

func TestEmbedJob_Run_YieldedBuildingLookupStopsWithoutWarning(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	runner := &fakeRunner{}
	backend := &fakeBackend{
		buildErr:      errors.New("building lookup failed"),
		yieldBuilding: cancel,
	}
	var logs bytes.Buffer
	job := &EmbedJob{
		Worker:  runner,
		Backend: backend,
		Log:     slog.New(slog.NewTextHandler(&logs, nil)),
	}

	job.Run(ctx)

	_, run, _ := runner.calls()
	assert.Equal(0, run, "RunOnce calls after yielded building lookup")
	assert.NotContains(logs.String(), "building generation lookup failed", "yield must not log a lookup warning")
}

func TestEmbedJob_Run_YieldedActiveLookupStopsWithoutWarning(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	runner := &fakeRunner{}
	backend := &fakeBackend{
		activeErr:   errors.New("active lookup failed"),
		yieldActive: cancel,
	}
	var logs bytes.Buffer
	job := &EmbedJob{
		Worker:  runner,
		Backend: backend,
		Log:     slog.New(slog.NewTextHandler(&logs, nil)),
	}

	job.Run(ctx)

	_, run, _ := runner.calls()
	assert.Equal(0, run, "RunOnce calls after yielded active lookup")
	assert.NotContains(logs.String(), "active generation lookup failed", "yield must not log a lookup warning")
}

func TestEmbedJob_Run_YieldedBackstopStopsBeforeActivationWork(t *testing.T) {
	assert := assert.New(t)
	building := &vector.Generation{ID: 77, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{activeErr: vector.ErrNoActiveGeneration, building: building}
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	runner := &fakeRunner{yieldBackstop: cancel}
	coverage := &fakeCoverage{}
	var logs bytes.Buffer
	job := &EmbedJob{
		Worker:      runner,
		Backend:     backend,
		Store:       coverage,
		Fingerprint: "m:768",
		Log:         slog.New(slog.NewTextHandler(&logs, nil)),
	}

	job.Run(ctx)

	_, run, _ := runner.calls()
	assert.Equal(1, run, "RunOnce calls")
	n, _ := runner.backstops()
	assert.Equal(1, n, "RunBackstop calls")
	assert.Equal(0, coverage.calls, "coverage queries after yielded backstop")
	assert.Empty(backend.activations(), "activation calls after yielded backstop")
	assert.Contains(logs.String(), "embed run complete", "RunOnce completion is logged before backstop")
	assert.NotContains(logs.String(), "embed backstop failed", "yield is not a backstop failure")
	assert.NotContains(logs.String(), "building generation activated", "yield must stop activation")
}

func TestEmbedJob_Run_YieldedCoverageStopsBeforeActivation(t *testing.T) {
	assert := assert.New(t)
	building := &vector.Generation{ID: 77, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{activeErr: vector.ErrNoActiveGeneration, building: building}
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	coverage := &fakeCoverage{missing: 0, yieldCancel: cancel}
	var logs bytes.Buffer
	job := &EmbedJob{
		Worker:      &fakeRunner{},
		Backend:     backend,
		Store:       coverage,
		Fingerprint: "m:768",
		Log:         slog.New(slog.NewTextHandler(&logs, nil)),
	}

	job.Run(ctx)

	assert.Equal(1, coverage.calls, "coverage queries")
	assert.Empty(backend.activations(), "activation calls after yielded coverage")
	assert.NotContains(logs.String(), "embed: building generation activated", "yield must stop activation logging")
}

func TestEmbedJob_Run_YieldedActivationStopsWithoutWarningOrCompletion(t *testing.T) {
	for _, test := range []struct {
		name        string
		activateErr error
	}{
		{name: "error", activateErr: errors.New("activation operation failed")},
		{name: "success"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			building := &vector.Generation{ID: 77, State: vector.GenerationBuilding, Fingerprint: "m:768"}
			ctx, cancel := context.WithCancelCause(context.Background())
			t.Cleanup(func() { cancel(nil) })
			backend := &fakeBackend{
				activeErr:     vector.ErrNoActiveGeneration,
				building:      building,
				activateErr:   test.activateErr,
				yieldActivate: cancel,
			}
			coverage := &fakeCoverage{missing: 0}
			var logs bytes.Buffer
			job := &EmbedJob{
				Worker:      &fakeRunner{},
				Backend:     backend,
				Store:       coverage,
				Fingerprint: "m:768",
				Log:         slog.New(slog.NewTextHandler(&logs, nil)),
			}

			job.Run(ctx)

			assert.Equal([]vector.GenerationID{77}, backend.activations(), "activation calls")
			assert.NotContains(logs.String(), "embed: activation failed", "yield must not log activation failure")
			assert.NotContains(logs.String(), "embed: building generation activated", "yield must not log activation completion")
		})
	}
}

func TestEmbedJob_Run_YieldedRunErrorStopsWithoutFailureWork(t *testing.T) {
	assert := assert.New(t)
	building := &vector.Generation{ID: 77, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{activeErr: vector.ErrNoActiveGeneration, building: building}
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	runner := &fakeRunner{
		runErr:      errors.New("operation failed"),
		yieldCancel: cancel,
	}
	coverage := &fakeCoverage{}
	var logs bytes.Buffer
	job := &EmbedJob{
		Worker:      runner,
		Backend:     backend,
		Store:       coverage,
		Fingerprint: "m:768",
		Log:         slog.New(slog.NewTextHandler(&logs, nil)),
	}

	job.Run(ctx)

	_, run, _ := runner.calls()
	assert.Equal(1, run, "RunOnce calls")
	n, _ := runner.backstops()
	assert.Equal(0, n, "RunBackstop calls")
	assert.Equal(0, coverage.calls, "coverage queries")
	assert.Empty(backend.activations(), "activation calls")
	assert.NotContains(logs.String(), "embed run failed", "yield must not log a failure")
	assert.NotContains(logs.String(), "embed run complete", "yield must not log completion")
}

func TestEmbedJob_Run_ErrorWithoutYieldLogsFailure(t *testing.T) {
	assert := assert.New(t)
	backend := &fakeBackend{active: vector.Generation{ID: 5, State: vector.GenerationActive}}
	runner := &fakeRunner{runErr: errors.New("operation failed")}
	var logs bytes.Buffer
	job := &EmbedJob{
		Worker:  runner,
		Backend: backend,
		Log:     slog.New(slog.NewTextHandler(&logs, nil)),
	}

	job.Run(context.Background())

	assert.Contains(logs.String(), "embed run failed", "ordinary errors must log a failure")
	assert.NotContains(logs.String(), "embed run complete", "failed runs must not log completion")
}

func TestEmbedJob_Run_PersonFailureStillRunsMessageBackstop(t *testing.T) {
	assert := assert.New(t)
	backend := &fakeBackend{active: vector.Generation{ID: 5, State: vector.GenerationActive}}
	runner := &fakeRunner{runErr: &embed.GenerationRunError{Person: errors.New("person provider failed")}}
	var logs bytes.Buffer
	job := &EmbedJob{
		Worker: runner, Backend: backend, BackstopInterval: time.Hour,
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
	}

	job.Run(t.Context())

	n, gen := runner.backstops()
	assert.Equal(1, n, "person-only failure must not starve message recovery")
	assert.Equal(vector.GenerationID(5), gen)
	assert.Contains(logs.String(), "person embedding run failed")
	assert.NotContains(logs.String(), "embed run failed")
}

func TestEmbedJob_MaybeRunBackstop_YieldedCleanRunDoesNotRecordCompletion(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	runner := &fakeRunner{yieldBackstop: cancel}
	now := time.Now()
	job := &EmbedJob{
		Worker:           runner,
		BackstopInterval: time.Hour,
		Now:              func() time.Time { return now },
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	const gen = vector.GenerationID(5)

	job.maybeRunBackstop(ctx, gen, logger, time.Now().UTC())
	job.maybeRunBackstop(ctx, gen, logger, time.Now().UTC())

	assert.NotContains(logs.String(), "embed backstop complete", "yield must not log completion")
	n, _ := runner.backstops()
	assert.Equal(2, n, "yield must not throttle a retry")
	_, recorded := job.lastBackstop[gen]
	assert.False(recorded, "yield must not record lastBackstop")
}

func TestEmbedJob_Run_ActiveGeneration(t *testing.T) {
	assert := assert.New(t)
	backend := &fakeBackend{active: vector.Generation{ID: 5, State: vector.GenerationActive}}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	job.Run(context.Background())

	reclaim, run, gen := runner.calls()
	assert.Equal(1, reclaim, "ReclaimStale calls")
	assert.Equal(1, run, "RunOnce calls")
	assert.Equal(vector.GenerationID(5), gen, "RunOnce gen")
	// New precedence: BuildingGeneration is consulted first; with no
	// building present we then fall through to active. Activation
	// must NOT fire for the active gen.
	assert.Empty(backend.activations(), "ActivateGeneration calls (target was active)")
}

func TestEmbedJob_Run_ActiveGenerationFingerprintMismatch(t *testing.T) {
	// An active generation whose fingerprint differs from the configured
	// one means the operator changed model, dimension, or preprocessing
	// policy without running --full-rebuild. Topping it up would let the
	// daemon embed new messages under the current policy into an index
	// whose existing vectors used a different policy — silently mixing
	// two embedding spaces in one generation. pickTarget must refuse,
	// the same way it refuses a mismatched in-flight build.
	backend := &fakeBackend{
		active: vector.Generation{
			ID: 7, State: vector.GenerationActive, Fingerprint: "old-model:768:p1-111111",
		},
	}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend, Fingerprint: "new-model:768:p1-111111"}

	job.Run(context.Background())

	_, run, _ := runner.calls()
	assert.Equal(t, 0, run, "RunOnce calls (refuse to top up mismatched active)")
	assert.Empty(t, backend.activations(), "ActivateGeneration calls")
}

func TestEmbedJob_Run_ActiveGenerationFingerprintMatch(t *testing.T) {
	// Counterpart of the mismatch test: when the active fingerprint
	// matches config exactly, the daemon must continue to top it up.
	backend := &fakeBackend{
		active: vector.Generation{
			ID: 9, State: vector.GenerationActive, Fingerprint: "m:768:p1-111111",
		},
	}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend, Fingerprint: "m:768:p1-111111"}

	job.Run(context.Background())

	_, run, gen := runner.calls()
	assert.Equal(t, 1, run, "RunOnce calls (matching active should top up)")
	assert.Equal(t, vector.GenerationID(9), gen, "RunOnce gen")
}

func TestEmbedJob_Run_BuildingRefusedWithoutFingerprint(t *testing.T) {
	// A daemon with no configured Fingerprint cannot tell whether a
	// building generation matches the model it is supposed to be
	// using; draining (and thus auto-activating) it could silently
	// swap the production index to a different model. pickTarget
	// must refuse, leaving the build for the CLI to resolve.
	building := &vector.Generation{ID: 7, State: vector.GenerationBuilding, Fingerprint: "old-model:512"}
	backend := &fakeBackend{
		activeErr: vector.ErrNoActiveGeneration,
		building:  building,
	}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend} // Fingerprint left empty

	job.Run(context.Background())

	_, run, _ := runner.calls()
	assert.Equal(t, 0, run, "RunOnce calls (refuse to drain without fingerprint)")
	assert.Empty(t, backend.activations(), "ActivateGeneration calls")
}

func TestEmbedJob_Run_NothingToDo(t *testing.T) {
	backend := &fakeBackend{
		activeErr: vector.ErrNoActiveGeneration,
		building:  nil,
	}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	job.Run(context.Background())

	_, run, _ := runner.calls()
	assert.Equal(t, 0, run, "RunOnce calls (nothing to do)")
}

func TestEmbedJob_Run_ReclaimStaleFailureContinues(t *testing.T) {
	backend := &fakeBackend{active: vector.Generation{ID: 3}}
	runner := &fakeRunner{reclaimErr: errors.New("boom")}
	job := &EmbedJob{Worker: runner, Backend: backend}

	job.Run(context.Background())

	_, run, gen := runner.calls()
	assert.Equal(t, 1, run, "RunOnce calls (should proceed despite reclaim error)")
	assert.Equal(t, vector.GenerationID(3), gen, "RunOnce gen")
}

func TestEmbedJob_Run_ActiveGenerationError(t *testing.T) {
	backend := &fakeBackend{activeErr: errors.New("db failure")}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	job.Run(context.Background())

	_, run, _ := runner.calls()
	assert.Equal(t, 0, run, "RunOnce calls on active lookup error")
}

// TestEmbedJob_Run_PrefersBuildingOverActive regresses the daemon
// equivalent of the CLI's pickEmbedGeneration precedence bug. With
// both an active generation AND a matching building generation
// present (the typical "operator just kicked off --full-rebuild"
// state), the daemon must drain the building so it can later
// activate, NOT keep topping up the old active forever.
func TestEmbedJob_Run_PrefersBuildingOverActive(t *testing.T) {
	building := &vector.Generation{ID: 99, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{
		active:   vector.Generation{ID: 5, State: vector.GenerationActive, Fingerprint: "m:768"},
		building: building,
	}
	// No Store wired, so the activation gate skips auto-activation; we're
	// only asserting target selection here.
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend, Fingerprint: "m:768"}

	job.Run(context.Background())

	_, _, gen := runner.calls()
	assert.Equal(t, vector.GenerationID(99), gen,
		"RunOnce gen should be building (%d) — active (%d) would strand the rebuild",
		building.ID, backend.active.ID)
}

// TestEmbedJobRunMaintainsActivePeopleThroughoutCompatibleBuild catches the
// scheduler dropping active-generation person creations, edits, and deletions
// whenever a compatible message rebuild is in flight.
func TestEmbedJobRunMaintainsActivePeopleThroughoutCompatibleBuild(t *testing.T) {
	check := assert.New(t)
	active := vector.Generation{ID: 5, State: vector.GenerationActive, Fingerprint: "m:768"}
	building := &vector.Generation{ID: 99, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{active: active, building: building}
	desired := map[int64]string{1: "rev-one", 2: "rev-two"}
	corpora := map[vector.GenerationID]map[int64]string{
		active.ID:   {9: "deleted"},
		building.ID: {9: "deleted"},
	}
	reconcile := func(gen vector.GenerationID) {
		corpora[gen] = maps.Clone(desired)
	}
	runner := &fakeRunner{
		onPerson:  reconcile,
		onRunOnce: reconcile,
	}
	job := &EmbedJob{
		Worker: runner, Backend: backend, Fingerprint: "m:768", BackstopInterval: -1,
	}

	job.Run(t.Context())
	check.Equal(desired, corpora[active.ID], "active person additions and deletions")
	check.Equal(desired, corpora[building.ID], "building person additions and deletions")

	desired = map[int64]string{1: "rev-one-edited", 3: "rev-three"}
	job.Run(t.Context())
	check.Equal(desired, corpora[active.ID], "active person edits, additions, and deletions")
	check.Equal(desired, corpora[building.ID], "building person edits, additions, and deletions")
	check.Equal([]vector.GenerationID{active.ID, active.ID}, runner.persons())
}

// TestEmbedJobRunMaintainsCompatibleActivePeoplePastMismatchedBuild catches
// the early mismatched-build return starving otherwise-compatible active
// person upkeep.
func TestEmbedJobRunMaintainsCompatibleActivePeoplePastMismatchedBuild(t *testing.T) {
	active := vector.Generation{ID: 5, State: vector.GenerationActive, Fingerprint: "new:768"}
	backend := &fakeBackend{
		active: active,
		building: &vector.Generation{
			ID: 33, State: vector.GenerationBuilding, Fingerprint: "old:512",
		},
	}
	runner := &fakeRunner{}
	job := &EmbedJob{
		Worker: runner, Backend: backend, Fingerprint: "new:768", BackstopInterval: -1,
	}

	job.Run(t.Context())

	_, buildRuns, _ := runner.calls()
	assert.Zero(t, buildRuns, "mismatched building message work remains declined")
	assert.Equal(t, []vector.GenerationID{active.ID}, runner.persons(),
		"compatible active person upkeep must still run")
}

// TestEmbedJobRunActivePersonFailureDoesNotStarveCompatibleBuild catches a
// person-only active maintenance failure aborting valid building-generation
// message and person work.
func TestEmbedJobRunActivePersonFailureDoesNotStarveCompatibleBuild(t *testing.T) {
	active := vector.Generation{ID: 5, State: vector.GenerationActive, Fingerprint: "m:768"}
	building := &vector.Generation{ID: 99, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{active: active, building: building}
	runner := &fakeRunner{personErr: errors.New("synthetic active person failure")}
	var logs bytes.Buffer
	job := &EmbedJob{
		Worker: runner, Backend: backend, Fingerprint: "m:768", BackstopInterval: -1,
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
	}

	job.Run(t.Context())

	_, buildRuns, buildGen := runner.calls()
	assert.Equal(t, 1, buildRuns, "valid building pass must still run")
	assert.Equal(t, building.ID, buildGen)
	assert.Contains(t, logs.String(), "active person embedding run failed")
}

// TestEmbedJob_Run_ActivatesBuildingWhenDrained verifies the
// activation gate: after RunOnce on a building generation, if coverage
// is complete for that gen (no live message still needs embedding), the
// daemon must call ActivateGeneration so the new index actually starts
// serving. Without this, a daemon-only deployment can never complete a
// `--full-rebuild` started by the CLI.
func TestEmbedJob_Run_ActivatesBuildingWhenDrained(t *testing.T) {
	building := &vector.Generation{ID: 77, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{
		activeErr: vector.ErrNoActiveGeneration,
		building:  building,
	}
	runner := &fakeRunner{}
	cov := &fakeCoverage{missing: 0}
	job := &EmbedJob{Worker: runner, Backend: backend, Store: cov, Fingerprint: "m:768"}

	job.Run(context.Background())

	assert.Equal(t, []vector.GenerationID{77}, backend.activations(), "activations")
}

// TestEmbedJob_Run_DoesNotActivateWhilePending guards the inverse
// case: coverage still reports missing messages, so the building must
// NOT be activated yet (its index is incomplete).
func TestEmbedJob_Run_DoesNotActivateWhilePending(t *testing.T) {
	building := &vector.Generation{ID: 77, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{
		activeErr: vector.ErrNoActiveGeneration,
		building:  building,
	}
	runner := &fakeRunner{}
	cov := &fakeCoverage{missing: 1}
	job := &EmbedJob{Worker: runner, Backend: backend, Store: cov, Fingerprint: "m:768"}

	job.Run(context.Background())

	assert.Empty(t, backend.activations(), "activations (missing still > 0)")
}

// TestEmbedJob_Run_LeavesMismatchedBuildingForCLI guards against the
// daemon silently topping up an unrelated rebuild. When a building
// generation's fingerprint differs from the configured one, the
// daemon must bail out so the operator can resolve via the CLI
// (`msgvault embeddings build --full-rebuild` or retire the stale build).
func TestEmbedJob_Run_LeavesMismatchedBuildingForCLI(t *testing.T) {
	building := &vector.Generation{ID: 33, State: vector.GenerationBuilding, Fingerprint: "old:512"}
	backend := &fakeBackend{
		active:   vector.Generation{ID: 5, State: vector.GenerationActive, Fingerprint: "new:768"},
		building: building,
	}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend, Fingerprint: "new:768"}

	job.Run(context.Background())

	_, run, _ := runner.calls()
	assert.Equal(t, 0, run, "RunOnce calls (mismatched build must be left alone)")
	assert.Empty(t, backend.activations(), "activations")
}

// TestEmbedJob_Run_PostActivationEnqueueDrainsOnNextRun is the
// eventual-consistency check that pairs with the comment in
// embed_job.go's activation gate. It simulates the race the gate is
// designed to tolerate: coverage reads 0 missing, activation flips
// state to active, then a new message appears (as if a sync committed
// between the read and the activate). The next worker run must pick the
// now-active generation as its target — proving the post-activation
// top-up path runs and the system converges.
func TestEmbedJob_Run_PostActivationEnqueueDrainsOnNextRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	gen := vector.Generation{ID: 88, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{
		activeErr: vector.ErrNoActiveGeneration,
		building:  &gen,
	}
	runner := &fakeRunner{}
	cov := &fakeCoverage{missing: 0}
	job := &EmbedJob{Worker: runner, Backend: backend, Store: cov, Fingerprint: "m:768"}

	// Tick 1: building covered, activation flips to active.
	job.Run(context.Background())
	require.Equal([]vector.GenerationID{88}, backend.activations(), "tick 1 activations")

	// Simulate the race: a sync commit lands AFTER activation, adding a
	// message that reads as missing for the (now-active) generation. The
	// fakeBackend reflects the post-activation state.
	cov.missing = 1
	backend.building = nil
	backend.active = vector.Generation{ID: 88, State: vector.GenerationActive, Fingerprint: "m:768"}
	backend.activeErr = nil

	// Tick 2: the active path picks it up and drains.
	job.Run(context.Background())
	_, run, gen2 := runner.calls()
	assert.Equal(2, run, "tick 2 RunOnce calls")
	assert.Equal(vector.GenerationID(88), gen2, "tick 2 RunOnce gen")
	// Activation must NOT fire a second time (idempotency: active-mode
	// runs never call ActivateGeneration).
	assert.Len(backend.activations(), 1, "activations (only first activation)")
}

// TestEmbedJob_Run_BackstopRunsOnFirstTick verifies the auto-backstop is
// woven into the existing embed job: on the first tick (lastBackstop zero)
// it runs a full backstop on the same target as RunOnce.
func TestEmbedJob_Run_BackstopRunsOnFirstTick(t *testing.T) {
	backend := &fakeBackend{active: vector.Generation{ID: 5, State: vector.GenerationActive}}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	job.Run(context.Background())

	_, run, runGen := runner.calls()
	assert.Equal(t, 1, run, "RunOnce calls")
	n, bsGen := runner.backstops()
	assert.Equal(t, 1, n, "RunBackstop calls on first tick")
	assert.Equal(t, runGen, bsGen, "backstop targets the same generation as RunOnce")
}

// TestEmbedJobOperationPassScopesShareScheduledOccurrence catches scheduled
// forward and backstop passes using manual triggers, unstable timestamps, or a
// reused invocation key.
func TestEmbedJobOperationPassScopesShareScheduledOccurrence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	occurrence := time.Date(2026, 8, 30, 12, 34, 0, 0, time.UTC)
	backend := &fakeBackend{active: vector.Generation{ID: 5, State: vector.GenerationActive}}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend, Now: func() time.Time { return occurrence }}

	job.Run(t.Context())
	runScopes, backstopScopes := runner.passScopes()
	require.Len(runScopes, 1)
	require.Len(backstopScopes, 1)
	assert.Equal(operations.TriggerScheduled, runScopes[0].Trigger)
	assert.Equal(operations.TriggerScheduled, backstopScopes[0].Trigger)
	assert.Equal(occurrence, runScopes[0].StartedAt)
	assert.Equal(occurrence, backstopScopes[0].StartedAt)
	assert.Equal("scheduled:2026-08-30T12:34:00Z:g:5:forward", runScopes[0].Key)
	assert.Equal("scheduled:2026-08-30T12:34:00Z:g:5:backstop", backstopScopes[0].Key)
	messageScope, err := runScopes[0].ForKind(operations.KindMessageEmbedding)
	require.NoError(err)
	personScope, err := runScopes[0].ForKind(operations.KindPersonEmbedding)
	require.NoError(err)
	assert.Equal("scheduled:2026-08-30T12:34:00Z:g:5:forward:message_embedding", messageScope.Key)
	assert.Equal("scheduled:2026-08-30T12:34:00Z:g:5:forward:person_embedding", personScope.Key)
}

// TestEmbedJob_Run_BackstopGatedByInterval verifies the ~daily gating: a
// second tick within BackstopInterval does NOT run another backstop (only
// RunOnce), and a tick after the interval elapses runs one again.
func TestEmbedJob_Run_BackstopGatedByInterval(t *testing.T) {
	assert := assert.New(t)
	backend := &fakeBackend{active: vector.Generation{ID: 5, State: vector.GenerationActive}}
	runner := &fakeRunner{}
	now := time.Now()
	clock := &now
	job := &EmbedJob{
		Worker:           runner,
		Backend:          backend,
		BackstopInterval: 24 * time.Hour,
		Now:              func() time.Time { return *clock },
	}

	// Tick 1: backstop runs (first tick).
	job.Run(context.Background())
	n, _ := runner.backstops()
	assert.Equal(1, n, "tick 1: backstop runs")

	// Tick 2, only 1h later: within interval -> only RunOnce, no backstop.
	*clock = now.Add(1 * time.Hour)
	job.Run(context.Background())
	n, _ = runner.backstops()
	assert.Equal(1, n, "tick 2 (within interval): no extra backstop")
	_, run, _ := runner.calls()
	assert.Equal(2, run, "tick 2: RunOnce still runs")

	// Tick 3, 25h after the last backstop: interval elapsed -> backstop runs.
	*clock = now.Add(25 * time.Hour)
	job.Run(context.Background())
	n, _ = runner.backstops()
	assert.Equal(2, n, "tick 3 (interval elapsed): backstop runs again")
}

// TestEmbedJob_Run_BackstopDisabled verifies a negative BackstopInterval
// disables the auto-backstop entirely (only RunOnce runs).
func TestEmbedJob_Run_BackstopDisabled(t *testing.T) {
	backend := &fakeBackend{active: vector.Generation{ID: 5, State: vector.GenerationActive}}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend, BackstopInterval: -1}

	job.Run(context.Background())

	n, _ := runner.backstops()
	assert.Equal(t, 0, n, "backstop disabled: no RunBackstop")
	_, run, _ := runner.calls()
	assert.Equal(t, 1, run, "RunOnce still runs")
}

// TestEmbedJob_Run_BackstopFailureNotFatal verifies a backstop error is
// logged but does not block the rest of the cycle, and lastBackstop is not
// advanced (so the next tick retries).
func TestEmbedJob_Run_BackstopFailureRetries(t *testing.T) {
	backend := &fakeBackend{active: vector.Generation{ID: 5, State: vector.GenerationActive}}
	runner := &fakeRunner{backstopErr: errors.New("boom")}
	now := time.Now()
	clock := &now
	job := &EmbedJob{
		Worker:  runner,
		Backend: backend,
		Now:     func() time.Time { return *clock },
	}

	// Tick 1: backstop attempted, fails.
	job.Run(context.Background())
	n, _ := runner.backstops()
	assert.Equal(t, 1, n, "tick 1: backstop attempted")

	// Tick 2 immediately after: because the failure did not advance
	// lastBackstop, the backstop is retried (lastBackstop still zero).
	runner.backstopErr = nil
	job.Run(context.Background())
	n, _ = runner.backstops()
	assert.Equal(t, 2, n, "tick 2: backstop retried after prior failure")
}

// TestEmbedJob_Run_BackstopThrottleIsPerGeneration reproduces the compound
// precondition the per-generation throttle fixes: the throttle was recently
// set for the ACTIVE generation (so a single job-global throttle WOULD skip
// the next backstop), then pickTarget switches to a DIFFERENT building
// generation that has a below-watermark straggler. With a global time.Time
// throttle the building generation's first backstop would be suppressed for up
// to BackstopInterval — leaving the straggler unrecovered and blocking
// auto-activation (MissingCount stays > 0). With the per-generation map the
// building generation has no recorded backstop, so it runs on this tick,
// recovers the straggler, and the generation activates.
//
// This FAILS with the old global throttle (no backstop for gen 99 -> straggler
// remains, no activation) and PASSES with the per-gen map.
func TestEmbedJob_Run_BackstopThrottleIsPerGeneration(t *testing.T) {
	assert := assert.New(t)
	building := &vector.Generation{ID: 99, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{
		active:   vector.Generation{ID: 5, State: vector.GenerationActive, Fingerprint: "m:768"},
		building: building,
	}
	// The straggler is recovered by the backstop pass: coverage reports it
	// missing UNTIL RunBackstop runs, then reports complete. This mirrors the
	// production recovery path (RunBackstop re-embeds the sub-watermark
	// straggler, after which MissingCount drops to 0 and the gen can activate).
	cov := &recoverOnBackstopCoverage{}
	runner := &fakeRunner{onBackstop: cov.markRecovered}
	now := time.Now()
	clock := &now
	job := &EmbedJob{
		Worker:           runner,
		Backend:          backend,
		Store:            cov,
		Fingerprint:      "m:768",
		BackstopInterval: 24 * time.Hour,
		Now:              func() time.Time { return *clock },
		// Seed only the ACTIVE generation's last backstop to "just now". A
		// job-global throttle would read this and skip the building gen's
		// backstop; the per-gen map must not, because gen 99 has no entry.
		lastBackstop: map[vector.GenerationID]time.Time{5: now},
	}

	// Single tick: pickTarget prefers the building generation. Its backstop
	// must run despite the active generation's recent (seeded) backstop.
	job.Run(context.Background())

	n, bsGen := runner.backstops()
	assert.Equal(1, n, "building generation backstop must run despite active gen's recent backstop")
	assert.Equal(vector.GenerationID(99), bsGen, "backstop targets the building generation")
	assert.Equal([]vector.GenerationID{99}, backend.activations(),
		"building generation activates after backstop recovers its straggler")
}

// recoverOnBackstopCoverage models the activation gate's view of a building
// generation that has one below-watermark straggler: MissingCount reports 1
// until the backstop pass recovers it (markRecovered), then 0.
type recoverOnBackstopCoverage struct {
	mu        sync.Mutex
	recovered bool
}

func (c *recoverOnBackstopCoverage) markRecovered() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recovered = true
}

func (c *recoverOnBackstopCoverage) MissingCount(context.Context, int64) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.recovered {
		return 0, nil
	}
	return 1, nil
}

// fakeCoverage satisfies EmbedCoverage for the activation-gate tests:
// it reports a fixed number of live messages still needing embedding.
type fakeCoverage struct {
	missing     int64
	calls       int
	yieldCancel context.CancelCauseFunc
}

type fakeConvergenceChecker struct {
	result ConvergenceResult
	err    error
	cancel context.CancelCauseFunc
}

func (c *fakeConvergenceChecker) CheckConvergence(context.Context, vector.GenerationID) (ConvergenceResult, error) {
	if c.cancel != nil {
		c.cancel(jobctx.ErrYieldedToWaiter)
	}
	return c.result, c.err
}

func TestEmbedJob_Run_YieldedConvergenceStopsWithoutWarning(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	backend := &fakeBackend{building: &vector.Generation{ID: 7, Fingerprint: "m:768"}}
	var logs bytes.Buffer
	job := &EmbedJob{
		Worker: &fakeRunner{}, Backend: backend, Fingerprint: "m:768",
		Convergence:      &fakeConvergenceChecker{cancel: cancel, err: errors.New("canceled query")},
		BackstopInterval: -1, Log: slog.New(slog.NewTextHandler(&logs, nil)),
	}

	job.Run(ctx)

	assert.Empty(t, backend.activations())
	assert.NotContains(t, logs.String(), "convergence check after run failed")
}

func TestEmbedJob_Run_ContextualActivationRequiresEveryConvergenceDimension(t *testing.T) {
	complete := ConvergenceResult{
		MessageCoverageComplete: true,
		PersonCoverageComplete:  true,
		LatestJournalSequence:   12,
		ConsumedJournalSequence: 12,
		ReconciliationComplete:  true,
	}
	tests := []struct {
		name   string
		mutate func(*ConvergenceResult)
		want   bool
	}{
		{name: "complete", want: true},
		{name: "message coverage incomplete", mutate: func(r *ConvergenceResult) { r.MessageCoverageComplete = false }},
		{name: "person coverage incomplete", mutate: func(r *ConvergenceResult) { r.PersonCoverageComplete = false }},
		{name: "journal not consumed", mutate: func(r *ConvergenceResult) { r.ConsumedJournalSequence-- }},
		{name: "reconciliation incomplete", mutate: func(r *ConvergenceResult) { r.ReconciliationComplete = false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := complete
			if tt.mutate != nil {
				tt.mutate(&result)
			}
			backend := &fakeBackend{building: &vector.Generation{ID: 7, Fingerprint: "m:768"}}
			job := &EmbedJob{
				Worker: &fakeRunner{}, Backend: backend, Fingerprint: "m:768",
				Convergence: &fakeConvergenceChecker{result: result}, SequenceBoundActivation: true,
				BackstopInterval: -1,
			}
			job.Run(t.Context())
			if tt.want {
				assert.Equal(t, []vector.GenerationID{7}, backend.activations())
				assert.Equal(t, []int64{12}, backend.activateSequences)
			} else {
				assert.Empty(t, backend.activations())
			}
		})
	}
}

func TestEmbedJob_Run_ContextualConvergedBuildActivatesBeforeBackstop(t *testing.T) {
	backend := &fakeBackend{building: &vector.Generation{ID: 7, Fingerprint: "m:768"}}
	runner := &fakeRunner{}
	job := &EmbedJob{
		Worker: runner, Backend: backend, Fingerprint: "m:768",
		Convergence: &fakeConvergenceChecker{result: ConvergenceResult{
			MessageCoverageComplete: true,
			PersonCoverageComplete:  true,
			LatestJournalSequence:   12,
			ConsumedJournalSequence: 12,
			ReconciliationComplete:  true,
		}},
		SequenceBoundActivation: true,
	}

	job.Run(t.Context())

	backstops, _ := runner.backstops()
	assert.Zero(t, backstops,
		"an already-converged contextual build must not restart reconciliation before activation")
	assert.Equal(t, []vector.GenerationID{7}, backend.activations())
	assert.Equal(t, []int64{12}, backend.activateSequences)
}

func TestEmbedJob_Run_ContextualIncompleteBuildUsesBackstopRecovery(t *testing.T) {
	assert := assert.New(t)
	backend := &fakeBackend{building: &vector.Generation{ID: 7, Fingerprint: "m:768"}}
	checker := &fakeConvergenceChecker{result: ConvergenceResult{
		MessageCoverageComplete: false,
		PersonCoverageComplete:  true,
		LatestJournalSequence:   12,
		ConsumedJournalSequence: 12,
		ReconciliationComplete:  true,
	}}
	runner := &fakeRunner{onBackstop: func() {
		checker.result.MessageCoverageComplete = true
	}}
	job := &EmbedJob{
		Worker: runner, Backend: backend, Fingerprint: "m:768",
		Convergence: checker, SequenceBoundActivation: true,
	}

	job.Run(t.Context())

	backstops, backstopGen := runner.backstops()
	assert.Equal(1, backstops)
	assert.Equal(vector.GenerationID(7), backstopGen)
	assert.Equal([]vector.GenerationID{7}, backend.activations())
	assert.Equal([]int64{12}, backend.activateSequences)
}

func TestEmbedJob_Run_LegacyConvergenceUsesNormalActivation(t *testing.T) {
	backend := &fakeBackend{building: &vector.Generation{ID: 7, Fingerprint: "m:768"}}
	job := &EmbedJob{
		Worker: &fakeRunner{}, Backend: backend, Fingerprint: "m:768",
		Convergence: &fakeConvergenceChecker{result: ConvergenceResult{
			MessageCoverageComplete: true,
			PersonCoverageComplete:  true,
			ReconciliationComplete:  true,
		}},
		BackstopInterval: -1,
	}

	job.Run(t.Context())

	assert.Equal(t, []vector.GenerationID{7}, backend.activations())
	assert.Empty(t, backend.activateSequences,
		"legacy scheduled builds must not require contextual document progress")
}

func (c *fakeCoverage) MissingCount(_ context.Context, _ int64) (int64, error) {
	c.calls++
	if c.yieldCancel != nil {
		c.yieldCancel(jobctx.ErrYieldedToWaiter)
	}
	return c.missing, nil
}

// slowRunner blocks RunOnce on `release` so tests can control when it
// completes. gate closes exactly once on the first RunOnce entry so
// tests can wait for the slow call to actually be in flight.
type slowRunner struct {
	mu       sync.Mutex
	runCalls int
	gate     chan struct{}
	release  chan struct{}
	gateOnce sync.Once
}

func (r *slowRunner) ReclaimStale(context.Context) (int, error) { return 0, nil }

func (r *slowRunner) RunOnce(
	context.Context, vector.GenerationID, operations.PassScope,
) (embed.RunResult, error) {
	r.mu.Lock()
	r.runCalls++
	r.mu.Unlock()
	if r.gate != nil {
		r.gateOnce.Do(func() { close(r.gate) })
	}
	if r.release != nil {
		<-r.release
	}
	return embed.RunResult{}, nil
}

func (r *slowRunner) RunBackstop(
	context.Context, vector.GenerationID, operations.PassScope,
) (embed.RunResult, error) {
	return embed.RunResult{}, nil
}

func (r *slowRunner) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runCalls
}

// TestEmbedJob_Run_SkipsWhenAlreadyRunning verifies the TryLock guard:
// a second Run invoked while the first is still in flight must return
// immediately without calling the worker. This prevents cron and the
// post-sync hook from stepping on each other's claim passes.
func TestEmbedJob_Run_SkipsWhenAlreadyRunning(t *testing.T) {
	backend := &fakeBackend{active: vector.Generation{ID: 11}}
	gate := make(chan struct{})
	release := make(chan struct{})
	runner := &slowRunner{gate: gate, release: release}
	job := &EmbedJob{Worker: runner, Backend: backend}

	go job.Run(context.Background())

	// Wait for the first RunOnce to actually be in flight.
	select {
	case <-gate:
	case <-time.After(time.Second):
		require.Fail(t, "first RunOnce did not start")
	}

	// Second call must return immediately (no waiters queued).
	done := make(chan struct{})
	go func() {
		job.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		require.Fail(t, "second Run blocked; TryLock guard did not short-circuit")
	}

	assert.Equal(t, 1, runner.calls(), "RunOnce calls during overlap")

	// Release the first call so the job can complete.
	close(release)
}

func TestEmbedJob_Run_NilSafe(t *testing.T) {
	// All nil-safety guards should return cleanly without panicking or
	// calling the worker. Use a runner that panics if touched.
	touchy := &fakeRunner{}
	cases := []struct {
		name string
		job  *EmbedJob
	}{
		{"nil job", nil},
		{"nil worker", &EmbedJob{Backend: &fakeBackend{}}},
		{"nil backend", &EmbedJob{Worker: touchy}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.job.Run(context.Background())
		})
	}
	_, run, _ := touchy.calls()
	assert.Equal(t, 0, run, "nil-safe Run should not invoke worker")
}

// ---------- SetEmbedJob tests ----------

func TestSchedulerSetDocumentVectorJobUsesEmbeddingSchedulePolicy(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	s := New(func(context.Context, string) error { return nil })
	t.Cleanup(func() { <-s.Stop().Done() })
	called := 0
	requirements.NoError(s.SetDocumentVectorJob(func(context.Context) error {
		called++
		return nil
	}, "*/5 * * * *", true))
	assertions.True(s.documentVectorEntrySet)
	assertions.True(s.runDocumentVectorAfterSync)
	requirements.ErrorContains(s.SetDocumentVectorJob(func(context.Context) error { return nil }, "invalid", false), "invalid")
	assertions.True(s.documentVectorEntrySet, "invalid replacement preserves the prior job")
	assertions.Zero(called)
}

func TestSchedulerDocumentVectorJobRunsOnlyAfterSuccessfulSync(t *testing.T) {
	for _, test := range []struct {
		name     string
		syncErr  error
		wantRuns int64
	}{
		{name: "successful sync", wantRuns: 1},
		{name: "failed sync", syncErr: errors.New("sync failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			syncDone := make(chan struct{})
			s := New(func(context.Context, string) error {
				close(syncDone)
				return test.syncErr
			})
			var runs atomic.Int64
			documentDone := make(chan struct{}, 1)
			requirements.NoError(s.SetDocumentVectorJob(func(context.Context) error {
				runs.Add(1)
				documentDone <- struct{}{}
				return nil
			}, "", true))
			requirements.NoError(s.AddAccount("test@example.test", "0 0 1 1 *"))
			s.Start()
			t.Cleanup(func() { <-s.Stop().Done() })
			requirements.NoError(s.TriggerSync("test@example.test"))
			select {
			case <-syncDone:
			case <-time.After(time.Second):
				requirements.Fail("sync did not complete")
			}
			if test.wantRuns == 1 {
				select {
				case <-documentDone:
				case <-time.After(time.Second):
					requirements.Fail("document vector job did not run")
				}
			} else {
				assertions.Never(func() bool { return runs.Load() != 0 }, 100*time.Millisecond, 5*time.Millisecond)
			}
			assertions.Equal(test.wantRuns, runs.Load())
		})
	}
}

func TestScheduler_SetEmbedJob_AddsCronEntry(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s := New(func(ctx context.Context, email string) error { return nil })
	backend := &fakeBackend{active: vector.Generation{ID: 1}}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	require.NoError(s.SetEmbedJob(job, "*/5 * * * *", false), "SetEmbedJob first")
	assert.True(s.embedEntrySet, "embedEntrySet should be true after first SetEmbedJob")

	// Replacing with a new schedule should not error.
	require.NoError(s.SetEmbedJob(job, "0 * * * *", true), "SetEmbedJob replace")
	assert.True(s.embedEntrySet, "embedEntrySet should remain true after replacement")
	assert.True(s.runEmbedAfterSync, "runEmbedAfterSync should be true after replacement with runAfterSync=true")

	// Clearing.
	require.NoError(s.SetEmbedJob(nil, "", false), "SetEmbedJob clear")
	assert.False(s.embedEntrySet, "embedEntrySet should be false after clear")
	assert.Nil(s.embedJob, "embedJob should be nil after clear")
	assert.False(s.runEmbedAfterSync, "runEmbedAfterSync should be false after clear")
}

func TestScheduler_SetEmbedJob_InvalidCron(t *testing.T) {
	s := New(func(ctx context.Context, email string) error { return nil })
	backend := &fakeBackend{}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	err := s.SetEmbedJob(job, "not a cron", false)
	require.Error(t, err, "SetEmbedJob with invalid cron")
	assert.False(t, s.embedEntrySet, "embedEntrySet should remain false after invalid cron")
}

func TestScheduler_SetEmbedJob_InvalidReplacePreservesPrevious(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// After a successful SetEmbedJob, a later call with an invalid cron
	// must leave the previous job, schedule, and post-sync flag intact.
	s := New(func(ctx context.Context, email string) error { return nil })
	backend := &fakeBackend{}
	job1 := &EmbedJob{Worker: &fakeRunner{}, Backend: backend}
	job2 := &EmbedJob{Worker: &fakeRunner{}, Backend: backend}

	require.NoError(s.SetEmbedJob(job1, "*/5 * * * *", true), "SetEmbedJob(job1)")
	prevEntry := s.embedEntry

	require.Error(s.SetEmbedJob(job2, "bogus cron", true), "SetEmbedJob(job2, invalid)")

	assert.Same(job1, s.embedJob, "embedJob was replaced on invalid cron; want job1")
	assert.True(s.runEmbedAfterSync, "runEmbedAfterSync should remain true")
	assert.True(s.embedEntrySet, "cron entry should still be job1's (entrySet)")
	assert.Equal(prevEntry, s.embedEntry, "cron entry should still be job1's")
}

func TestScheduler_SetEmbedJob_EmptyScheduleNoCronEntry(t *testing.T) {
	assert := assert.New(t)
	s := New(func(ctx context.Context, email string) error { return nil })
	backend := &fakeBackend{}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	require.NoError(t, s.SetEmbedJob(job, "", true), "SetEmbedJob")
	assert.False(s.embedEntrySet, "empty schedule should not create a cron entry")
	assert.NotNil(s.embedJob, "embedJob should be set even with empty schedule")
	assert.True(s.runEmbedAfterSync, "runEmbedAfterSync should be true")
}

func TestScheduler_RunAfterSync_Fires(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	syncDone := make(chan struct{})
	s := New(func(ctx context.Context, email string) error {
		close(syncDone)
		return nil
	})
	backend := &fakeBackend{active: vector.Generation{ID: 42}}
	runDone := make(chan struct{})
	runner := &fakeRunner{runDone: runDone}
	job := &EmbedJob{Worker: runner, Backend: backend}

	require.NoError(s.SetEmbedJob(job, "", true), "SetEmbedJob")
	require.NoError(s.AddAccount("test@gmail.com", "0 0 1 1 *"), "AddAccount")

	s.Start()
	defer func() {
		ctx := s.Stop()
		<-ctx.Done()
	}()

	require.NoError(s.TriggerSync("test@gmail.com"), "TriggerSync")

	select {
	case <-syncDone:
	case <-time.After(time.Second):
		require.Fail("syncFunc did not run")
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		require.Fail("embed RunOnce did not fire after sync")
	}

	_, run, gen := runner.calls()
	assert.Equal(1, run, "RunOnce calls")
	assert.Equal(vector.GenerationID(42), gen, "RunOnce gen")
}

func TestScheduler_RunAfterSync_DisabledDoesNotFire(t *testing.T) {
	require := require.New(t)
	s := New(func(ctx context.Context, email string) error { return nil })
	backend := &fakeBackend{active: vector.Generation{ID: 1}}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	// runAfterSync = false
	require.NoError(s.SetEmbedJob(job, "", false), "SetEmbedJob")
	require.NoError(s.AddAccount("test@gmail.com", "0 0 1 1 *"), "AddAccount")

	s.Start()
	defer func() {
		ctx := s.Stop()
		<-ctx.Done()
	}()

	require.NoError(s.TriggerSync("test@gmail.com"), "TriggerSync")

	// Give runSync a chance to finish.
	time.Sleep(50 * time.Millisecond)

	_, run, _ := runner.calls()
	assert.Equal(t, 0, run, "RunOnce calls when runAfterSync is false")
}

func TestScheduler_RunAfterSync_SkipOnStopped(t *testing.T) {
	require := require.New(t)
	// When a sync's post-sync window coincides with Stop(), the embed
	// hook must skip. We gate the syncFunc on a release channel so the
	// test can Stop the scheduler before the sync completes.
	release := make(chan struct{})
	s := New(func(ctx context.Context, email string) error {
		<-release
		return nil
	})
	backend := &fakeBackend{active: vector.Generation{ID: 1}}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	require.NoError(s.SetEmbedJob(job, "", true), "SetEmbedJob")
	require.NoError(s.AddAccount("test@gmail.com", "0 0 1 1 *"), "AddAccount")

	s.Start()
	require.NoError(s.TriggerSync("test@gmail.com"), "TriggerSync")

	// Ask the scheduler to stop while the sync is still in-flight.
	stopCtx := s.Stop()
	close(release) // let the sync complete
	<-stopCtx.Done()

	_, run, _ := runner.calls()
	assert.Equal(t, 0, run, "RunOnce calls when scheduler is stopped")
}

func TestScheduler_RunAfterSync_SkipOnSyncError(t *testing.T) {
	require := require.New(t)
	s := New(func(ctx context.Context, email string) error {
		return errors.New("sync failed")
	})
	backend := &fakeBackend{active: vector.Generation{ID: 1}}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	require.NoError(s.SetEmbedJob(job, "", true), "SetEmbedJob")
	require.NoError(s.AddAccount("test@gmail.com", "0 0 1 1 *"), "AddAccount")

	s.Start()
	defer func() {
		ctx := s.Stop()
		<-ctx.Done()
	}()

	require.NoError(s.TriggerSync("test@gmail.com"), "TriggerSync")

	time.Sleep(50 * time.Millisecond)

	_, run, _ := runner.calls()
	assert.Equal(t, 0, run, "RunOnce calls when sync failed")
}

func TestScheduler_VisualRunAfterSyncDoesNotExtendSync(t *testing.T) {
	requirements := require.New(t)
	visualStarted := make(chan struct{})
	releaseVisual := make(chan struct{})
	s := New(func(context.Context, string) error { return nil })
	s.SetVisualPostSyncJob(func(context.Context) error {
		close(visualStarted)
		<-releaseVisual
		return nil
	})
	requirements.NoError(s.AddAccount("test@gmail.com", "0 0 1 1 *"))
	s.Start()
	t.Cleanup(func() {
		select {
		case <-releaseVisual:
		default:
			close(releaseVisual)
		}
		ctx := s.Stop()
		<-ctx.Done()
	})

	requirements.NoError(s.TriggerSync("test@gmail.com"))
	select {
	case <-visualStarted:
	case <-time.After(time.Second):
		requirements.Fail("visual post-sync pass did not start")
	}
	requirements.Eventually(func() bool {
		statuses := s.Status()
		return len(statuses) == 1 && !statuses[0].Running
	}, time.Second, 5*time.Millisecond, "sync should finish while hosted visual work remains blocked")
	close(releaseVisual)
}

func TestValidateCronExpr(t *testing.T) {
	tests := []struct {
		expr    string
		wantErr bool
	}{
		{"0 2 * * *", false},    // 2am daily
		{"*/15 * * * *", false}, // Every 15 minutes
		{"0 0 1 * *", false},    // Monthly on 1st
		{"0 0 * * 0", false},    // Weekly on Sunday
		{"invalid", true},
		{"* * * * * *", true}, // Too many fields
		{"", true},
	}

	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			err := ValidateCronExpr(tt.expr)
			if tt.wantErr {
				assert.Error(t, err, "ValidateCronExpr(%q)", tt.expr)
			} else {
				assert.NoError(t, err, "ValidateCronExpr(%q)", tt.expr)
			}
		})
	}
}

type yieldingWorkTracker struct {
	fakeWorkTracker

	yield atomic.Bool
}

func (t *yieldingWorkTracker) ShouldYield() bool {
	return t.yield.Load()
}

func TestGenericJobYieldContextFinishesCleanly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	oldPoll := yieldPollInterval
	yieldPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { yieldPollInterval = oldPoll })

	tracker := &yieldingWorkTracker{}
	started := make(chan struct{})
	causeCh := make(chan error, 1)
	s := New(func(context.Context, string) error { return nil }).WithWorkTracker(tracker)
	require.NoError(s.AddJob(Job{
		Name:     "attachment-maintenance",
		Schedule: "17 3 * * *",
		Run: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			causeCh <- context.Cause(ctx)
			return ctx.Err()
		},
	}), "AddJob")

	errCh := make(chan error, 1)
	go func() { errCh <- s.TriggerJob("attachment-maintenance") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		require.FailNow("generic job did not start")
	}

	tracker.yield.Store(true)
	select {
	case cause := <-causeCh:
		require.ErrorIs(cause, ErrYieldedToWaiter, "generic job cancellation cause")
	case <-time.After(time.Second):
		require.FailNow("generic job did not yield")
	}
	select {
	case err := <-errCh:
		require.NoError(err, "yield is not a generic job failure")
	case <-time.After(time.Second):
		require.FailNow("TriggerJob did not return after yield")
	}
	require.Eventually(func() bool {
		return tracker.active() == 0
	}, time.Second, time.Millisecond, "gate released after generic job yield")
	status := s.JobStatus()
	require.Len(status, 1)
	assert.Empty(status[0].LastError, "yield must not be recorded as a generic job error")

	stopCtx := s.Stop()
	select {
	case <-stopCtx.Done():
	case <-time.After(time.Second):
		require.FailNow("scheduler did not stop after yielded generic job")
	}
}

func TestGenericJobShutdownContextFinishesCleanly(t *testing.T) {
	require := require.New(t)
	started := make(chan struct{})
	s := New(func(context.Context, string) error { return nil })
	require.NoError(s.AddJob(Job{
		Name:     "attachment-maintenance",
		Schedule: "17 3 * * *",
		Run: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	}), "AddJob")
	s.Start()

	errCh := make(chan error, 1)
	go func() { errCh <- s.TriggerJob("attachment-maintenance") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		require.FailNow("generic job did not start")
	}

	stopCtx := s.Stop()
	select {
	case <-stopCtx.Done():
	case <-time.After(time.Second):
		require.FailNow("scheduler shutdown did not wait for canceled generic job")
	}
	select {
	case err := <-errCh:
		require.ErrorIs(err, context.Canceled, "running job observes scheduler cancellation")
	case <-time.After(time.Second):
		require.FailNow("TriggerJob did not finish during shutdown")
	}
}

func TestRemoveJobClearsStableGenericScheduleAndRunner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s := New(nil)
	require.NoError(s.AddJob(Job{Name: "carddav", Schedule: "0 1 * * *", Run: func(context.Context) error { return nil }}))
	require.True(s.IsJobScheduled("carddav"))

	s.RemoveJob("carddav")

	assert.False(s.IsJobScheduled("carddav"))
	assert.Empty(s.JobStatus())
	require.ErrorContains(s.TriggerJob("carddav"), "not scheduled")
}

func TestScheduledSyncYieldsToWaiter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	oldPoll := yieldPollInterval
	yieldPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { yieldPollInterval = oldPoll })

	tracker := &yieldingWorkTracker{}
	started := make(chan struct{})
	var startedOnce sync.Once
	syncCtxErr := make(chan error, 1)
	s := New(func(ctx context.Context, email string) error {
		startedOnce.Do(func() { close(started) })
		<-ctx.Done()
		syncCtxErr <- context.Cause(ctx)
		return ctx.Err()
	}).WithWorkTracker(tracker)

	require.NoError(s.AddAccount("test@gmail.com", "0 0 1 1 *"), "AddAccount")
	s.Start()
	defer func() {
		ctx := s.Stop()
		<-ctx.Done()
	}()

	require.NoError(s.TriggerSync("test@gmail.com"), "TriggerSync")
	select {
	case <-started:
	case <-time.After(time.Second):
		require.FailNow("sync did not start")
	}

	tracker.yield.Store(true)
	select {
	case cause := <-syncCtxErr:
		require.ErrorIs(cause, ErrYieldedToWaiter, "cancellation cause")
	case <-time.After(time.Second):
		require.FailNow("sync was not cancelled after yield request")
	}

	require.Eventually(func() bool {
		return tracker.active() == 0
	}, time.Second, time.Millisecond, "gate released after yield")

	for _, status := range s.Status() {
		assert.Empty(status.LastError, "yield must not be recorded as a sync error")
	}
}

func TestScheduler_VisualPostSyncQueuesPendingRunWhileActive(t *testing.T) {
	requirements := require.New(t)
	var mu sync.Mutex
	runs := 0
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	s := New(func(context.Context, string) error { return nil })
	s.SetVisualPostSyncJob(func(context.Context) error {
		mu.Lock()
		runs++
		count := runs
		mu.Unlock()
		if count == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		return nil
	})
	requirements.NoError(s.AddAccount("test@gmail.com", "0 0 1 1 *"))
	s.Start()
	t.Cleanup(func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
		ctx := s.Stop()
		<-ctx.Done()
	})

	requirements.NoError(s.TriggerSync("test@gmail.com"))
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		requirements.Fail("first visual pass did not start")
	}
	// A second sync completes while the first pass is still running; its
	// changes must be processed by a queued follow-up pass, not dropped.
	requirements.NoError(s.TriggerSync("test@gmail.com"))
	time.Sleep(50 * time.Millisecond)
	close(releaseFirst)
	requirements.Eventually(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return runs == 2
	}, 2*time.Second, 10*time.Millisecond, "the pending pass must run after the active one finishes")
}
