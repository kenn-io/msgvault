package cmd

import (
	"context"
	"errors"
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
	run                 func(context.Context, buildCacheMode) error
	current             string
	pending             bool
	pendingMode         buildCacheMode
	lastVerification    time.Time
	verificationRetryAt time.Time
	jobs                map[string]api.CacheBuildStatus
	wg                  sync.WaitGroup
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
	_, err := m.accept(buildCacheModeScheduledAuto)
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
	return m.acceptWithFollowup(mode, false)
}

// A sync may commit while the active builder is reading an older snapshot.
// Queue one follow-up so its new rows cannot be lost by coalescing.
func (m *cacheBuildJobs) acceptAfterWrite(mode buildCacheMode) (api.CacheBuildStatus, error) {
	return m.acceptWithFollowup(mode, true)
}

func (m *cacheBuildJobs) acceptWithFollowup(mode buildCacheMode, afterWrite bool) (api.CacheBuildStatus, error) {
	if m == nil {
		return api.CacheBuildStatus{}, errors.New("analytics cache build manager unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return api.CacheBuildStatus{}, m.ctx.Err()
	}
	if m.current != "" {
		if afterWrite {
			if !m.pending || (m.pendingMode == buildCacheModeScheduledAuto && mode != buildCacheModeScheduledAuto) {
				m.pendingMode = mode
			}
			m.pending = true
		}
		return m.jobs[m.current], nil
	}
	return m.startLocked(mode)
}

func (m *cacheBuildJobs) startLocked(mode buildCacheMode) (api.CacheBuildStatus, error) {
	done, ok := m.idle.BeginWorkContext(m.ctx)
	if !ok {
		return api.CacheBuildStatus{}, errors.New("daemon is shutting down")
	}
	job := api.CacheBuildStatus{
		JobID: uuid.NewString(), Status: api.CacheBuildQueued, AcceptedAt: time.Now().UTC(),
	}
	m.current = job.JobID
	m.jobs[job.JobID] = job
	m.wg.Add(1)
	go m.execute(job.JobID, mode, done)
	return job, nil
}

func (m *cacheBuildJobs) execute(id string, mode buildCacheMode, done func()) {
	defer m.wg.Done()
	defer done()
	m.mu.Lock()
	job := m.jobs[id]
	job.Status = api.CacheBuildRunning
	m.jobs[id] = job
	m.mu.Unlock()
	err := m.run(m.ctx, mode)
	m.mu.Lock()
	job = m.jobs[id]
	finishedAt := time.Now().UTC()
	job.FinishedAt = &finishedAt
	if err != nil {
		job.Status = api.CacheBuildFailed
		job.Error = "analytics cache build failed; see daemon logs"
		logger.Error("background analytics cache build failed", "job_id", id, "error", err)
		if mode == buildCacheModeScheduledAuto {
			m.lastVerification = time.Time{}
			m.verificationRetryAt = finishedAt.Add(time.Minute)
		}
	} else {
		job.Status = api.CacheBuildPublished
		if mode == buildCacheModeScheduledAuto {
			m.verificationRetryAt = time.Time{}
		}
	}
	m.jobs[id] = job
	if m.current == id {
		m.current = ""
	}
	if m.pending && m.ctx.Err() == nil {
		pendingMode := m.pendingMode
		m.pending = false
		m.pendingMode = buildCacheModeDefault
		if _, startErr := m.startLocked(pendingMode); startErr != nil {
			logger.Error("queue follow-up analytics cache build failed", "error", startErr)
		}
	}
	m.mu.Unlock()
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
