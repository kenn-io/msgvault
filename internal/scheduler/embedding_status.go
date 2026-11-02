package scheduler

import "go.kenn.io/msgvault/internal/vector"

// EmbeddingStatus reports the embed job's live scheduler state. Cron,
// post-sync, and continuation passes each wait for gate admission.
func (s *Scheduler) EmbeddingStatus() vector.EmbeddingStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	status := vector.EmbeddingStatus{Job: vector.EmbeddingJobState{State: "idle"}, Scheduler: vector.EmbeddingSchedulerState{
		Registered:   s.genericFuncs[embedJobName] != nil,
		Schedule:     s.genericSchedules[embedJobName],
		RunAfterSync: s.genericAfterSync[embedJobName],
	}}
	if started, running := s.genericStartedAt[embedJobName]; running {
		status.Job.State = "running"
		status.Job.StartedAt = new(started.UTC())
	} else if queued, waiting := s.genericQueuedAt[embedJobName]; waiting {
		status.Job.State = "queued"
		status.Job.QueuedAt = new(queued.UTC())
	}
	return status
}
