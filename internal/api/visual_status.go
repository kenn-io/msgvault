package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/vector/visual"
)

// A coverage scan re-reads every candidate blob, so its budget is far below
// the change feed's: one scan per interval with no burst headroom.
const (
	visualCoverageScansPerSecond = 1.0 / 15
	visualCoverageScanBurst      = 1
)

type visualOperationAction string

const (
	visualOperationBuild  visualOperationAction = "build"
	visualOperationResume visualOperationAction = "resume"
	visualOperationRetry  visualOperationAction = "retry"
)

func visualOperationPassScope(
	ctx context.Context, action visualOperationAction, target *visualRetryRequest, startedAt time.Time,
) (operations.PassScope, error) {
	switch action {
	case visualOperationBuild, visualOperationResume, visualOperationRetry:
	default:
		return operations.PassScope{}, errors.New("visual operation action is invalid")
	}
	requestID := requestIDFromContext(ctx)
	if requestID == "" {
		return operations.PassScope{}, errors.New("visual operation request ID is required")
	}
	identity := "msgvault:visual-operation:v1\x00" + string(action) + "\x00" + requestID
	if action == visualOperationRetry {
		if target == nil {
			return operations.PassScope{}, errors.New("visual retry target is required")
		}
		identity += fmt.Sprintf("\x00%d\x00%s", target.MessageID, target.BlobHash)
	}
	digest := sha256.Sum256([]byte(identity))
	scope := operations.PassScope{
		Key:     fmt.Sprintf("http:visual:%s:%x", action, digest),
		Trigger: operations.TriggerManual, StartedAt: startedAt.UTC(),
	}
	if err := scope.Validate(); err != nil {
		return operations.PassScope{}, err
	}
	return scope, nil
}

type visualBuildRequest struct {
	Consent bool `json:"consent"`
}

type visualRetryRequest struct {
	MessageID int64  `json:"message_id"`
	BlobHash  string `json:"blob_hash"`
}

type visualRetireRequest struct {
	GenerationID int64 `json:"generation_id"`
}

func (s *Server) handleVisualStatus(w http.ResponseWriter, r *http.Request) {
	s.vectorMu.RLock()
	statusFn := s.visualStatus
	s.vectorMu.RUnlock()
	if statusFn == nil {
		writeError(w, http.StatusServiceUnavailable, "visual_search_not_ready", "Visual attachment search is not initialized")
		return
	}
	// Progress counters are cheap; the per-format coverage scan re-reads
	// every candidate blob, so it is opt-in, origin-guarded, rate-limited
	// with no trusted-loopback exemption, and serialized — on a keyless
	// loopback daemon a hostile page could otherwise sustain archive-wide
	// scans through ambient cross-origin GETs.
	includeCoverage := r.URL.Query().Get("coverage") == "1"
	if includeCoverage {
		if s.requestAuthentication(r).Mode == AuthModeLoopback &&
			s.crossOriginAmbientReadRequest(r) {
			writeError(w, http.StatusForbidden, "cross_origin_loopback",
				"Keyless loopback coverage scans must be same-origin; "+
					"configure an API key for cross-origin access")
			return
		}
		if !s.visualCoverageRateLimiter.Allow(clientIP(r)) {
			writeRateLimitExceeded(w)
			return
		}
		if !s.visualCoverageScan.TryLock() {
			writeError(w, http.StatusTooManyRequests, "visual_coverage_busy",
				"A coverage scan is already running; retry shortly or omit coverage=1")
			return
		}
		defer s.visualCoverageScan.Unlock()
	}
	status, err := statusFn(r.Context(), includeCoverage)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "visual_status_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleVisualRun(w http.ResponseWriter, r *http.Request) {
	s.vectorMu.RLock()
	run, statusFn := s.visualRun, s.visualStatus
	s.vectorMu.RUnlock()
	s.runVisualOperation(w, r, run, statusFn, visualOperationResume, nil, "visual_resume_failed")
}

func (s *Server) handleVisualBuild(w http.ResponseWriter, r *http.Request) {
	var request visualBuildRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !request.Consent {
		writeError(w, http.StatusBadRequest, "visual_consent_required", "Explicit hosted-processing consent is required")
		return
	}
	s.vectorMu.RLock()
	build, statusFn := s.visualBuild, s.visualStatus
	s.vectorMu.RUnlock()
	s.runVisualOperation(w, r, build, statusFn, visualOperationBuild, nil, "visual_build_failed")
}

func (s *Server) handleVisualRetry(w http.ResponseWriter, r *http.Request) {
	var request visualRetryRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.MessageID <= 0 || strings.TrimSpace(request.BlobHash) == "" {
		writeError(w, http.StatusBadRequest, "invalid_visual_owner", "message_id and blob_hash are required")
		return
	}
	// Match RetryOwner's canonical target before deriving the invocation key.
	request.BlobHash = strings.ToLower(strings.TrimSpace(request.BlobHash))
	s.vectorMu.RLock()
	retry, statusFn := s.visualRetry, s.visualStatus
	s.vectorMu.RUnlock()
	if retry == nil {
		writeError(w, http.StatusServiceUnavailable, "visual_search_not_ready", "Visual attachment search is not initialized")
		return
	}
	s.runVisualOperation(w, r, func(ctx context.Context, scope operations.PassScope) error {
		return retry(ctx, scope, request.MessageID, request.BlobHash)
	}, statusFn, visualOperationRetry, &request, "visual_retry_failed")
}

func (s *Server) runVisualOperation(
	w http.ResponseWriter,
	r *http.Request,
	run func(context.Context, operations.PassScope) error,
	statusFn func(context.Context, bool) (visual.Status, error),
	action visualOperationAction,
	target *visualRetryRequest,
	errorCode string,
) {
	if run == nil || statusFn == nil {
		writeError(w, http.StatusServiceUnavailable, "visual_search_not_ready", "Visual attachment search is not initialized")
		return
	}
	scope, err := visualOperationPassScope(r.Context(), action, target, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "visual_operation_scope_failed", "Visual operation request identity is unavailable")
		return
	}
	if err := run(r.Context(), scope); err != nil {
		writeError(w, http.StatusBadGateway, errorCode, err.Error())
		return
	}
	// Operation responses feed polling loops after every pass, so they skip
	// the full coverage scan and report only progress counters.
	status, err := statusFn(r.Context(), false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "visual_status_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleVisualRetire(w http.ResponseWriter, r *http.Request) {
	var request visualRetireRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.GenerationID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_visual_generation", "generation_id must be positive")
		return
	}
	s.vectorMu.RLock()
	retire, statusFn := s.visualRetire, s.visualStatus
	s.vectorMu.RUnlock()
	if retire == nil || statusFn == nil {
		writeError(w, http.StatusServiceUnavailable, "visual_search_not_ready", "Visual attachment search is not initialized")
		return
	}
	status, err := statusFn(r.Context(), false)
	if err != nil || status.Generation.ID != request.GenerationID {
		writeError(w, http.StatusConflict, "visual_generation_changed", "The configured visual generation does not match")
		return
	}
	if err := retire(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "visual_retire_failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
