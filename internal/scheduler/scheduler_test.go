package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

func TestNew(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	requirepkg.NotNil(t, s, "New()")
	assertpkg.NotNil(t, s.cron, "cron")
	assertpkg.NotNil(t, s.jobs, "jobs map")
}

func TestAddAccount(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	// Valid cron expression
	requirepkg.NoError(t, s.AddAccount("test@gmail.com", "0 2 * * *"), "AddAccount() with valid cron")

	// Check job was added
	s.mu.RLock()
	_, exists := s.jobs["test@gmail.com"]
	s.mu.RUnlock()

	assertpkg.True(t, exists, "job was not added to jobs map")
}

func TestAddAccountInvalidCron(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	err := s.AddAccount("test@gmail.com", "invalid cron")
	assertpkg.Error(t, err, "AddAccount() with invalid cron")
}

func TestAddAccountReplacesExisting(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	// Add initial schedule
	requirepkg.NoError(t, s.AddAccount("test@gmail.com", "0 2 * * *"), "AddAccount()")

	s.mu.RLock()
	firstID := s.jobs["test@gmail.com"]
	s.mu.RUnlock()

	// Replace with new schedule
	requirepkg.NoError(t, s.AddAccount("test@gmail.com", "0 3 * * *"), "AddAccount() replacement")

	s.mu.RLock()
	secondID := s.jobs["test@gmail.com"]
	s.mu.RUnlock()

	assertpkg.NotEqual(t, firstID, secondID, "job ID was not updated after replacement")
}

func TestRemoveAccount(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	requirepkg.NoError(t, s.AddAccount("test@gmail.com", "0 2 * * *"), "AddAccount")
	s.RemoveAccount("test@gmail.com")

	s.mu.RLock()
	_, exists := s.jobs["test@gmail.com"]
	s.mu.RUnlock()

	assertpkg.False(t, exists, "job still exists after RemoveAccount()")
}

func TestRemoveAccountNonExistent(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	// Should not panic
	s.RemoveAccount("nonexistent@gmail.com")
}

func TestAddAccountsFromConfig(t *testing.T) {
	assert := assertpkg.New(t)
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

	assertpkg.Equal(t, 1, scheduled, "scheduled")
	assertpkg.Len(t, errs, 1, "errs")
}

func TestSchedulerGenericJobStatus(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
		assertpkg.Fail(t, "Stop() did not complete in time")
	}
}

func TestIsRunning(t *testing.T) {
	assert := assertpkg.New(t)
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
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	assert := assertpkg.New(t)
	var called atomic.Int32
	s := New(func(ctx context.Context, email string) error {
		called.Add(1)
		time.Sleep(50 * time.Millisecond)
		return nil
	})

	requirepkg.NoError(t, s.AddAccount("test@gmail.com", "0 0 1 1 *"), "AddAccount")

	// Trigger manually
	err := s.TriggerSync("test@gmail.com")
	requirepkg.NoError(t, err, "TriggerSync()")

	// Wait for sync to start
	time.Sleep(10 * time.Millisecond)

	// Second trigger should fail (already running)
	err = s.TriggerSync("test@gmail.com")
	requirepkg.Error(t, err, "TriggerSync() while running")

	// Wait for completion
	time.Sleep(100 * time.Millisecond)

	assert.Equal(int32(1), called.Load(), "syncFunc called times")
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

	requirepkg.NoError(t, s.AddAccount("test@gmail.com", "0 0 1 1 *"), "AddAccount")

	// Try to trigger multiple times concurrently
	for range 5 {
		_ = s.TriggerSync("test@gmail.com")
	}

	time.Sleep(200 * time.Millisecond)

	assertpkg.LessOrEqual(t, maxConcurrent.Load(), int32(1), "max concurrent")
}

func TestStatus(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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

	requirepkg.NoError(t, s.AddAccount("test@gmail.com", "0 0 1 1 *"), "AddAccount")

	ctx := s.Stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		requirepkg.Fail(t, "Stop() did not complete in time")
	}

	err := s.TriggerSync("test@gmail.com")
	assertpkg.Error(t, err, "TriggerSync() after Stop()")
}

// ---------- fakes for EmbedJob tests ----------

