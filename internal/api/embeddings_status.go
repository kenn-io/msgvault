package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/vector"
)

// SetEmbeddingStatus installs the status reader; etaWindow is the number of
// recent batches the rate and ETA use, matching a foreground build.
func (s *Server) SetEmbeddingStatus(read func(context.Context, int64) (vector.EmbeddingStatus, error), etaWindow int) {
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	s.embeddingStatus = read
	s.embeddingETAWindow = etaWindow
}

func (s *Server) handleEmbeddingStatus(w http.ResponseWriter, r *http.Request) {
	var sourceID int64
	query, decodeErr := url.ParseQuery(r.URL.RawQuery)
	if decodeErr != nil || query.Has("source_id") {
		id, err := strconv.ParseInt(query.Get("source_id"), 10, 64)
		if decodeErr != nil || err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_source_id", "source_id must be positive")
			return
		}
		sourceID = id
	}
	s.vectorMu.RLock()
	read, etaWindow := s.embeddingStatus, s.embeddingETAWindow
	vectorStatus := s.vectorStatus
	s.vectorMu.RUnlock()
	if read == nil && (vectorStatus == VectorStatusError || vectorStatus == VectorStatusInitializing) {
		writeError(w, http.StatusServiceUnavailable, "embeddings_status_unavailable", "Embedding status is unavailable; initialize or upgrade the archive with the daemon and retry")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	status := vector.EmbeddingStatus{Generation: vector.EmbeddingGenerationStatus{State: "not_initialized"}, Job: vector.EmbeddingJobState{State: "idle"}}
	if read != nil {
		var err error
		status, err = read(ctx, sourceID)
		if err != nil {
			s.logger.Error("read embedding status", "error", err)
			writeError(w, http.StatusServiceUnavailable, "embeddings_status_unavailable", "Embedding status is unavailable; initialize or upgrade the archive with the daemon and retry")
			return
		}
	}
	status.SampledAt, status.SourceID = time.Now().UTC(), sourceID
	label, since, held := s.operationGateHolder()
	status.Scheduler.SlotHeld = held
	status.Scheduler.SlotHolder = embeddingSlotKind(label, held)
	if held {
		status.Scheduler.SlotHeldSince = &since
	}
	scheduled := vector.EmbeddingStatus{Job: vector.EmbeddingJobState{State: "idle"}}
	if reader, ok := s.scheduler.(interface{ EmbeddingStatus() vector.EmbeddingStatus }); ok {
		scheduled = reader.EmbeddingStatus()
		status.Scheduler.Registered = scheduled.Scheduler.Registered
		status.Scheduler.Schedule = scheduled.Scheduler.Schedule
		status.Scheduler.RunAfterSync = scheduled.Scheduler.RunAfterSync
	}
	setEmbeddingJobState(&status, scheduled.Job, held && isManualEmbeddingGateLabel(label), since)
	setEmbeddingDiagnosticsLiveness(&status, etaWindow)
	writeJSON(w, http.StatusOK, status)
}

// setEmbeddingJobState reports a scheduled job, or a manual build that holds
// the operation slot before its operation run starts.
func setEmbeddingJobState(status *vector.EmbeddingStatus, scheduled vector.EmbeddingJobState, manual bool, since time.Time) {
	switch {
	case scheduled.State == "running":
		status.Job = scheduled
		status.Job.HoldingSchedulerSlot = status.Scheduler.SlotHeld
	case manual:
		status.Job = vector.EmbeddingJobState{State: "running", HoldingSchedulerSlot: true, StartedAt: &since}
	case scheduled.State == "queued":
		status.Job = scheduled
	default:
		status.Job = vector.EmbeddingJobState{State: "idle"}
	}
	if status.Job.State != "running" {
		return
	}
	status.Job.Phase = "preparing_or_converging"
	// Until this job's message pass starts, the latest run is an earlier
	// job's. After it finishes, person embedding can keep the job running and
	// its failures still apply.
	if !startedDuringJob(status.LatestRunStartedAt, *status) {
		status.Failed = 0
	}
}

// startedDuringJob reports whether a message run started after the running
// job did. SQLite stores run times to the millisecond, so both sides are
// compared at that precision. A run left running by a killed process started
// before the job and does not count.
func startedDuringJob(runStartedAt *time.Time, status vector.EmbeddingStatus) bool {
	if runStartedAt == nil || status.Job.StartedAt == nil {
		return false
	}
	return !runStartedAt.Truncate(time.Millisecond).Before(status.Job.StartedAt.Truncate(time.Millisecond))
}

// setEmbeddingDiagnosticsLiveness treats a snapshot as live only when it
// belongs to the active operation run and that run started during this job.
// An unfinished snapshot from any other run was interrupted, so its current
// batch and rate are dropped.
func setEmbeddingDiagnosticsLiveness(status *vector.EmbeddingStatus, etaWindow int) {
	d := status.Diagnostics
	if d == nil || d.FinishedAt != nil {
		status.SetRecentRate(status.SampledAt, status.SourceID != 0, etaWindow)
		return
	}
	live := status.Job.State == "running" && status.ActiveRunID != 0 && d.RunID == status.ActiveRunID &&
		startedDuringJob(status.ActiveRunStartedAt, *status)
	if !live {
		interrupted := *d
		interrupted.CurrentBatch = nil
		status.Diagnostics = &interrupted
		status.MessagesPerMinute, status.ETASeconds = nil, nil
		status.WindowBatches, status.WindowSeconds = 0, 0
		return
	}
	if d.CurrentBatch != nil {
		status.Job.Phase = d.CurrentBatch.Phase
		status.Job.PhaseSeconds = max(0, status.SampledAt.Sub(d.CurrentBatch.PhaseStartedAt).Seconds())
	}
	status.SetRecentRate(status.SampledAt, status.SourceID != 0, etaWindow)
}

// embeddingSlotKind maps the slot holder's label to an allowlisted kind; raw
// labels can name accounts.
func embeddingSlotKind(label string, held bool) string {
	switch {
	case !held:
		return "none"
	case label == "scheduled embedding" || isManualEmbeddingGateLabel(label):
		return "embeddings"
	case strings.HasPrefix(label, "scheduled sync of ") || strings.HasPrefix(label, "msgvault sync") ||
		isSourceSyncJobName(label):
		return "sync"
	default:
		return "other"
	}
}

func isManualEmbeddingGateLabel(label string) bool {
	switch label {
	case "msgvault embeddings build", "msgvault embeddings resume", "msgvault build-embeddings":
		return true
	default:
		return false
	}
}
