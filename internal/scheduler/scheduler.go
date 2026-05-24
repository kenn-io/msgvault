// Package scheduler provides cron-based scheduling for automated email sync.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/syncerr"
)

// SyncFunc is the callback invoked when a scheduled sync should run.
// It receives the account email and should perform incremental sync + cache build.
type SyncFunc func(ctx context.Context, email string) error

// AccountStatus represents the sync status of a scheduled account.
type AccountStatus struct {
	Email     string    `json:"email"`
	Running   bool      `json:"running"`
	LastRun   time.Time `json:"last_run,omitempty"`
	NextRun   time.Time `json:"next_run"`
	Schedule  string    `json:"schedule"`
	LastError string    `json:"last_error,omitempty"`
}

// Scheduler manages cron-based email sync scheduling.
type Scheduler struct {
	cron     *cron.Cron
	syncFunc SyncFunc
	logger   *slog.Logger

	mu        sync.RWMutex
	jobs      map[string]cron.EntryID // email -> cron entry ID
	schedules map[string]string       // email -> cron expression
	running   map[string]bool         // email -> currently syncing
	lastRun   map[string]time.Time    // email -> last successful run
	lastErr   map[string]error        // email -> last error

	// Embed job state (optional). Set via SetEmbedJob; cron.EntryID 0
	// may be valid, so embedEntrySet tracks whether an entry exists.
	embedJob          *EmbedJob
	embedEntry        cron.EntryID
	embedEntrySet     bool
	runEmbedAfterSync bool

	ctx     context.Context    // cancelled on Stop
	cancel  context.CancelFunc // cancels ctx
	wg      sync.WaitGroup     // tracks running sync goroutines
	started bool               // true after Start(), false after Stop()
	stopped bool               // true after Stop()
}

// New creates a new Scheduler with the given sync callback.
func New(syncFunc SyncFunc) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		cron: cron.New(cron.WithParser(cron.NewParser(
			cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
		))),
		syncFunc:  syncFunc,
		logger:    slog.Default(),
		jobs:      make(map[string]cron.EntryID),
		schedules: make(map[string]string),
		running:   make(map[string]bool),
		lastRun:   make(map[string]time.Time),
		lastErr:   make(map[string]error),
		ctx:       ctx,
		cancel:    cancel,
	}
}

// WithLogger sets the logger for the scheduler.
func (s *Scheduler) WithLogger(logger *slog.Logger) *Scheduler {
	s.logger = logger
	return s
}

// AddAccount schedules sync for an account using the given cron expression.
// Returns an error if the cron expression is invalid.
func (s *Scheduler) AddAccount(email, cronExpr string) error {
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
		s.mu.Lock()
		if s.stopped || s.running[email] {
			s.mu.Unlock()
			return
		}
		s.running[email] = true
		s.wg.Add(1)
		s.mu.Unlock()
		s.runSync(email)
	})
	if err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", cronExpr, err)
	}

	s.jobs[email] = entryID
	s.schedules[email] = cronExpr
	s.logger.Info("scheduled sync",
		"email", email,
		"schedule", cronExpr,
		"next_run", s.cron.Entry(entryID).Next)

	return nil
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
		job.Run(s.ctx)
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
		"next_run", s.cron.Entry(entryID).Next)
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
	defer func() {
		s.mu.Lock()
		s.running[email] = false
		s.mu.Unlock()
	}()

	s.logger.Info("starting scheduled sync", "email", email)
	start := time.Now()

	err := s.syncFunc(s.ctx, email)

	s.mu.Lock()
	if err != nil {
		s.lastErr[email] = err
		// Transient network failures (e.g., DNS lookup timeout after
		// laptop sleep/wake) aren't actionable — the next scheduled tick
		// will retry. Log at WARN to keep them out of error dashboards.
		if syncerr.IsTransientNetwork(err) {
			s.logger.Warn("scheduled sync skipped (transient network)",
				"email", email,
				"duration", time.Since(start),
				"error", err)
		} else {
			s.logger.Error("scheduled sync failed",
				"email", email,
				"duration", time.Since(start),
				"error", err)
		}
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
	if err != nil {
		return
	}
	var postSync *EmbedJob
	s.mu.RLock()
	if s.runEmbedAfterSync && s.embedJob != nil && !s.stopped {
		postSync = s.embedJob
	}
	s.mu.RUnlock()
	if postSync != nil {
		postSync.Run(s.ctx)
	}
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
		return fmt.Errorf("scheduler is stopped")
	}

	if _, exists := s.jobs[email]; !exists {
		return fmt.Errorf("account %s is not scheduled", email)
	}
	if s.running[email] {
		return fmt.Errorf("sync already running for %s", email)
	}

	s.running[email] = true
	s.wg.Add(1)
	go s.runSync(email)
	return nil
}

// Status returns the current status of all scheduled accounts.
func (s *Scheduler) Status() []AccountStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var statuses []AccountStatus
	for email, entryID := range s.jobs {
		entry := s.cron.Entry(entryID)
		status := AccountStatus{
			Email:    email,
			Running:  s.running[email],
			LastRun:  s.lastRun[email],
			NextRun:  entry.Next,
			Schedule: s.schedules[email],
		}
		if err := s.lastErr[email]; err != nil {
			status.LastError = err.Error()
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// ValidateCronExpr validates a cron expression without scheduling anything.
func ValidateCronExpr(expr string) error {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	_, err := parser.Parse(expr)
	if err != nil {
		return fmt.Errorf("invalid cron expression: %w", err)
	}
	return nil
}