// fakeBackend implements vector.Backend. Only ActiveGeneration,
// BuildingGeneration, ActivateGeneration, and EnsureSeeded are
// meaningfully populated; the rest panic to catch accidental usage.
type fakeBackend struct {
	active    vector.Generation
	activeErr error
	building  *vector.Generation
	buildErr  error
	// activateErr is what ActivateGeneration returns. activateCalls
	// records the gen IDs the EmbedJob asked to activate.
	activateErr     error
	mu              sync.Mutex
	activateCallIDs []vector.GenerationID
	// ensureSeededErr is what EnsureSeeded returns; ensureSeededIDs
	// records the gen IDs the EmbedJob passed to EnsureSeeded.
	ensureSeededErr error
	ensureSeededIDs []vector.GenerationID

	activeCalls   atomic.Int32
	buildingCalls atomic.Int32
}

func (f *fakeBackend) ActiveGeneration(ctx context.Context) (vector.Generation, error) {
	f.activeCalls.Add(1)
	return f.active, f.activeErr
}

func (f *fakeBackend) BuildingGeneration(ctx context.Context) (*vector.Generation, error) {
	f.buildingCalls.Add(1)
	return f.building, f.buildErr
}

func (f *fakeBackend) CreateGeneration(ctx context.Context, model string, dim int, fp string) (vector.GenerationID, error) {
	panic("unexpected: CreateGeneration")
}
func (f *fakeBackend) ActivateGeneration(ctx context.Context, gen vector.GenerationID) error {
	f.mu.Lock()
	f.activateCallIDs = append(f.activateCallIDs, gen)
	f.mu.Unlock()
	return f.activateErr
}
func (f *fakeBackend) activations() []vector.GenerationID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]vector.GenerationID(nil), f.activateCallIDs...)
}
func (f *fakeBackend) RetireGeneration(ctx context.Context, gen vector.GenerationID) error {
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
func (f *fakeBackend) Close() error { return nil }
func (f *fakeBackend) EnsureSeeded(_ context.Context, gen vector.GenerationID) error {
	f.mu.Lock()
	f.ensureSeededIDs = append(f.ensureSeededIDs, gen)
	f.mu.Unlock()
	return f.ensureSeededErr
}
func (f *fakeBackend) ensureSeededCalls() []vector.GenerationID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]vector.GenerationID(nil), f.ensureSeededIDs...)
}

// fakeRunner records calls to satisfy EmbedRunner.
type fakeRunner struct {
	mu            sync.Mutex
	reclaimErr    error
	reclaimCalls  int
	runErr        error
	runCalls      int
	lastRunGen    vector.GenerationID
	runOnceResult embed.RunResult
	runDoneOnce   sync.Once
	runDone       chan struct{} // optional: closed after first RunOnce
}

func (r *fakeRunner) ReclaimStale(ctx context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reclaimCalls++
	return 0, r.reclaimErr
}

func (r *fakeRunner) RunOnce(ctx context.Context, gen vector.GenerationID) (embed.RunResult, error) {
	r.mu.Lock()
	r.runCalls++
	r.lastRunGen = gen
	ch := r.runDone
	res := r.runOnceResult
	err := r.runErr
	r.mu.Unlock()
	if ch != nil {
		r.runDoneOnce.Do(func() { close(ch) })
	}
	return res, err
}

func (r *fakeRunner) calls() (reclaim, run int, lastGen vector.GenerationID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reclaimCalls, r.runCalls, r.lastRunGen
}

// ---------- EmbedJob tests ----------

func TestEmbedJob_Run_ActiveGeneration(t *testing.T) {
	assert := assertpkg.New(t)
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
	assertpkg.Equal(t, 0, run, "RunOnce calls (refuse to top up mismatched active)")
	assertpkg.Empty(t, backend.activations(), "ActivateGeneration calls")
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
	assertpkg.Equal(t, 1, run, "RunOnce calls (matching active should top up)")
	assertpkg.Equal(t, vector.GenerationID(9), gen, "RunOnce gen")
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
	assertpkg.Equal(t, 0, run, "RunOnce calls (refuse to drain without fingerprint)")
	assertpkg.Empty(t, backend.activations(), "ActivateGeneration calls")
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
	assertpkg.Equal(t, 0, run, "RunOnce calls (nothing to do)")
}

func TestEmbedJob_Run_ReclaimStaleFailureContinues(t *testing.T) {
	backend := &fakeBackend{active: vector.Generation{ID: 3}}
	runner := &fakeRunner{reclaimErr: errors.New("boom")}
	job := &EmbedJob{Worker: runner, Backend: backend}

	job.Run(context.Background())

	_, run, gen := runner.calls()
	assertpkg.Equal(t, 1, run, "RunOnce calls (should proceed despite reclaim error)")
	assertpkg.Equal(t, vector.GenerationID(3), gen, "RunOnce gen")
}

