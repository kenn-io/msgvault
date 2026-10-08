// Package scheduler provides cron-based scheduling for automated email sync.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/syncerr"
)

// SyncFunc is the callback invoked when a scheduled sync should run.
// It receives the account email and should perform incremental sync + cache build.
type SyncFunc func(ctx context.Context, email string) error

type WorkTracker interface {
	BeginWork() (func(), bool)
	BeginWorkContext(ctx context.Context) (func(), bool)
}

// LabeledWorkTracker lets the scheduler identify the job holding the gate.
type LabeledWorkTracker interface {
	BeginLabeledWorkContext(ctx context.Context, label string) (func(), bool)
}

// YieldChecker is optionally implemented by work trackers that can report a
// waiter the running job should yield to. Scheduled jobs are resumable, so
// they step aside rather than block an interactive command for their whole
// runtime.
type YieldChecker interface {
	ShouldYield() bool
}

// ErrYieldedToWaiter is the cancellation cause set when a scheduled job is
// interrupted to let a waiting operation acquire the work gate.
var ErrYieldedToWaiter = jobctx.ErrYieldedToWaiter

// ErrReschedule asks the scheduler to run another bounded pass after queued
// work. It does not record a job failure or wait for the next cron tick.
var ErrReschedule = errors.New("scheduled job has more work")

// yieldPollInterval is how often a running scheduled job checks for waiters.
// Variable only so tests can shorten it.
var yieldPollInterval = 5 * time.Second

// AccountStatus represents the sync status of a scheduled account.
type AccountStatus struct {
	Email     string    `json:"email"`
	Running   bool      `json:"running"`
	LastRun   time.Time `json:"last_run,omitzero"`
	NextRun   time.Time `json:"next_run"`
	Schedule  string    `json:"schedule"`
	LastError string    `json:"last_error,omitempty"`
	// Queued reports a run waiting for the daemon operation gate.
	Queued bool `json:"queued,omitempty"`
	// Pending reports a follow-up run requested while the current run was
	// executing or yielded; it queues when the current run finishes.
	Pending bool `json:"pending,omitempty"`
	// StartedAt is when the current run began executing (after the gate).
	StartedAt time.Time `json:"started_at,omitzero"`
}

type Job struct {
	Name     string
	Schedule string
	Run      func(context.Context) error
	// Preemptible permits cancellation for another scheduled job. Enable only
	// when the job can resume from persisted progress after cancellation.
	Preemptible bool
	// MaxRuntime bounds a pass after it acquires the gate. On expiry the
	// scheduler queues a follow-up only if the pass committed progress.
	// Expiry without progress reports an error and waits for the next trigger.
	// Enable only for jobs that can resume committed progress. Zero disables it.
	MaxRuntime time.Duration
}

