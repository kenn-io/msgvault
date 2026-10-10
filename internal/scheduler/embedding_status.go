package scheduler

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/vector"
)

// EmbeddingStatus is live scheduler evidence, including a post-sync pass that
// inherits the account sync's slot instead of acquiring a second one.
func (s *Scheduler) EmbeddingStatus() vector.EmbeddingStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	status := vector.EmbeddingStatus{Job: vector.EmbeddingJobState{State: "idle"}, Scheduler: vector.EmbeddingSchedulerState{
		Registered: s.embed.job != nil, Schedule: s.embedSchedule, RunAfterSync: s.embed.runAfterSync,
	}}
	if s.embedRunning > 0 {
		status.Job.State = "running"
		started := s.embedStartedAt
		status.Job.StartedAt = &started
	} else if s.embedQueued > 0 {
		status.Job.State = "queued"
	}
	if s.embedQueued > 0 {
		queued := s.embedQueuedAt
		status.Job.QueuedAt = &queued
	}
	return status
}

func (s *Scheduler) runEmbeddingJob(job func(context.Context) error, acquire bool) error {
	if acquire {
		s.mu.Lock()
		if s.embedQueued == 0 {
			s.embedQueuedAt = time.Now().UTC()
		}
		s.embedQueued++
		s.mu.Unlock()
		done, ok := s.beginWork("scheduled embedding")
		s.mu.Lock()
		s.embedQueued--
		s.mu.Unlock()
		if !ok {
			return nil
		}
		defer done()
	}
	s.mu.Lock()
	s.embedRunning++
	s.embedStartedAt = time.Now().UTC()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.embedRunning--
		s.mu.Unlock()
	}()
	ctx, end := s.jobContext("embed", false, 0)
	defer end()
	return job(ctx)
}