func TestEmbedJob_Run_ActiveGenerationError(t *testing.T) {
	backend := &fakeBackend{activeErr: errors.New("db failure")}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	job.Run(context.Background())

	_, run, _ := runner.calls()
	assertpkg.Equal(t, 0, run, "RunOnce calls on active lookup error")
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
	// Pending count = 0 (no VectorsDB wired), so the activation gate
	// will skip auto-activation; we're only asserting the target
	// selection here.
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend, Fingerprint: "m:768"}

	job.Run(context.Background())

	_, _, gen := runner.calls()
	assertpkg.Equal(t, vector.GenerationID(99), gen,
		"RunOnce gen should be building (%d) — active (%d) would strand the rebuild",
		building.ID, backend.active.ID)
}

// TestEmbedJob_Run_ActivatesBuildingWhenDrained verifies the
// activation gate: after RunOnce on a building generation, if
// pending_embeddings is empty for that gen, the daemon must call
// ActivateGeneration so the new index actually starts serving.
// Without this, a daemon-only deployment can never complete a
// `--full-rebuild` started by the CLI.
func TestEmbedJob_Run_ActivatesBuildingWhenDrained(t *testing.T) {
	db := newPendingDB(t)
	building := &vector.Generation{ID: 77, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{
		activeErr: vector.ErrNoActiveGeneration,
		building:  building,
	}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend, VectorsDB: db, Fingerprint: "m:768"}

	job.Run(context.Background())

	assertpkg.Equal(t, []vector.GenerationID{77}, backend.activations(), "activations")
}

// TestEmbedJob_Run_DoesNotActivateWhilePending guards the inverse
// case: pending_embeddings still has rows, so the building must NOT
// be activated yet (its index is incomplete).
func TestEmbedJob_Run_DoesNotActivateWhilePending(t *testing.T) {
	db := newPendingDB(t)
	_, err := db.Exec(`INSERT INTO pending_embeddings (generation_id, message_id) VALUES (77, 1)`)
	requirepkg.NoError(t, err, "seed pending")
	building := &vector.Generation{ID: 77, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{
		activeErr: vector.ErrNoActiveGeneration,
		building:  building,
	}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend, VectorsDB: db, Fingerprint: "m:768"}

	job.Run(context.Background())

	assertpkg.Empty(t, backend.activations(), "activations (pending still > 0)")
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
	assertpkg.Equal(t, 0, run, "RunOnce calls (mismatched build must be left alone)")
	assertpkg.Empty(t, backend.activations(), "activations")
}

// TestEmbedJob_Run_EnsuresSeededBeforeRunOnce regresses the crash
// window where CreateGeneration inserted a `building` row but died
// before committing the initial seed. Without EnsureSeeded on the
// resume path, RunOnce would see an empty queue, pendingCount would
// be 0, and the daemon would activate an unseeded generation — a
// silent, catastrophic data loss for semantic search. EnsureSeeded
// must be called BEFORE RunOnce so the seed commits first.
func TestEmbedJob_Run_EnsuresSeededBeforeRunOnce(t *testing.T) {
	db := newPendingDB(t)
	building := &vector.Generation{ID: 99, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{
		activeErr: vector.ErrNoActiveGeneration,
		building:  building,
	}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend, VectorsDB: db, Fingerprint: "m:768"}

	job.Run(context.Background())

	assertpkg.Equal(t, []vector.GenerationID{99}, backend.ensureSeededCalls(), "EnsureSeeded calls")
	_, run, _ := runner.calls()
	assertpkg.Equal(t, 1, run, "RunOnce calls (should run after seeding)")
}

// TestEmbedJob_Run_EnsureSeededErrorBailsOut guards the error path:
// if EnsureSeeded returns an error (e.g. the generation was already
// activated or retired between BuildingGeneration and EnsureSeeded),
// the daemon must NOT call RunOnce or ActivateGeneration — the
// generation is not in a state the daemon can safely drive.
func TestEmbedJob_Run_EnsureSeededErrorBailsOut(t *testing.T) {
	db := newPendingDB(t)
	building := &vector.Generation{ID: 55, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{
		activeErr:       vector.ErrNoActiveGeneration,
		building:        building,
		ensureSeededErr: errors.New("generation state=active, want building"),
	}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend, VectorsDB: db, Fingerprint: "m:768"}

	job.Run(context.Background())

	_, run, _ := runner.calls()
	assertpkg.Equal(t, 0, run, "RunOnce calls (EnsureSeeded failed — must not proceed)")
	assertpkg.Empty(t, backend.activations(), "activations (EnsureSeeded failed)")
}

