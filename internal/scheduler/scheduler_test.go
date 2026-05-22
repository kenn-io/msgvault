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

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

func TestNew(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	if s == nil {
		t.Fatal("New() returned nil")
	}
	if s.cron == nil {
		t.Error("cron is nil")
	}
	if s.jobs == nil {
		t.Error("jobs map is nil")
	}
}

func TestAddAccount(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	// Valid cron expression
	if err := s.AddAccount("test@gmail.com", "0 2 * * *"); err != nil {
		t.Errorf("AddAccount() with valid cron = %v, want nil", err)
	}

	// Check job was added
	s.mu.RLock()
	_, exists := s.jobs["test@gmail.com"]
	s.mu.RUnlock()

	if !exists {
		t.Error("job was not added to jobs map")
	}
}

func TestAddAccountInvalidCron(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	err := s.AddAccount("test@gmail.com", "invalid cron")
	if err == nil {
		t.Error("AddAccount() with invalid cron = nil, want error")
	}
}

func TestAddAccountReplacesExisting(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	// Add initial schedule
	if err := s.AddAccount("test@gmail.com", "0 2 * * *"); err != nil {
		t.Fatalf("AddAccount() = %v", err)
	}

	s.mu.RLock()
	firstID := s.jobs["test@gmail.com"]
	s.mu.RUnlock()

	// Replace with new schedule
	if err := s.AddAccount("test@gmail.com", "0 3 * * *"); err != nil {
		t.Fatalf("AddAccount() replacement = %v", err)
	}

	s.mu.RLock()
	secondID := s.jobs["test@gmail.com"]
	s.mu.RUnlock()

	if firstID == secondID {
		t.Error("job ID was not updated after replacement")
	}
}

func TestRemoveAccount(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	if err := s.AddAccount("test@gmail.com", "0 2 * * *"); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	s.RemoveAccount("test@gmail.com")

	s.mu.RLock()
	_, exists := s.jobs["test@gmail.com"]
	s.mu.RUnlock()

	if exists {
		t.Error("job still exists after RemoveAccount()")
	}
}

func TestRemoveAccountNonExistent(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	// Should not panic
	s.RemoveAccount("nonexistent@gmail.com")
}

func TestAddAccountsFromConfig(t *testing.T) {
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

	if len(errs) != 0 {
		t.Errorf("AddAccountsFromConfig() errors = %v", errs)
	}
	if scheduled != 2 {
		t.Errorf("AddAccountsFromConfig() scheduled = %d, want 2", scheduled)
	}

	// Check only enabled accounts with schedules were added
	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.jobs["user1@gmail.com"]; !ok {
		t.Error("user1@gmail.com should be scheduled")
	}
	if _, ok := s.jobs["user2@gmail.com"]; !ok {
		t.Error("user2@gmail.com should be scheduled")
	}
	if _, ok := s.jobs["disabled@gmail.com"]; ok {
		t.Error("disabled@gmail.com should not be scheduled")
	}
	if _, ok := s.jobs["noschedule@gmail.com"]; ok {
		t.Error("noschedule@gmail.com should not be scheduled")
	}
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

	if scheduled != 1 {
		t.Errorf("scheduled = %d, want 1", scheduled)
	}
	if len(errs) != 1 {
		t.Errorf("len(errs) = %d, want 1", len(errs))
	}
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
		t.Error("Stop() did not complete in time")
	}
}

func TestIsRunning(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	// Not running before Start
	if s.IsRunning() {
		t.Error("IsRunning() = true before Start()")
	}

	s.Start()

	// Running after Start
	if !s.IsRunning() {
		t.Error("IsRunning() = false after Start()")
	}

	ctx := s.Stop()

	// Not running after Stop
	if s.IsRunning() {
		t.Error("IsRunning() = true after Stop()")
	}

	// Wait for stop
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Error("Stop() did not complete in time")
	}
}

