package cmd

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/msgvault/internal/api"
)

// cacheBuildJobs owns detached builds for one daemon. The builder's file lock
// remains the cross-process serialization boundary; the registry coalesces
// requests arriving at this daemon and gives callers a stable status ID.
type cacheBuildJobs struct {
	mu                  sync.Mutex
	ctx                 context.Context
	idle                *api.IdleTracker
	logger              *slog.Logger
	run                 func(context.Context, buildCacheMode) error
	current             string
	pending             string
	pendingMode         buildCacheMode
	lastVerification    time.Time
	verificationRetryAt time.Time
	jobs                map[string]api.CacheBuildStatus
	completed           []string
	wg                  sync.WaitGroup
	// paused holds new builds while a backup freeze checkpoints the archive.
	// runCancel and runDone belong to the current build, if any.
	paused    bool
	runCancel context.CancelFunc
	runDone   chan struct{}
}

// verifyWhenDue queues a full staleness check in the builder subprocess.
// A clean publication keeps its original timestamp, so remember the last
// check to avoid launching another subprocess on every subsequent query.
func (m *cacheBuildJobs) verifyWhenDue(publishedAt time.Time, interval time.Duration, now time.Time) error {
	if publishedAt.IsZero() {
		return nil
	}
	if interval <= 0 {
		interval = time.Minute
	}
	if now.Before(publishedAt.Add(interval)) {
		return nil
	}
	m.mu.Lock()
	if m.current != "" {
		m.mu.Unlock()
		return nil
	}
	if now.Before(m.verificationRetryAt) {
		m.mu.Unlock()
		return nil
	}
	due := m.lastVerification.IsZero() || !now.Before(m.lastVerification.Add(interval))
	if due {
		m.lastVerification = now
	}
	m.mu.Unlock()
	if !due {
		return nil
	}
	_, err := m.acceptWithFollowup(buildCacheModeScheduledAuto, false, now)
	if err != nil {
		m.mu.Lock()
		if m.lastVerification.Equal(now) {
			m.lastVerification = time.Time{}
		}
		m.mu.Unlock()
	}
	return err
}

func newCacheBuildJobs(
	ctx context.Context, idle *api.IdleTracker,
	run func(context.Context, buildCacheMode) error,
) *cacheBuildJobs {
	if ctx == nil {
		ctx = context.Background()
	}
	if run == nil {
		run = buildCacheSubprocessMode
	}
	return &cacheBuildJobs{ctx: ctx, idle: idle, run: run, jobs: make(map[string]api.CacheBuildStatus)}
}

func (m *cacheBuildJobs) accept(mode buildCacheMode) (api.CacheBuildStatus, error) {
	return m.acceptWithFollowup(mode, false, time.Now())
}

// A sync may commit while the active builder is reading an older snapshot.
// Return a separate queued job so callers can wait for the later snapshot.
func (m *cacheBuildJobs) acceptAfterWrite(mode buildCacheMode) (api.CacheBuildStatus, error) {
	return m.acceptWithFollowup(mode, true, time.Now())
}