type JobStatus struct {
	Name      string    `json:"name"`
	Running   bool      `json:"running"`
	LastRun   time.Time `json:"last_run,omitzero"`
	NextRun   time.Time `json:"next_run"`
	Schedule  string    `json:"schedule"`
	LastError string    `json:"last_error,omitempty"`
	Queued    bool      `json:"queued,omitempty"`
	Pending   bool      `json:"pending,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
}

// Scheduler manages cron-based email sync scheduling.
type Scheduler struct {
	cron                    *cron.Cron
	syncFunc                SyncFunc
	logger                  *slog.Logger
	work                    WorkTracker
	accountPreemptionPolicy func(string) bool

	mu        sync.RWMutex
	jobs      map[string]cron.EntryID // email -> cron entry ID
	schedules map[string]string       // email -> cron expression
	running   map[string]bool         // email -> currently syncing
	lastRun   map[string]time.Time    // email -> last successful run
	lastErr   map[string]error        // email -> last error
	queued    map[string]bool         // email -> run waiting for the gate
	pending   map[string]bool         // email -> follow-up requested by a tick or yield
	startedAt map[string]time.Time    // email -> current run began executing

	genericJobs        map[string]cron.EntryID
	genericSchedules   map[string]string
	genericRunning     map[string]bool
	genericLastRun     map[string]time.Time
	genericLastErr     map[string]error
	genericFuncs       map[string]func(context.Context) error
	genericPreemptible map[string]bool
	genericMaxRuntime  map[string]time.Duration
	genericQueued      map[string]bool
	genericPending     map[string]bool
	genericStartedAt   map[string]time.Time

	// queuedRuns counts runs blocked waiting for the work gate.
	queuedRuns int

	// Embed job state (optional). Set via SetEmbedJob; cron.EntryID 0
	// may be valid, so embedEntrySet tracks whether an entry exists.
	embedJob                   *EmbedJob
	embedEntry                 cron.EntryID
	embedEntrySet              bool
	runEmbedAfterSync          bool
	visualPostSync             func(context.Context) error
	visualPostRunning          bool
	visualPostPending          bool
	documentVectorJob          func(context.Context) error
	documentVectorEntry        cron.EntryID
	documentVectorEntrySet     bool
	runDocumentVectorAfterSync bool

	ctx     context.Context    // cancelled on Stop
	cancel  context.CancelFunc // cancels ctx
	wg      sync.WaitGroup     // tracks running sync goroutines
	started bool               // true after Start(), false after Stop()
	stopped bool               // true after Stop()
}

// SetVisualPostSyncJob installs the independently consented visual lane's
// bounded post-sync pass. Nil disables the hook.
func (s *Scheduler) SetVisualPostSyncJob(run func(context.Context) error) {
	s.mu.Lock()
	s.visualPostSync = run
	s.mu.Unlock()
}

// New creates a new Scheduler with the given sync callback.
func New(syncFunc SyncFunc) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		cron: cron.New(cron.WithParser(cron.NewParser(
			cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
		))),
		syncFunc:           syncFunc,
		logger:             slog.Default(),
		jobs:               make(map[string]cron.EntryID),
		schedules:          make(map[string]string),
		running:            make(map[string]bool),
		lastRun:            make(map[string]time.Time),
		lastErr:            make(map[string]error),
		queued:             make(map[string]bool),
		pending:            make(map[string]bool),
		startedAt:          make(map[string]time.Time),
		genericJobs:        make(map[string]cron.EntryID),
		genericSchedules:   make(map[string]string),
		genericRunning:     make(map[string]bool),
		genericLastRun:     make(map[string]time.Time),
		genericLastErr:     make(map[string]error),
		genericFuncs:       make(map[string]func(context.Context) error),
		genericPreemptible: make(map[string]bool),
		genericMaxRuntime:  make(map[string]time.Duration),
		genericQueued:      make(map[string]bool),
		genericPending:     make(map[string]bool),
		genericStartedAt:   make(map[string]time.Time),
		ctx:                ctx,
		cancel:             cancel,
	}
}

// WithLogger sets the logger for the scheduler.
func (s *Scheduler) WithLogger(logger *slog.Logger) *Scheduler {
	s.logger = logger
	return s
}

func (s *Scheduler) WithWorkTracker(tracker WorkTracker) *Scheduler {
	s.work = tracker
	return s
}

// WithAccountPreemptionPolicy reports whether an account sync can safely
// yield to other scheduled work. When unset, account syncs remain preemptible.
func (s *Scheduler) WithAccountPreemptionPolicy(policy func(string) bool) *Scheduler {
	s.mu.Lock()
	s.accountPreemptionPolicy = policy
	s.mu.Unlock()
	return s
}

// AddAccount schedules sync for an account using the given cron expression.
// Returns an error if the cron expression is invalid.
func (s *Scheduler) AddAccount(email, cronExpr string) error {
	if err := ValidateCronExpr(cronExpr); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Remove existing schedule if present
	if entryID, exists := s.jobs[email]; exists {
		s.cron.Remove(entryID)
		delete(s.jobs, email)
		delete(s.schedules, email)
	}

	// Validate and add the cron job
	entryID, err := s.cron.AddFunc(cronExpr, func() {
		s.onAccountTick(email)
	})
	if err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", cronExpr, err)
	}

	s.jobs[email] = entryID
	s.schedules[email] = cronExpr
	s.logger.Info("scheduled sync",
		"email", email,
		"schedule", cronExpr,
		"next_run", s.nextRun(entryID))

	return nil
}

// onAccountTick handles one cron firing for an account. A tick that finds
// the previous run still waiting for the gate is redundant; one that finds it
// executing is remembered as a single follow-up run instead of being lost.
func (s *Scheduler) onAccountTick(email string) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	if s.running[email] {
		s.coalesceTickLocked("email", email, s.queued, s.pending)
		s.mu.Unlock()
		return
	}
	s.running[email] = true
	s.queued[email] = true
	s.wg.Add(1)
	s.mu.Unlock()
	s.runSync(email)
}

// coalesceTickLocked records a tick that arrived while its job was active.
// The caller holds s.mu.
// It reports whether a new follow-up run was recorded; false means an
// existing run or follow-up already covers the request.
func (s *Scheduler) coalesceTickLocked(kind, name string, queued, pending map[string]bool) bool {
	if queued[name] {
		s.logger.Debug("scheduled tick dropped: previous run is still waiting to start", kind, name)
		return false
	}
	if pending[name] {
		return false
	}
	pending[name] = true
	s.logger.Info("scheduled sync skipped: previous run still active; queued one follow-up run", kind, name)
	return true
}

// AddAccountsFromConfig adds all enabled accounts from the config.
// Returns the number of accounts scheduled and any errors encountered.
func (s *Scheduler) AddAccountsFromConfig(cfg *config.Config) (int, []error) {
	var errors []error
	scheduled := 0

	for _, acc := range cfg.ScheduledAccounts() {
		if err := s.AddAccount(acc.Email, acc.Schedule); err != nil {
			errors = append(errors, fmt.Errorf("%s: %w", acc.Email, err))
		} else {
			scheduled++
		}
	}

	return scheduled, errors
}

func (s *Scheduler) AddJob(job Job) error {
	if job.MaxRuntime < 0 {
		return errors.New("job runtime budget must be non-negative")
	}
	if job.Name == "" || job.Run == nil {
		return errors.New("job name and run function are required")
	}
	if err := ValidateCronExpr(job.Schedule); err != nil {
		return err
	}
	entryID, err := s.cron.AddFunc(job.Schedule, func() {
		s.onJobTick(job.Name)
	})
	if err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", job.Schedule, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, exists := s.genericJobs[job.Name]; exists {
		s.cron.Remove(old)
	}
	s.genericJobs[job.Name] = entryID
	s.genericSchedules[job.Name] = job.Schedule
	s.genericFuncs[job.Name] = job.Run
	s.genericPreemptible[job.Name] = job.Preemptible
	s.genericMaxRuntime[job.Name] = job.MaxRuntime
	s.logger.Info("scheduled job", "job", job.Name, "schedule", job.Schedule, "next_run", s.nextRun(entryID))
	return nil
}

// RemoveJob removes a generic job's cron entry and callable registration.
// An invocation already running is allowed to finish under the normal work
// gate; no future cron or manual invocation can start after removal returns.
func (s *Scheduler) RemoveJob(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entryID, exists := s.genericJobs[name]; exists {
		s.cron.Remove(entryID)
	}
	delete(s.genericJobs, name)
	delete(s.genericSchedules, name)
	delete(s.genericFuncs, name)
	delete(s.genericPreemptible, name)
	delete(s.genericMaxRuntime, name)
	delete(s.genericLastRun, name)
	delete(s.genericLastErr, name)
	if s.genericPending[name] {
		s.logger.Warn("scheduled job follow-up dropped without running",
			"job", name, "reason", "job removed")
		delete(s.genericPending, name)
	}
	s.logger.Info("removed scheduled job", "job", name)
}

// RemoveAccount removes the schedule for an account.
func (s *Scheduler) RemoveAccount(email string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if entryID, exists := s.jobs[email]; exists {
		s.cron.Remove(entryID)
		delete(s.jobs, email)
		delete(s.schedules, email)
		s.logger.Info("removed schedule", "email", email)
	}
	delete(s.pending, email)
}

// SetEmbedJob registers the embed job on a cron schedule. If schedule
// is empty, no cron entry is created (the job can still fire via the
// post-sync hook when runAfterSync is true). Replacing a previously-set
// job removes the old cron entry. Passing nil clears any existing job.
//
// A schedule rejected by ValidateCronExpr is caught before any state
// mutates, so the previous job and cron entry are preserved. An
// internal AddFunc failure after that point is treated as an invariant
// violation (ValidateCronExpr already accepted the expression) and
// clears the embed job rather than restoring the prior one.
func (s *Scheduler) SetEmbedJob(job *EmbedJob, schedule string, runAfterSync bool) error {
	// Validate the cron expression before mutating any state so a bad
	// schedule can't leave the scheduler with a half-removed previous
	// job. ValidateCronExpr is cheap and pure.
	if job != nil && schedule != "" {
		if err := ValidateCronExpr(schedule); err != nil {
			return fmt.Errorf("invalid embed cron expression %q: %w", schedule, err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.embedEntrySet {
		s.cron.Remove(s.embedEntry)
		s.embedEntrySet = false
	}
	s.embedJob = job
	s.runEmbedAfterSync = runAfterSync && job != nil

	if job == nil || schedule == "" {
		return nil
	}
	entryID, err := s.cron.AddFunc(schedule, func() {
		if s.isStopped() {
			return
		}
		done, ok := s.beginWork("scheduled embedding")
		if !ok {
			return
		}
		defer done()
		runCtx, endRun := s.jobContext("embed", false, 0)
		defer endRun()
		job.Run(runCtx)
	})
	if err != nil {
		// ValidateCronExpr above should have caught any parse error;
		// if AddFunc still fails here it's an internal invariant
		// violation, not caller input. Roll back the state we mutated.
		s.embedJob = nil
		s.runEmbedAfterSync = false
		return fmt.Errorf("register embed cron: %w", err)
	}
	s.embedEntry = entryID
	s.embedEntrySet = true
	s.logger.Info("scheduled embed job",
		"schedule", schedule,
		"next_run", s.nextRun(entryID))
	return nil
}

// SetDocumentVectorJob installs the bounded document-vector convergence job
// on the same cron/post-sync policy used by message embeddings.
func (s *Scheduler) SetDocumentVectorJob(job func(context.Context) error, schedule string, runAfterSync bool) error {
	if job != nil && schedule != "" {
		if err := ValidateCronExpr(schedule); err != nil {
			return fmt.Errorf("invalid document vector cron expression %q: %w", schedule, err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.documentVectorEntrySet {
		s.cron.Remove(s.documentVectorEntry)
		s.documentVectorEntrySet = false
	}
	s.documentVectorJob = job
	s.runDocumentVectorAfterSync = runAfterSync && job != nil
	if job == nil || schedule == "" {
		return nil
	}
	entry, err := s.cron.AddFunc(schedule, func() {
		if s.isStopped() {
			return
		}
		done, ok := s.beginWork("scheduled document indexing")
		if !ok {
			return
		}
		defer done()
		runCtx, endRun := s.jobContext("document-vector", false, 0)
		defer endRun()
		if runErr := job(runCtx); runErr != nil {
			s.logger.Error("scheduled document vector reconciliation failed", "error", runErr)
		}
	})
	if err != nil {
		s.documentVectorJob = nil
		s.runDocumentVectorAfterSync = false
		return fmt.Errorf("register document vector cron: %w", err)
	}
	s.documentVectorEntry = entry
	s.documentVectorEntrySet = true
	return nil
}

// isStopped reports s.stopped under a read lock. Used by cron
// callbacks that only need to abort on shutdown.
func (s *Scheduler) isStopped() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stopped
}

// Start begins executing scheduled jobs.
func (s *Scheduler) Start() {
	s.mu.Lock()
	s.started = true
	s.stopped = false
	s.mu.Unlock()

	s.cron.Start()
	s.logger.Info("scheduler started", "jobs", len(s.jobs))
}

// IsRunning returns true if the scheduler has been started and not yet stopped.
func (s *Scheduler) IsRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.started && !s.stopped
}

// Stop gracefully stops the scheduler, cancels running sync jobs, and waits
// for them to finish. Returns a context that is done when all work completes.
// Stop also waits for any post-sync embed passes that are in flight. Those
// passes receive the cancelled s.ctx and are expected to bail quickly.
func (s *Scheduler) Stop() context.Context {
	s.logger.Info("scheduler stopping")

	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()

	cronCtx := s.cron.Stop()
	s.cancel() // signal running syncs to stop

	done := make(chan struct{})
	go func() {
		<-cronCtx.Done()
		s.wg.Wait()
		close(done)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-done
		cancel()
	}()
	return ctx
}

// runSync executes sync for an account (called by cron or TriggerSync).
// The caller must have already called wg.Add(1) and set running[email] = true.
func (s *Scheduler) runSync(email string) {
	defer s.wg.Done()
	defer s.finishAccountRun(email)

	s.mu.Lock()
	s.queued[email] = true
	s.mu.Unlock()
	done, ok := s.beginWork("scheduled sync of " + email)
	s.mu.Lock()
	delete(s.queued, email)
	if ok {
		s.startedAt[email] = time.Now()
	}
	s.mu.Unlock()
	if !ok {
		s.logger.Info("scheduled sync skipped: daemon is draining", "email", email)
		return
	}
	defer done()

	s.logger.Info("starting scheduled sync", "email", email)
	start := time.Now()

	s.mu.RLock()
	preemptionPolicy := s.accountPreemptionPolicy
	s.mu.RUnlock()
	preemptible := preemptionPolicy == nil || preemptionPolicy(email)
	runCtx, endRun := s.jobContext("sync "+email, preemptible, 0)
	err := s.syncFunc(runCtx, email)
	endRun()
	yielded := yieldedToWaiter(runCtx) || jobctx.PreemptionRequested(runCtx)

	s.mu.Lock()
	if yielded {
		if callbackErr := jobctx.ErrorAfterYield(runCtx, err); callbackErr != nil {
			s.lastErr[email] = callbackErr
			logScheduledSyncError(s.logger, email, time.Since(start), callbackErr)
		}
		if _, scheduled := s.jobs[email]; scheduled {
			s.pending[email] = true
		}
		s.logger.Info("scheduled sync yielded to a waiting operation; queued follow-up",
			"email", email,
			"duration", time.Since(start))
	} else if err != nil {
		s.lastErr[email] = err
		logScheduledSyncError(s.logger, email, time.Since(start), err)
	} else {
		s.lastRun[email] = time.Now()
		s.lastErr[email] = nil
		s.logger.Info("scheduled sync completed",
			"email", email,
			"duration", time.Since(start))
	}
	s.mu.Unlock()

	// Post-sync embed hook: only fire on successful sync, and only
	// when configured. Runs synchronously in this goroutine so it's
	// naturally covered by the wg.Done deferred above.
	if err != nil || yielded {
		return
	}
	var postSync *EmbedJob
	s.mu.RLock()
	if s.runEmbedAfterSync && s.embedJob != nil && !s.stopped {
		postSync = s.embedJob
	}
	s.mu.RUnlock()
	if postSync != nil {
		embedCtx, endEmbed := s.jobContext("post-sync embed", false, 0)
		postSync.Run(embedCtx)
		endEmbed()
	}
	var documentVectorPostSync func(context.Context) error
	s.mu.RLock()
	if s.runDocumentVectorAfterSync && s.documentVectorJob != nil && !s.stopped {
		documentVectorPostSync = s.documentVectorJob
	}
	s.mu.RUnlock()
	if documentVectorPostSync != nil {
		documentCtx, endDocument := s.jobContext("post-sync document vector", false, 0)
		if documentErr := documentVectorPostSync(documentCtx); documentErr != nil {
			s.logger.Error("post-sync document vector reconciliation failed", "error", documentErr)
		}
		endDocument()
	}
	s.startVisualPostSync()
}

// logScheduledSyncError keeps transient network failures below the error
// level while preserving other callback errors for scheduled account status.
func logScheduledSyncError(logger *slog.Logger, email string, duration time.Duration, err error) {
	if syncerr.IsTransientNetwork(err) {
		logger.Warn("scheduled sync skipped (transient network)",
			"email", email,
			"duration", duration,
			"error", err)
		return
	}
	logger.Error("scheduled sync failed",
		"email", email,
		"duration", duration,
		"error", err)
}

// finishAccountRun releases an account run, or hands its reservation to the
// single follow-up requested by a tick or yield. The follow-up is
// reserved (running stays true, wg.Add) before the finishing run's wg.Done.
func (s *Scheduler) finishAccountRun(email string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.startedAt, email)
	if s.pending[email] && !s.stopped {
		delete(s.pending, email)
		s.queued[email] = true
		s.wg.Add(1)
		go s.runSync(email)
		return
	}
	delete(s.pending, email)
	s.running[email] = false
}

// startVisualPostSync queues hosted multimodal work after the source sync has
// committed. It deliberately does not wait in the sync goroutine: provider
// latency must never extend or fail an otherwise successful archive sync.
func (s *Scheduler) startVisualPostSync() {
	s.mu.Lock()
	if s.visualPostSync == nil || s.stopped {
		s.mu.Unlock()
		return
	}
	if s.visualPostRunning {
		// A sync finished while a pass is in flight; remember it so the
		// changes it committed are processed right after, instead of waiting
		// for the next sync or cron tick.
		s.visualPostPending = true
		s.mu.Unlock()
		return
	}
	run := s.visualPostSync
	s.visualPostRunning = true
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			s.visualPostRunning = false
			rerun := s.visualPostPending
			s.visualPostPending = false
			s.mu.Unlock()
			if rerun {
				s.startVisualPostSync()
			}
		}()
		done, ok := s.beginWork("post-sync multimodal indexing")
		if !ok {
			return
		}
		defer done()
		visualCtx, endVisual := s.jobContext("post-sync multimodal", false, 0)
		defer endVisual()
		if err := run(visualCtx); err != nil {
			s.logger.Error("post-sync multimodal pass failed", "error", err)
		}
	}()
}

// IsScheduled returns true if the account has been added to the scheduler.
func (s *Scheduler) IsScheduled(email string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, exists := s.jobs[email]
	return exists
}

// TriggerSync manually triggers a sync for an account (outside of schedule).
// Returns an error if a sync is already running, the account is not scheduled,
// or the scheduler has been stopped.
func (s *Scheduler) TriggerSync(email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stopped {
		return errors.New("scheduler is stopped")
	}

	if _, exists := s.jobs[email]; !exists {
		return fmt.Errorf("account %s is not scheduled", email)
	}
	if s.running[email] {
		return fmt.Errorf("sync already running for %s", email)
	}

	s.running[email] = true
	s.queued[email] = true
	s.wg.Add(1)
	go s.runSync(email)
	return nil
}

func (s *Scheduler) IsJobScheduled(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.genericJobs[name]
	return ok
}

// TriggerJob synchronously reserves and runs the named generic job. It is
// used by the cron scheduler (which fires jobs from its own goroutine with
// no gate held) and relies on running to completion before returning so
// lastRun/lastErr are recorded synchronously.
func (s *Scheduler) TriggerJob(name string) error {
	run, disp, err := s.reserveGenericJob(name, false)
	if err != nil {
		return err
	}
	if disp != JobStarted {
		return nil
	}
	return s.runJob(name, run)
}

// JobDisposition says what a manual trigger of a generic job did.
type JobDisposition string

const (
	// JobStarted: the job was idle and a run was started.
	JobStarted JobDisposition = "started"
	// JobPending: the job was running; one follow-up run was recorded. The
	// value matches the "pending" field that job status reports for it.
	JobPending JobDisposition = "pending"
	// JobCoalesced: the job is waiting to start (its initial run is queued
	// behind the operation gate) or a follow-up is already recorded, so this
	// request added no run.
	JobCoalesced JobDisposition = "coalesced"
)

// StartJob asynchronously reserves and runs the named generic job in a new
// goroutine, returning as soon as the reservation succeeds. This is used by
// callers (e.g. an HTTP handler) that may already hold the daemon's
// operation gate, so the job's gate acquisition must happen after the
// caller has had a chance to return and release it.
//
// A request that arrives while the job is active coalesces like a cron tick:
// at most one follow-up run is kept, and the returned disposition says
// whether a run started, a follow-up was queued, or the request was merged
// into one already pending.
func (s *Scheduler) StartJob(name string) (JobDisposition, error) {
	run, disp, err := s.reserveGenericJob(name, true)
	if err != nil {
		return "", err
	}
	if disp != JobStarted {
		return disp, nil
	}
	go func() {
		_ = s.runJob(name, run)
	}()
	return disp, nil
}

// onJobTick handles one cron firing for a generic job, coalescing a tick
// that arrives while the job is active like onAccountTick does.
func (s *Scheduler) onJobTick(name string) {
	run, disp, err := s.reserveGenericJob(name, true)
	if err != nil || disp != JobStarted {
		return
	}
	_ = s.runJob(name, run)
}

// reserveGenericJob validates and reserves the named generic job under the
// lock, mirroring TriggerSync's reservation of an account sync. The
// disposition is JobStarted when the caller now owns a run. When the job is
// already running it is JobPending or JobCoalesced if coalesce is set (one
// follow-up run is remembered), and empty otherwise (a no-op, not an error).
func (s *Scheduler) reserveGenericJob(name string, coalesce bool) (run func(context.Context) error, disp JobDisposition, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	run = s.genericFuncs[name]
	if run == nil {
		return nil, "", fmt.Errorf("job %q is not scheduled", name)
	}
	if s.stopped {
		return nil, "", errors.New("scheduler is stopped")
	}
	if s.genericRunning[name] {
		if !coalesce {
			return nil, "", nil
		}
		if s.coalesceTickLocked("job", name, s.genericQueued, s.genericPending) {
			return nil, JobPending, nil
		}
		return nil, JobCoalesced, nil
	}
	s.genericRunning[name] = true
	s.genericQueued[name] = true
	s.wg.Add(1)
	return run, JobStarted, nil
}

// runJob executes an already-reserved generic job and records the result.
// The caller must have set genericRunning[name] = true and called
// s.wg.Add(1) before invoking runJob.
func (s *Scheduler) runJob(name string, run func(context.Context) error) error {
	defer s.wg.Done()
	defer s.finishGenericRun(name)

	s.mu.Lock()
	s.genericQueued[name] = true
	preemptible := s.genericPreemptible[name]
	maxRuntime := s.genericMaxRuntime[name]
	s.mu.Unlock()
	done, ok := s.beginWork(name)
	s.mu.Lock()
	delete(s.genericQueued, name)
	if ok {
		s.genericStartedAt[name] = time.Now()
	}
	s.mu.Unlock()
	if !ok {
		return nil
	}
	defer done()

	runCtx, endRun := s.jobContext(name, preemptible, maxRuntime)
	err := run(runCtx)
	endRun()
	s.mu.Lock()
	defer s.mu.Unlock()
	budgetExpired := errors.Is(context.Cause(runCtx), jobctx.ErrRunBudgetExceeded)
	if budgetExpired && !jobctx.HasProgress(runCtx) {
		delete(s.genericPending, name)
		err = errors.Join(fmt.Errorf("%w before committing progress", jobctx.ErrRunBudgetExceeded),
			jobctx.ErrorAfterYield(runCtx, err))
		s.genericLastErr[name] = err
		s.logger.Warn("scheduled job reached its runtime limit before committing progress; waiting for the next trigger",
			"job", name,
			"max_runtime", maxRuntime,
			"error", err)
		return err
	}
	yielded := budgetExpired || yieldedToWaiter(runCtx) || jobctx.PreemptionRequested(runCtx)
	if errors.Is(err, ErrReschedule) {
		s.genericPending[name] = true
		s.logger.Info("scheduled job has remaining work; queued follow-up",
			"job", name)
		return nil
	}
	if yielded {
		s.genericPending[name] = true
		if callbackErr := jobctx.ErrorAfterYield(runCtx, err); callbackErr != nil {
			s.genericLastErr[name] = callbackErr
			s.logger.Error("scheduled job yielded after callback error; queued follow-up",
				"job", name,
				"error", callbackErr)
			return callbackErr
		}
		s.logger.Info("scheduled job yielded to waiting work; queued follow-up",
			"job", name)
		return nil
	}
	if err != nil {
		s.genericLastErr[name] = err
		return err
	}
	s.genericLastRun[name] = time.Now()
	delete(s.genericLastErr, name)
	return nil
}

// finishGenericRun releases a generic job run, or hands its reservation to
// the single follow-up requested by a tick, yield, or bounded pass.
func (s *Scheduler) finishGenericRun(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.genericStartedAt, name)
	// Run the job as currently registered: AddJob may have replaced it while
	// this run executed, and RemoveJob drops the follow-up.
	current := s.genericFuncs[name]
	if s.genericPending[name] && !s.stopped && current != nil {
		delete(s.genericPending, name)
		s.genericQueued[name] = true
		s.wg.Add(1)
		go func() { _ = s.runJob(name, current) }()
		return
	}
	if s.genericPending[name] {
		level := slog.LevelWarn
		if s.stopped {
			level = slog.LevelInfo
		}
		s.logger.Log(context.Background(), level, "scheduled job follow-up dropped without running",
			"job", name, "stopped", s.stopped, "job_registered", current != nil)
		delete(s.genericPending, name)
	}
	s.genericRunning[name] = false
}

func (s *Scheduler) beginWork(label string) (func(), bool) {
	if s.work == nil {
		return func() {}, true
	}
	s.mu.Lock()
	s.queuedRuns++
	s.mu.Unlock()
	var done func()
	var ok bool
	if labeled, supportsLabels := s.work.(LabeledWorkTracker); supportsLabels {
		done, ok = labeled.BeginLabeledWorkContext(s.ctx, label)
	} else {
		done, ok = s.work.BeginWorkContext(s.ctx)
	}
	s.mu.Lock()
	s.queuedRuns--
	s.mu.Unlock()
	return done, ok
}

// preemptAfter is how long a scheduled run may hold the work gate while
// other scheduled runs are queued before it is asked to yield. Variable only
// so tests can shorten it.
var preemptAfter = time.Minute

// jobContext derives the context a scheduled job runs with. When the work
// tracker can report waiters, the context is cancelled with cause
// ErrYieldedToWaiter so the resumable job steps aside for the waiter and
// is queued to resume after the waiter. Preemptible contexts also carry a
// cooperative preemption request (see jobctx.WithPreemption), raised once the
// run has held the gate for preemptAfter while other scheduled runs queue
// behind it. Jobs that have not stopped at the next poll are cancelled with
// ErrYieldedToWaiter. A positive maxRuntime ends the run with cause
// jobctx.ErrRunBudgetExceeded. Other jobs keep their own runtime budgets. The
// returned stop function must be called when the job finishes.
func (s *Scheduler) jobContext(label string, preemptible bool, maxRuntime time.Duration) (context.Context, func()) {
	started := time.Now()
	baseCtx := jobctx.WithProgress(s.ctx)
	stopBudget := func() {}
	if maxRuntime > 0 {
		baseCtx, stopBudget = context.WithTimeoutCause(baseCtx, maxRuntime, jobctx.ErrRunBudgetExceeded)
	}
	preemptCtx, requestPreemption := jobctx.WithPreemption(baseCtx)
	yc, canYield := s.work.(YieldChecker)
	if s.work == nil {
		return preemptCtx, stopBudget
	}
	ctx, cancel := context.WithCancelCause(preemptCtx)
	go func() {
		ticker := time.NewTicker(yieldPollInterval)
		defer ticker.Stop()
		preempted := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if canYield && yc.ShouldYield() {
					cancel(ErrYieldedToWaiter)
					return
				}
				if preempted {
					cancel(ErrYieldedToWaiter)
					return
				}
				if preemptible && !preempted && time.Since(started) >= preemptAfter && s.hasQueuedRuns() {
					preempted = true
					s.logger.Info("scheduled job asked to yield to queued work",
						"job", label,
						"held", time.Since(started).Round(time.Second))
					requestPreemption()
					// Give cooperative jobs one poll interval to reach a safe
					// checkpoint, then cancel with the recognized yield cause so
					// other sources still release the work gate.
				}
			}
		}
	}()
	return ctx, func() { cancel(nil); stopBudget() }
}

func (s *Scheduler) hasQueuedRuns() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.queuedRuns > 0
}

// nextRun reports when a cron entry fires next. robfig/cron fills Entry.Next
// only once its run loop has started, so before Start the value is computed
// from the schedule instead of reporting the zero time.
func (s *Scheduler) nextRun(entryID cron.EntryID) time.Time {
	entry := s.cron.Entry(entryID)
	if !entry.Next.IsZero() || entry.Schedule == nil {
		return entry.Next
	}
	return entry.Schedule.Next(time.Now())
}

func yieldedToWaiter(ctx context.Context) bool {
	return jobctx.YieldedToWaiter(ctx)
}

// Status returns the current status of all scheduled accounts.
func (s *Scheduler) Status() []AccountStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var statuses []AccountStatus
	for email, entryID := range s.jobs {
		status := AccountStatus{
			Email:     email,
			Running:   !s.startedAt[email].IsZero(),
			LastRun:   s.lastRun[email],
			NextRun:   s.nextRun(entryID),
			Schedule:  s.schedules[email],
			Queued:    s.queued[email],
			Pending:   s.pending[email],
			StartedAt: s.startedAt[email],
		}
		if err := s.lastErr[email]; err != nil {
			status.LastError = err.Error()
		}
		statuses = append(statuses, status)
	}
	return statuses
}

func (s *Scheduler) JobStatus() []JobStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]JobStatus, 0, len(s.genericJobs))
	for name, entryID := range s.genericJobs {
		var lastErr string
		if err := s.genericLastErr[name]; err != nil {
			lastErr = err.Error()
		}
		out = append(out, JobStatus{
			Name:      name,
			Running:   !s.genericStartedAt[name].IsZero(),
			LastRun:   s.genericLastRun[name],
			NextRun:   s.nextRun(entryID),
			Schedule:  s.genericSchedules[name],
			LastError: lastErr,
			Queued:    s.genericQueued[name],
			Pending:   s.genericPending[name],
			StartedAt: s.genericStartedAt[name],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NormalizeCronExpr trims a schedule and turns a value that is only a time
// zone prefix into the empty string: with no fields after the zone there is
// no schedule, so it is off rather than an error.
func NormalizeCronExpr(expr string) string {
	trimmed := strings.TrimSpace(expr)
	if strings.HasPrefix(trimmed, "TZ=") || strings.HasPrefix(trimmed, "CRON_TZ=") {
		if !strings.ContainsAny(trimmed, " \t") {
			return ""
		}
	}
	return trimmed
}

// ValidateCronExpr validates a cron expression without scheduling anything.
// It accepts what the scheduler's parser accepts: five fields, optionally
// preceded by a "CRON_TZ=<zone>" or "TZ=<zone>" prefix. It also rejects a
// field made only of commas, which the parser stores but never matches.
func ValidateCronExpr(expr string) error {
	fields := expr
	if strings.HasPrefix(fields, "TZ=") || strings.HasPrefix(fields, "CRON_TZ=") {
		// The parser slices at the first space without checking for one.
		space := strings.Index(fields, " ")
		if space < 0 {
			return errors.New("invalid cron expression: the time zone must be followed by the five schedule fields")
		}
		fields = fields[space:]
	}
	for field := range strings.FieldsSeq(fields) {
		if strings.Trim(field, ",") == "" {
			return fmt.Errorf("invalid cron expression: field %q lists no values", field)
		}
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	if _, err := parser.Parse(expr); err != nil {
		return fmt.Errorf("invalid cron expression: %w", err)
	}
	return nil
}
