package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"go.kenn.io/msgvault/internal/vector/visual"
)

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
	status, err := statusFn(r.Context())
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
	s.runVisualOperation(w, r, run, statusFn, "visual_resume_failed")
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
	s.runVisualOperation(w, r, build, statusFn, "visual_build_failed")
}

func (s *Server) handleVisualRetry(w http.ResponseWriter, r *http.Request) {
	var request visualRetryRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.MessageID <= 0 || strings.TrimSpace(request.BlobHash) == "" {
		writeError(w, http.StatusBadRequest, "invalid_visual_owner", "message_id and blob_hash are required")
		return
	}
	s.vectorMu.RLock()
	retry, statusFn := s.visualRetry, s.visualStatus
	s.vectorMu.RUnlock()
	if retry == nil {
		writeError(w, http.StatusServiceUnavailable, "visual_search_not_ready", "Visual attachment search is not initialized")
		return
	}
	s.runVisualOperation(w, r, func(ctx context.Context) error {
		return retry(ctx, request.MessageID, request.BlobHash)
	}, statusFn, "visual_retry_failed")
}

func (s *Server) runVisualOperation(
	w http.ResponseWriter,
	r *http.Request,
	run func(context.Context) error,
	statusFn func(context.Context) (visual.Status, error),
	errorCode string,
) {
	if run == nil || statusFn == nil {
		writeError(w, http.StatusServiceUnavailable, "visual_search_not_ready", "Visual attachment search is not initialized")
		return
	}
	if err := run(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, errorCode, err.Error())
		return
	}
	status, err := statusFn(r.Context())
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
	status, err := statusFn(r.Context())
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
