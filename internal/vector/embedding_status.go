package vector

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/operations"
)

const EmbeddingRecentBatchLimit = 20
const EmbeddingSampleMaxAge = 5 * time.Minute

// EmbeddingStatus mirrors the generation/coverage shape of visual.Status.
// Failed counts failures in the latest message-embedding invocation across all sources.
type EmbeddingStatus struct {
	Generation         EmbeddingGenerationStatus  `json:"generation"`
	ActiveGeneration   *EmbeddingGenerationStatus `json:"active_generation"`
	Eligible           int64                      `json:"eligible"`
	Current            int64                      `json:"current"`
	Pending            int64                      `json:"pending"`
	Failed             int64                      `json:"failed"`
	ActiveRunID        int64                      `json:"-"`
	ActiveRunStartedAt *time.Time                 `json:"-"`
	LatestRunStartedAt *time.Time                 `json:"-"`
	SampledAt          time.Time                  `json:"sampled_at"`
	SourceID           int64                      `json:"source_id,omitzero"`
	Job                EmbeddingJobState          `json:"job"`
	Scheduler          EmbeddingSchedulerState    `json:"scheduler"`
	MessagesPerMinute  *float64                   `json:"messages_per_minute"`
	ETASeconds         *float64                   `json:"eta_seconds"`
	WindowBatches      int                        `json:"window_batches"`
	WindowSeconds      float64                    `json:"window_seconds"`
	Diagnostics        *EmbeddingDiagnostics      `json:"diagnostics"`
}

type EmbeddingGenerationStatus struct {
	ID        GenerationID `json:"id"`
	State     string       `json:"state"`
	Model     string       `json:"model"`
	Dimension int          `json:"dimension"`
}

func EmbeddingGenerationView(g Generation) EmbeddingGenerationStatus {
	return EmbeddingGenerationStatus{ID: g.ID, State: string(g.State), Model: g.Model, Dimension: g.Dimension}
}

type EmbeddingJobState struct {
	State                string     `json:"state"`
	StartedAt            *time.Time `json:"started_at"`
	QueuedAt             *time.Time `json:"queued_at"`
	HoldingSchedulerSlot bool       `json:"holding_scheduler_slot"`
	Phase                string     `json:"phase"`
	PhaseSeconds         float64    `json:"phase_seconds"`
}

// EmbeddingSchedulerState reports schedule and shared-slot ownership.
// SlotHolder is an allowlisted kind, never a raw label containing source IDs.
type EmbeddingSchedulerState struct {
	Registered    bool       `json:"registered"`
	Schedule      string     `json:"schedule"`
	RunAfterSync  bool       `json:"run_after_sync"`
	SlotHeld      bool       `json:"slot_held"`
	SlotHolder    string     `json:"slot_holder"`
	SlotHeldSince *time.Time `json:"slot_held_since"`
}

type EmbeddingBatch struct {
	Sequence       int                              `json:"sequence"`
	StartedAt      time.Time                        `json:"started_at"`
	FinishedAt     time.Time                        `json:"finished_at,omitzero"`
	Phase          string                           `json:"phase"`
	PhaseStartedAt time.Time                        `json:"phase_started_at"`
	Attempted      int                              `json:"attempted"`
	Completed      int                              `json:"completed"`
	Chars          int                              `json:"chars"`
	ProviderMS     float64                          `json:"provider_ms"`
	RequestMS      float64                          `json:"request_ms"`
	DBWriteMS      float64                          `json:"db_write_ms"`
	ElapsedMS      float64                          `json:"elapsed_ms"`
	Requests       int                              `json:"requests"`
	Retries        int                              `json:"retries"`
	RateLimits     int                              `json:"rate_limits"`
	Error          *operations.OperationPublicError `json:"error"`
}

// EmbeddingDiagnostics is one bounded snapshot per generation, not run history.
type EmbeddingDiagnostics struct {
	GenerationID          GenerationID                     `json:"generation_id"`
	RunID                 int64                            `json:"run_id"`
	StartedAt             time.Time                        `json:"started_at"`
	UpdatedAt             time.Time                        `json:"updated_at"`
	FinishedAt            *time.Time                       `json:"finished_at"`
	CurrentBatch          *EmbeddingBatch                  `json:"current_batch"`
	RecentBatches         []EmbeddingBatch                 `json:"recent_batches"`
	LastSuccessfulBatchAt *time.Time                       `json:"last_successful_batch_at"`
	LastError             *operations.OperationPublicError `json:"last_error"`
}

type EmbeddingDiagnosticWriter interface {
	SaveEmbeddingDiagnostics(ctx context.Context, snapshot EmbeddingDiagnostics) error
}

// SetRecentRate derives speed from newly committed coverage in the last window
// batches (the eta_window setting), including failed
// batches and the time since the last completion. Unknown/old rates stay null.
func (s *EmbeddingStatus) SetRecentRate(now time.Time, filtered bool, window int) {
	s.MessagesPerMinute, s.ETASeconds = nil, nil
	s.WindowBatches, s.WindowSeconds = 0, 0
	d := s.Diagnostics
	if d == nil || d.GenerationID != s.Generation.ID || len(d.RecentBatches) == 0 {
		return
	}
	batches := d.RecentBatches[max(0, len(d.RecentBatches)-max(1, window)):]
	last := batches[len(batches)-1].FinishedAt
	if last.IsZero() || now.Sub(last) > EmbeddingSampleMaxAge || now.Before(last) {
		return
	}
	end := now
	if d.FinishedAt != nil {
		end = *d.FinishedAt
	}
	seconds := end.Sub(batches[0].StartedAt).Seconds()
	if seconds <= 0 {
		return
	}
	completed := 0
	for _, batch := range batches {
		completed += max(0, batch.Completed)
	}
	s.WindowBatches, s.WindowSeconds = len(batches), seconds
	rate := float64(completed) * 60 / seconds
	s.MessagesPerMinute = &rate
	if rate > 0 && s.Job.State == "running" && !filtered && d.FinishedAt == nil {
		eta := float64(s.Pending) * 60 / rate
		s.ETASeconds = &eta
	}
}