func (m *cacheBuildJobs) acceptWithFollowup(mode buildCacheMode, afterWrite bool, now time.Time) (api.CacheBuildStatus, error) {
	if m == nil {
		return api.CacheBuildStatus{}, errors.New("analytics cache build manager unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return api.CacheBuildStatus{}, m.ctx.Err()
	}
	if mode == buildCacheModeScheduledAuto && now.Before(m.verificationRetryAt) {
		return api.CacheBuildStatus{}, nil
	}
	if m.current != "" && !afterWrite {
		return m.jobs[m.current], nil
	}
	if m.pending != "" {
		if m.pendingMode == buildCacheModeScheduledAuto && mode != buildCacheModeScheduledAuto {
			m.pendingMode = mode
		}
		return m.jobs[m.pending], nil
	}
	job := api.CacheBuildStatus{
		JobID: uuid.NewString(), Status: api.CacheBuildQueued, AcceptedAt: now.UTC(),
	}
	m.jobs[job.JobID] = job
	if m.current != "" || m.paused {
		m.pending, m.pendingMode = job.JobID, mode
		return job, nil
	}
	if err := m.startLocked(job.JobID, mode); err != nil {
		delete(m.jobs, job.JobID)
		return api.CacheBuildStatus{}, err
	}
	return job, nil
}

func (m *cacheBuildJobs) startLocked(id string, mode buildCacheMode) error {
	done, ok := m.idle.BeginWorkContext(m.ctx)
	if !ok {
		return errors.New("daemon is shutting down")
	}
	runCtx, cancel := context.WithCancel(m.ctx)
	m.current = id
	m.runCancel, m.runDone = cancel, make(chan struct{})
	m.wg.Add(1)
	go m.execute(runCtx, id, mode, done)
	return nil
}

func (m *cacheBuildJobs) execute(runCtx context.Context, id string, mode buildCacheMode, done func()) {
	defer m.wg.Done()
	defer done()
	m.mu.Lock()
	job := m.jobs[id]
	job.Status = api.CacheBuildRunning
	m.jobs[id] = job
	m.mu.Unlock()
	err := m.run(runCtx, mode)
	m.mu.Lock()
	interrupted := m.ctx.Err() == nil && runCtx.Err() != nil
	m.runCancel()
	close(m.runDone)
	m.runCancel, m.runDone = nil, nil
	if interrupted {
		m.requeueInterruptedLocked(id, mode)
		// A freeze that gave up waiting already resumed; run the job now.
		if !m.paused {
			m.dispatchPendingLocked()
		}
		m.mu.Unlock()
		return
	}
	finishedAt := time.Now().UTC()
	var failure string
	if err != nil {
		failure = "analytics cache build failed; see daemon logs"
		if m.ctx.Err() != nil {
			failure = "daemon is shutting down"
		} else {
			repairLogger(m.logger).Error("background analytics cache build failed", "job_id", id, "error", err)
		}
		if mode == buildCacheModeScheduledAuto {
			m.lastVerification = time.Time{}
			m.verificationRetryAt = finishedAt.Add(time.Minute)
		}
	} else if mode == buildCacheModeScheduledAuto {
		m.verificationRetryAt = time.Time{}
	}
	m.finishLocked(id, failure, finishedAt)
	if m.current == id {
		m.current = ""
	}
	if m.pending != "" && !m.paused {
		pending, pendingMode := m.pending, m.pendingMode
		m.pending = ""
		m.pendingMode = buildCacheModeDefault
		switch {
		case m.ctx.Err() != nil:
			m.finishLocked(pending, "daemon is shutting down", finishedAt)
		case pendingMode == buildCacheModeScheduledAuto && finishedAt.Before(m.verificationRetryAt):
			m.finishLocked(pending, "analytics cache retry deferred after build failure", finishedAt)
		default:
			if startErr := m.startLocked(pending, pendingMode); startErr != nil {
				m.finishLocked(pending, startErr.Error(), finishedAt)
				if m.ctx.Err() == nil {
					repairLogger(m.logger).Error("queue follow-up analytics cache build failed", "error", startErr)
				}
			}
		}
	}
	m.mu.Unlock()
}

// requeueInterruptedLocked handles a build a backup freeze stopped. The job
// returns to the queue and runs again after resume, unless a later request is
// already queued; that later build covers this one, so this job ends.
func (m *cacheBuildJobs) requeueInterruptedLocked(id string, mode buildCacheMode) {
	m.current = ""
	if m.pending != "" {
		m.finishLocked(id, "analytics cache build was interrupted by a backup; a queued build replaces it", time.Now().UTC())
		if mode != buildCacheModeScheduledAuto && m.pendingMode == buildCacheModeScheduledAuto {
			m.pendingMode = mode
		}
		return
	}
	job := m.jobs[id]
	job.Status = api.CacheBuildQueued
	m.jobs[id] = job
	m.pending, m.pendingMode = id, mode
}

// pause cancels the current build, waits for it to return, and holds new
// builds until the returned resume is called. The interrupted build stays
// queued under its job ID. On error the jobs are already resumed.
func (m *cacheBuildJobs) pause(ctx context.Context) (func(), error) {
	if m == nil {
		return func() {}, nil
	}
	m.mu.Lock()
	m.paused = true
	cancel, runDone := m.runCancel, m.runDone
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		select {
		case <-runDone:
		case <-ctx.Done():
			m.resume()
			return nil, ctx.Err()
		}
	}
	var once sync.Once
	return func() { once.Do(m.resume) }, nil
}

func (m *cacheBuildJobs) resume() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.paused = false
	m.dispatchPendingLocked()
}

// dispatchPendingLocked starts the queued job when no build is running.
func (m *cacheBuildJobs) dispatchPendingLocked() {
	if m.current != "" || m.pending == "" {
		return
	}
	pending, pendingMode := m.pending, m.pendingMode
	m.pending = ""
	m.pendingMode = buildCacheModeDefault
	if m.ctx.Err() != nil {
		m.finishLocked(pending, "daemon is shutting down", time.Now().UTC())
		return
	}
	if err := m.startLocked(pending, pendingMode); err != nil {
		m.finishLocked(pending, err.Error(), time.Now().UTC())
	}
}

// Keep the last 100 completions. Running and queued jobs are never evicted.
func (m *cacheBuildJobs) finishLocked(id, failure string, finishedAt time.Time) {
	job := m.jobs[id]
	job.Status = api.CacheBuildPublished
	if failure != "" {
		job.Status = api.CacheBuildFailed
	}
	job.Error = failure
	job.FinishedAt = &finishedAt
	m.jobs[id] = job
	m.completed = append(m.completed, id)
	if len(m.completed) > 100 {
		delete(m.jobs, m.completed[0])
		m.completed = m.completed[1:]
	}
}

func (m *cacheBuildJobs) waitContext(ctx context.Context) bool {
	if m == nil {
		return true
	}
	// The caller cancels m.ctx first. Drain any accept already holding the
	// mutex so no WaitGroup Add races the wait.
	m.mu.Lock()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	m.mu.Unlock()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (m *cacheBuildJobs) status(id string) (api.CacheBuildStatus, bool) {
	if m == nil {
		return api.CacheBuildStatus{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	return job, ok
}

func (m *cacheBuildJobs) active() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current != ""
}