// TestEmbedJob_Run_PostActivationEnqueueDrainsOnNextRun is the
// eventual-consistency check that pairs with the comment in
// embed_job.go's activation gate. It simulates the race the gate is
// designed to tolerate: pendingCount reads 0, activation flips
// state to active, then a new pending row appears (as if a sync
// committed between the read and the activate). The next worker
// run must pick the now-active generation as its target — proving
// the post-activation top-up path runs and the system converges.
func TestEmbedJob_Run_PostActivationEnqueueDrainsOnNextRun(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	db := newPendingDB(t)
	gen := vector.Generation{ID: 88, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{
		activeErr: vector.ErrNoActiveGeneration,
		building:  &gen,
	}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend, VectorsDB: db, Fingerprint: "m:768"}

	// Tick 1: building drained, activation flips to active.
	job.Run(context.Background())
	require.Equal([]vector.GenerationID{88}, backend.activations(), "tick 1 activations")

	// Simulate the race: a sync.EnqueueMessages commit lands AFTER
	// activation, adding a pending row bound to the (now-active)
	// generation. The fakeBackend reflects the post-activation state.
	_, err := db.Exec(`INSERT INTO pending_embeddings (generation_id, message_id) VALUES (88, 1)`)
	require.NoError(err, "enqueue")
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

// newPendingDB returns an in-memory SQLite handle with just the
// pending_embeddings table the activation gate counts against.
func newPendingDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	requirepkg.NoError(t, err, "open")
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
CREATE TABLE pending_embeddings (
    generation_id INTEGER NOT NULL,
    message_id    INTEGER NOT NULL,
    PRIMARY KEY (generation_id, message_id)
);`)
	requirepkg.NoError(t, err, "schema")
	return db
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

func (r *slowRunner) RunOnce(context.Context, vector.GenerationID) (embed.RunResult, error) {
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
		requirepkg.Fail(t, "first RunOnce did not start")
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
		requirepkg.Fail(t, "second Run blocked; TryLock guard did not short-circuit")
	}

	assertpkg.Equal(t, 1, runner.calls(), "RunOnce calls during overlap")

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
	assertpkg.Equal(t, 0, run, "nil-safe Run should not invoke worker")
}

// ---------- SetEmbedJob tests ----------

func TestScheduler_SetEmbedJob_AddsCronEntry(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	requirepkg.Error(t, err, "SetEmbedJob with invalid cron")
	assertpkg.False(t, s.embedEntrySet, "embedEntrySet should remain false after invalid cron")
}

func TestScheduler_SetEmbedJob_InvalidReplacePreservesPrevious(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	assert := assertpkg.New(t)
	s := New(func(ctx context.Context, email string) error { return nil })
	backend := &fakeBackend{}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	requirepkg.NoError(t, s.SetEmbedJob(job, "", true), "SetEmbedJob")
	assert.False(s.embedEntrySet, "empty schedule should not create a cron entry")
	assert.NotNil(s.embedJob, "embedJob should be set even with empty schedule")
	assert.True(s.runEmbedAfterSync, "runEmbedAfterSync should be true")
}

func TestScheduler_RunAfterSync_Fires(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require := requirepkg.New(t)
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
	assertpkg.Equal(t, 0, run, "RunOnce calls when runAfterSync is false")
}

func TestScheduler_RunAfterSync_SkipOnStopped(t *testing.T) {
	require := requirepkg.New(t)
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
	assertpkg.Equal(t, 0, run, "RunOnce calls when scheduler is stopped")
}

func TestScheduler_RunAfterSync_SkipOnSyncError(t *testing.T) {
	require := requirepkg.New(t)
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
	assertpkg.Equal(t, 0, run, "RunOnce calls when sync failed")
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
				assertpkg.Error(t, err, "ValidateCronExpr(%q)", tt.expr)
			} else {
				assertpkg.NoError(t, err, "ValidateCronExpr(%q)", tt.expr)
			}
		})
	}
}