func TestStopCancelsRunningSync(t *testing.T) {
	syncStarted := make(chan struct{})
	s := New(func(ctx context.Context, email string) error {
		close(syncStarted)
		<-ctx.Done()
		return ctx.Err()
	})

	if err := s.AddAccount("test@gmail.com", "0 0 1 1 *"); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	// Trigger sync
	if err := s.TriggerSync("test@gmail.com"); err != nil {
		t.Fatalf("TriggerSync: %v", err)
	}

	// Wait for sync to start
	select {
	case <-syncStarted:
	case <-time.After(time.Second):
		t.Fatal("sync did not start")
	}

	// Stop should cancel the running sync
	ctx := s.Stop()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Error("Stop() did not complete after cancelling sync")
	}

	// Verify the error was recorded
	statuses := s.Status()
	for _, status := range statuses {
		if status.Email == "test@gmail.com" {
			if status.LastError == "" {
				t.Error("expected error after cancelled sync")
			}
			return
		}
	}
}

func TestTriggerSync(t *testing.T) {
	var called atomic.Int32
	s := New(func(ctx context.Context, email string) error {
		called.Add(1)
		time.Sleep(50 * time.Millisecond)
		return nil
	})

	if err := s.AddAccount("test@gmail.com", "0 0 1 1 *"); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	// Trigger manually
	err := s.TriggerSync("test@gmail.com")
	if err != nil {
		t.Errorf("TriggerSync() = %v", err)
	}

	// Wait for sync to start
	time.Sleep(10 * time.Millisecond)

	// Second trigger should fail (already running)
	err = s.TriggerSync("test@gmail.com")
	if err == nil {
		t.Error("TriggerSync() while running = nil, want error")
	}

	// Wait for completion
	time.Sleep(100 * time.Millisecond)

	if called.Load() != 1 {
		t.Errorf("syncFunc called %d times, want 1", called.Load())
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

	if err := s.AddAccount("test@gmail.com", "0 0 1 1 *"); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	// Try to trigger multiple times concurrently
	for i := 0; i < 5; i++ {
		_ = s.TriggerSync("test@gmail.com")
	}

	time.Sleep(200 * time.Millisecond)

	if maxConcurrent.Load() > 1 {
		t.Errorf("max concurrent = %d, want 1", maxConcurrent.Load())
	}
}

func TestStatus(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	if err := s.AddAccount("test@gmail.com", "0 2 * * *"); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := s.AddAccount("other@gmail.com", "0 3 * * *"); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	s.Start()
	defer s.Stop()

	statuses := s.Status()

	if len(statuses) != 2 {
		t.Errorf("len(Status()) = %d, want 2", len(statuses))
	}

	// Find test@gmail.com status
	var found bool
	for _, status := range statuses {
		if status.Email == "test@gmail.com" {
			found = true
			if status.Running {
				t.Error("status.Running = true, want false")
			}
			if status.NextRun.IsZero() {
				t.Error("status.NextRun is zero")
			}
			break
		}
	}
	if !found {
		t.Error("test@gmail.com not found in status")
	}
}

func TestStatusAfterSyncSuccess(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	if err := s.AddAccount("test@gmail.com", "0 0 1 1 *"); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := s.TriggerSync("test@gmail.com"); err != nil {
		t.Fatalf("TriggerSync: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	statuses := s.Status()
	for _, status := range statuses {
		if status.Email == "test@gmail.com" {
			if status.LastRun.IsZero() {
				t.Error("LastRun should be set after successful sync")
			}
			if status.LastError != "" {
				t.Errorf("LastError = %q, want empty", status.LastError)
			}
			return
		}
	}
	t.Error("test@gmail.com not found in status")
}

func TestStatusAfterSyncError(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return errors.New("sync failed")
	})

	if err := s.AddAccount("test@gmail.com", "0 0 1 1 *"); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := s.TriggerSync("test@gmail.com"); err != nil {
		t.Fatalf("TriggerSync: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	statuses := s.Status()
	for _, status := range statuses {
		if status.Email == "test@gmail.com" {
			if status.LastError == "" {
				t.Error("LastError should be set after failed sync")
			}
			return
		}
	}
	t.Error("test@gmail.com not found in status")
}

func TestTriggerSyncAfterStop(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return nil
	})

	if err := s.AddAccount("test@gmail.com", "0 0 1 1 *"); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	ctx := s.Stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Stop() did not complete in time")
	}

	err := s.TriggerSync("test@gmail.com")
	if err == nil {
		t.Error("TriggerSync() after Stop() = nil, want error")
	}
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

func (f *fakeBackend) CreateGeneration(ctx context.Context, model string, dim int) (vector.GenerationID, error) {
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
	backend := &fakeBackend{active: vector.Generation{ID: 5, State: vector.GenerationActive}}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	job.Run(context.Background())

	reclaim, run, gen := runner.calls()
	if reclaim != 1 {
		t.Errorf("ReclaimStale calls = %d, want 1", reclaim)
	}
	if run != 1 {
		t.Errorf("RunOnce calls = %d, want 1", run)
	}
	if gen != 5 {
		t.Errorf("RunOnce gen = %d, want 5", gen)
	}
	// New precedence: BuildingGeneration is consulted first; with no
	// building present we then fall through to active. Activation
	// must NOT fire for the active gen.
	if got := backend.activations(); len(got) != 0 {
		t.Errorf("ActivateGeneration calls = %v, want none (target was active)", got)
	}
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
	if run != 0 {
		t.Errorf("RunOnce calls = %d, want 0 (refuse to drain without fingerprint)", run)
	}
	if got := backend.activations(); len(got) != 0 {
		t.Errorf("ActivateGeneration calls = %v, want none", got)
	}
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
	if run != 0 {
		t.Errorf("RunOnce calls = %d, want 0 (nothing to do)", run)
	}
}

func TestEmbedJob_Run_ReclaimStaleFailureContinues(t *testing.T) {
	backend := &fakeBackend{active: vector.Generation{ID: 3}}
	runner := &fakeRunner{reclaimErr: errors.New("boom")}
	job := &EmbedJob{Worker: runner, Backend: backend}

	job.Run(context.Background())

	_, run, gen := runner.calls()
	if run != 1 {
		t.Errorf("RunOnce calls = %d, want 1 (should proceed despite reclaim error)", run)
	}
	if gen != 3 {
		t.Errorf("RunOnce gen = %d, want 3", gen)
	}
}

func TestEmbedJob_Run_ActiveGenerationError(t *testing.T) {
	backend := &fakeBackend{activeErr: errors.New("db failure")}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	job.Run(context.Background())

	_, run, _ := runner.calls()
	if run != 0 {
		t.Errorf("RunOnce calls = %d, want 0 on active lookup error", run)
	}
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
	if gen != 99 {
		t.Errorf("RunOnce gen = %d, want building (%d) — active (%d) would strand the rebuild",
			gen, building.ID, backend.active.ID)
	}
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

	got := backend.activations()
	if len(got) != 1 || got[0] != 77 {
		t.Errorf("activations = %v, want [77]", got)
	}
}

// TestEmbedJob_Run_DoesNotActivateWhilePending guards the inverse
// case: pending_embeddings still has rows, so the building must NOT
// be activated yet (its index is incomplete).
func TestEmbedJob_Run_DoesNotActivateWhilePending(t *testing.T) {
	db := newPendingDB(t)
	if _, err := db.Exec(`INSERT INTO pending_embeddings (generation_id, message_id) VALUES (77, 1)`); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	building := &vector.Generation{ID: 77, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{
		activeErr: vector.ErrNoActiveGeneration,
		building:  building,
	}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend, VectorsDB: db, Fingerprint: "m:768"}

	job.Run(context.Background())

	if got := backend.activations(); len(got) != 0 {
		t.Errorf("activations = %v, want none (pending still > 0)", got)
	}
}

// TestEmbedJob_Run_LeavesMismatchedBuildingForCLI guards against the
// daemon silently topping up an unrelated rebuild. When a building
// generation's fingerprint differs from the configured one, the
// daemon must bail out so the operator can resolve via the CLI
// (`msgvault build-embeddings --full-rebuild` or retire the stale build).
func TestEmbedJob_Run_LeavesMismatchedBuildingForCLI(t *testing.T) {
	building := &vector.Generation{ID: 33, State: vector.GenerationBuilding, Fingerprint: "old:512"}
	backend := &fakeBackend{
		active:   vector.Generation{ID: 5, State: vector.GenerationActive, Fingerprint: "new:768"},
		building: building,
	}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend, Fingerprint: "new:768"}

	job.Run(context.Background())

	if _, run, _ := runner.calls(); run != 0 {
		t.Errorf("RunOnce calls = %d, want 0 (mismatched build must be left alone)", run)
	}
	if got := backend.activations(); len(got) != 0 {
		t.Errorf("activations = %v, want none", got)
	}
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

	got := backend.ensureSeededCalls()
	if len(got) != 1 || got[0] != 99 {
		t.Errorf("EnsureSeeded calls = %v, want [99]", got)
	}
	if _, run, _ := runner.calls(); run != 1 {
		t.Errorf("RunOnce calls = %d, want 1 (should run after seeding)", run)
	}
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

	if _, run, _ := runner.calls(); run != 0 {
		t.Errorf("RunOnce calls = %d, want 0 (EnsureSeeded failed — must not proceed)", run)
	}
	if got := backend.activations(); len(got) != 0 {
		t.Errorf("activations = %v, want none (EnsureSeeded failed)", got)
	}
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
	if got := backend.activations(); len(got) != 1 || got[0] != 88 {
		t.Fatalf("tick 1 activations = %v, want [88]", got)
	}

	// Simulate the race: a sync.EnqueueMessages commit lands AFTER
	// activation, adding a pending row bound to the (now-active)
	// generation. The fakeBackend reflects the post-activation state.
	if _, err := db.Exec(`INSERT INTO pending_embeddings (generation_id, message_id) VALUES (88, 1)`); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	backend.building = nil
	backend.active = vector.Generation{ID: 88, State: vector.GenerationActive, Fingerprint: "m:768"}
	backend.activeErr = nil

	// Tick 2: the active path picks it up and drains.
	job.Run(context.Background())
	_, run, gen2 := runner.calls()
	if run != 2 || gen2 != 88 {
		t.Errorf("tick 2 RunOnce calls=%d gen=%d, want 2 / 88", run, gen2)
	}
	// Activation must NOT fire a second time (idempotency: active-mode
	// runs never call ActivateGeneration).
	if got := backend.activations(); len(got) != 1 {
		t.Errorf("activations = %v, want only the first activation", got)
	}
}

