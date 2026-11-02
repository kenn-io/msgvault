package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/jobctx"
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

// embeddingStatusSourceID reads the optional positive source_id filter; zero
// means unfiltered. A malformed query is rejected rather than ignored.
func embeddingStatusSourceID(r *http.Request) (int64, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, false
	}
	if !query.Has("source_id") {
		return 0, true
	}
	if len(query["source_id"]) != 1 {
		return 0, false
	}
	id, err := strconv.ParseInt(query.Get("source_id"), 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) handleEmbeddingStatus(w http.ResponseWriter, r *http.Request) {
	sourceID, ok := embeddingStatusSourceID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_source_id", "source_id must be supplied once as a positive integer")
		return
	}
	status, etaWindow, err := s.loadEmbeddingStatus(r.Context(), sourceID)
	if err != nil {
		if errors.Is(err, vector.ErrEmbeddingStatusUnknownSource) || errors.Is(err, vector.ErrEmbeddingStatusSourceOutOfScope) {
			writeError(w, http.StatusBadRequest, "invalid_source_id", err.Error())
			return
		}
		writeError(w, http.StatusServiceUnavailable, "embeddings_status_unavailable", err.Error())
		return
	}
	status.SampledAt, status.SourceID = time.Now().UTC(), sourceID
	s.setEmbeddingSchedulerState(&status)
	setEmbeddingDiagnosticsLiveness(&status, etaWindow)
	writeJSON(w, http.StatusOK, status)
}

// loadEmbeddingStatus returns only public refusal messages.
func (s *Server) loadEmbeddingStatus(ctx context.Context, sourceID int64) (vector.EmbeddingStatus, int, error) {
	const unavailable = "embedding status is unavailable; initialize or upgrade the archive with the daemon and retry"
	s.vectorMu.RLock()
	read, etaWindow := s.embeddingStatus, s.embeddingETAWindow
	vectorStatus := s.vectorStatus
	s.vectorMu.RUnlock()
	status := vector.EmbeddingStatus{
		Generation: vector.EmbeddingGenerationStatus{State: "not_initialized"},
		Job:        vector.EmbeddingJobState{State: "idle"},
	}
	if read == nil {
		if vectorStatus == VectorStatusDisabled {
			return status, etaWindow, errors.New("vector search is disabled; set [vector] enabled = true and restart the daemon")
		}
		return status, etaWindow, errors.New(unavailable)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	status, err := read(ctx, sourceID)
	if err != nil {
		if errors.Is(err, vector.ErrEmbeddingStatusUnknownSource) {
			return status, etaWindow, vector.ErrEmbeddingStatusUnknownSource
		}
		if errors.Is(err, vector.ErrEmbeddingStatusSourceOutOfScope) {
			return status, etaWindow, vector.ErrEmbeddingStatusSourceOutOfScope
		}
		s.logger.Error("read embedding status", "error", err)
		if errors.Is(err, context.DeadlineExceeded) {
			return status, etaWindow, errors.New("embedding status took too long to read; retry")
		}
		return status, etaWindow, errors.New(unavailable)
	}
	return status, etaWindow, nil
}

// setEmbeddingSchedulerState adds the shared slot holder and the scheduler's
// view of the embedding job.
func (s *Server) setEmbeddingSchedulerState(status *vector.EmbeddingStatus) {
	label, since, held := s.operationGateHolder()
	status.Scheduler.SlotHeld = held
	status.Scheduler.SlotHolder = embeddingSlotKind(label, held)
	if held {
		status.Scheduler.SlotHeldSince = &since
	}
	scheduled := vector.EmbeddingJobState{State: "idle"}
	if reader, ok := s.scheduler.(interface{ EmbeddingStatus() vector.EmbeddingStatus }); ok {
		view := reader.EmbeddingStatus()
		scheduled = view.Job
		status.Scheduler.Registered = view.Scheduler.Registered
		status.Scheduler.Schedule = view.Scheduler.Schedule
		status.Scheduler.RunAfterSync = view.Scheduler.RunAfterSync
	}
	setEmbeddingJobState(status, scheduled, held && isManualEmbeddingGateLabel(label), since)
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

// setEmbeddingDiagnosticsLiveness distinguishes interrupted runs from delayed diagnostic writes.
func setEmbeddingDiagnosticsLiveness(status *vector.EmbeddingStatus, etaWindow int) {
	d := status.Diagnostics
	if d == nil || d.FinishedAt != nil {
		status.SetRecentRate(status.SampledAt, status.SourceID != 0, etaWindow)
		return
	}
	// A snapshot that stopped updating, for example because its writes keep
	// failing, would otherwise show a frozen phase and a decaying rate.
	fresh := status.SampledAt.Sub(d.UpdatedAt) <= vector.EmbeddingSnapshotStaleAfter
	live := status.Job.State == "running" && status.ActiveRunID != 0 && d.RunID == status.ActiveRunID &&
		startedDuringJob(status.ActiveRunStartedAt, *status)
	status.DiagnosticsStale = !fresh
	if !live || !fresh {
		if live {
			status.Job.Phase = "diagnostics_stale"
			status.Job.PhaseSeconds = 0
		}
		snapshot := *d
		snapshot.CurrentBatch = nil
		status.Diagnostics = &snapshot
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
	case label == jobctx.ScheduledEmbeddingLabel || isManualEmbeddingGateLabel(label):
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