// newPendingDB returns an in-memory SQLite handle with just the
// pending_embeddings table the activation gate counts against.
func newPendingDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
CREATE TABLE pending_embeddings (
    generation_id INTEGER NOT NULL,
    message_id    INTEGER NOT NULL,
    PRIMARY KEY (generation_id, message_id)
);`); err != nil {
		t.Fatalf("schema: %v", err)
	}
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
		t.Fatal("first RunOnce did not start")
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
		t.Fatal("second Run blocked; TryLock guard did not short-circuit")
	}

	if got := runner.calls(); got != 1 {
		t.Errorf("RunOnce calls = %d during overlap, want 1", got)
	}

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
	if run != 0 {
		t.Errorf("nil-safe Run should not invoke worker; got runCalls=%d", run)
	}
}

// ---------- SetEmbedJob tests ----------

func TestScheduler_SetEmbedJob_AddsCronEntry(t *testing.T) {
	s := New(func(ctx context.Context, email string) error { return nil })
	backend := &fakeBackend{active: vector.Generation{ID: 1}}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	if err := s.SetEmbedJob(job, "*/5 * * * *", false); err != nil {
		t.Fatalf("SetEmbedJob first = %v", err)
	}
	if !s.embedEntrySet {
		t.Error("embedEntrySet should be true after first SetEmbedJob")
	}

	// Replacing with a new schedule should not error.
	if err := s.SetEmbedJob(job, "0 * * * *", true); err != nil {
		t.Fatalf("SetEmbedJob replace = %v", err)
	}
	if !s.embedEntrySet {
		t.Error("embedEntrySet should remain true after replacement")
	}
	if !s.runEmbedAfterSync {
		t.Error("runEmbedAfterSync should be true after replacement with runAfterSync=true")
	}

	// Clearing.
	if err := s.SetEmbedJob(nil, "", false); err != nil {
		t.Fatalf("SetEmbedJob clear = %v", err)
	}
	if s.embedEntrySet {
		t.Error("embedEntrySet should be false after clear")
	}
	if s.embedJob != nil {
		t.Error("embedJob should be nil after clear")
	}
	if s.runEmbedAfterSync {
		t.Error("runEmbedAfterSync should be false after clear")
	}
}

func TestScheduler_SetEmbedJob_InvalidCron(t *testing.T) {
	s := New(func(ctx context.Context, email string) error { return nil })
	backend := &fakeBackend{}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	err := s.SetEmbedJob(job, "not a cron", false)
	if err == nil {
		t.Fatal("SetEmbedJob with invalid cron = nil, want error")
	}
	if s.embedEntrySet {
		t.Error("embedEntrySet should remain false after invalid cron")
	}
}

func TestScheduler_SetEmbedJob_InvalidReplacePreservesPrevious(t *testing.T) {
	// After a successful SetEmbedJob, a later call with an invalid cron
	// must leave the previous job, schedule, and post-sync flag intact.
	s := New(func(ctx context.Context, email string) error { return nil })
	backend := &fakeBackend{}
	job1 := &EmbedJob{Worker: &fakeRunner{}, Backend: backend}
	job2 := &EmbedJob{Worker: &fakeRunner{}, Backend: backend}

	if err := s.SetEmbedJob(job1, "*/5 * * * *", true); err != nil {
		t.Fatalf("SetEmbedJob(job1) = %v", err)
	}
	prevEntry := s.embedEntry

	if err := s.SetEmbedJob(job2, "bogus cron", true); err == nil {
		t.Fatal("SetEmbedJob(job2, invalid) = nil, want error")
	}

	if s.embedJob != job1 {
		t.Errorf("embedJob was replaced on invalid cron; want job1")
	}
	if !s.runEmbedAfterSync {
		t.Error("runEmbedAfterSync should remain true")
	}
	if !s.embedEntrySet || s.embedEntry != prevEntry {
		t.Errorf("cron entry should still be job1's (entrySet=%v, entry=%v, want %v)",
			s.embedEntrySet, s.embedEntry, prevEntry)
	}
}

func TestScheduler_SetEmbedJob_EmptyScheduleNoCronEntry(t *testing.T) {
	s := New(func(ctx context.Context, email string) error { return nil })
	backend := &fakeBackend{}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	if err := s.SetEmbedJob(job, "", true); err != nil {
		t.Fatalf("SetEmbedJob = %v", err)
	}
	if s.embedEntrySet {
		t.Error("empty schedule should not create a cron entry")
	}
	if s.embedJob == nil {
		t.Error("embedJob should be set even with empty schedule")
	}
	if !s.runEmbedAfterSync {
		t.Error("runEmbedAfterSync should be true")
	}
}

func TestScheduler_RunAfterSync_Fires(t *testing.T) {
	syncDone := make(chan struct{})
	s := New(func(ctx context.Context, email string) error {
		close(syncDone)
		return nil
	})
	backend := &fakeBackend{active: vector.Generation{ID: 42}}
	runDone := make(chan struct{})
	runner := &fakeRunner{runDone: runDone}
	job := &EmbedJob{Worker: runner, Backend: backend}

	if err := s.SetEmbedJob(job, "", true); err != nil {
		t.Fatalf("SetEmbedJob = %v", err)
	}
	if err := s.AddAccount("test@gmail.com", "0 0 1 1 *"); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	s.Start()
	defer func() {
		ctx := s.Stop()
		<-ctx.Done()
	}()

	if err := s.TriggerSync("test@gmail.com"); err != nil {
		t.Fatalf("TriggerSync: %v", err)
	}

	select {
	case <-syncDone:
	case <-time.After(time.Second):
		t.Fatal("syncFunc did not run")
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("embed RunOnce did not fire after sync")
	}

	_, run, gen := runner.calls()
	if run != 1 {
		t.Errorf("RunOnce calls = %d, want 1", run)
	}
	if gen != 42 {
		t.Errorf("RunOnce gen = %d, want 42", gen)
	}
}

func TestScheduler_RunAfterSync_DisabledDoesNotFire(t *testing.T) {
	s := New(func(ctx context.Context, email string) error { return nil })
	backend := &fakeBackend{active: vector.Generation{ID: 1}}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	// runAfterSync = false
	if err := s.SetEmbedJob(job, "", false); err != nil {
		t.Fatalf("SetEmbedJob = %v", err)
	}
	if err := s.AddAccount("test@gmail.com", "0 0 1 1 *"); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	s.Start()
	defer func() {
		ctx := s.Stop()
		<-ctx.Done()
	}()

	if err := s.TriggerSync("test@gmail.com"); err != nil {
		t.Fatalf("TriggerSync: %v", err)
	}

	// Give runSync a chance to finish.
	time.Sleep(50 * time.Millisecond)

	_, run, _ := runner.calls()
	if run != 0 {
		t.Errorf("RunOnce calls = %d, want 0 when runAfterSync is false", run)
	}
}

func TestScheduler_RunAfterSync_SkipOnStopped(t *testing.T) {
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

	if err := s.SetEmbedJob(job, "", true); err != nil {
		t.Fatalf("SetEmbedJob = %v", err)
	}
	if err := s.AddAccount("test@gmail.com", "0 0 1 1 *"); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	s.Start()
	if err := s.TriggerSync("test@gmail.com"); err != nil {
		t.Fatalf("TriggerSync: %v", err)
	}

	// Ask the scheduler to stop while the sync is still in-flight.
	stopCtx := s.Stop()
	close(release) // let the sync complete
	<-stopCtx.Done()

	_, run, _ := runner.calls()
	if run != 0 {
		t.Errorf("RunOnce calls = %d, want 0 when scheduler is stopped", run)
	}
}

func TestScheduler_RunAfterSync_SkipOnSyncError(t *testing.T) {
	s := New(func(ctx context.Context, email string) error {
		return errors.New("sync failed")
	})
	backend := &fakeBackend{active: vector.Generation{ID: 1}}
	runner := &fakeRunner{}
	job := &EmbedJob{Worker: runner, Backend: backend}

	if err := s.SetEmbedJob(job, "", true); err != nil {
		t.Fatalf("SetEmbedJob = %v", err)
	}
	if err := s.AddAccount("test@gmail.com", "0 0 1 1 *"); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	s.Start()
	defer func() {
		ctx := s.Stop()
		<-ctx.Done()
	}()

	if err := s.TriggerSync("test@gmail.com"); err != nil {
		t.Fatalf("TriggerSync: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	_, run, _ := runner.calls()
	if run != 0 {
		t.Errorf("RunOnce calls = %d, want 0 when sync failed", run)
	}
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
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateCronExpr(%q) error = %v, wantErr = %v", tt.expr, err, tt.wantErr)
			}
		})
	}
}
