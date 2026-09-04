package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

const (
	importJobsEndpointPath = "/api/v1/imports"
	maxImportRequestBytes  = 16 << 10
)

var (
	errImportAmbiguousSource = errors.New("import account is ambiguous")
	errImportUnsyncable      = errors.New("import account is not syncable")
)

type ImportJobRequest struct {
	Account  string `json:"account" minLength:"1"`
	After    string `json:"after,omitempty" pattern:"^[0-9]{4}-[0-9]{2}-[0-9]{2}$"`
	Before   string `json:"before,omitempty" pattern:"^[0-9]{4}-[0-9]{2}-[0-9]{2}$"`
	Limit    int    `json:"limit,omitempty" minimum:"0"`
	Query    string `json:"query,omitempty" doc:"Gmail search query; not supported for IMAP sources"`
	NoResume bool   `json:"noresume,omitempty"`
}

type ImportJobSummary struct {
	Processed int64 `json:"processed"`
	Added     int64 `json:"added"`
	Updated   int64 `json:"updated"`
	Skipped   int64 `json:"skipped"`
	Errors    int64 `json:"errors"`
}

type ImportJobResponse struct {
	JobID      string            `json:"job_id"`
	Account    string            `json:"account"`
	Status     string            `json:"status" enum:"pending,running,done,failed"`
	Processed  int64             `json:"processed"`
	Added      int64             `json:"added"`
	Skipped    int64             `json:"skipped"`
	Error      string            `json:"error,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	StartedAt  *time.Time        `json:"started_at"`
	FinishedAt *time.Time        `json:"finished_at"`
	Summary    *ImportJobSummary `json:"summary,omitempty"`
}

type importJobStore interface {
	GetSourcesByIdentifierOrDisplayName(query string) ([]*store.Source, error)
	GetSourceByID(id int64) (*store.Source, error)
	CreateSyncOperation(sourceID int64, operationID string) (*store.SyncOperation, error)
	GetSyncOperation(operationID string) (*store.SyncOperation, error)
}

func (s *Server) registerImportJobRoutes(api huma.API) {
	createOp := rawAPIV1Operation("createImportJob", http.MethodPost, "/imports", "Start a bounded historical import")
	createOp.RequestBody = jsonRequestBodyFor[ImportJobRequest](api)
	createOp.Responses = jsonResponsesFor[ImportJobResponse](api, http.StatusAccepted)
	addErrorResponses(api, createOp.Responses,
		http.StatusBadRequest, http.StatusConflict, http.StatusNotFound,
		http.StatusRequestEntityTooLarge, http.StatusUnauthorized,
		http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity,
		http.StatusInternalServerError, http.StatusServiceUnavailable,
	)
	registerRawHumaRoute(api, createOp, s.handleCreateImportJob)

	getOp := rawAPIV1Operation("getImportJob", http.MethodGet, "/imports/{job_id}", "Get historical import status")
	getOp.Responses = jsonResponsesFor[ImportJobResponse](api)
	addErrorResponses(api, getOp.Responses, http.StatusNotFound, http.StatusUnauthorized)
	registerRawHumaRoute(api, getOp, s.handleGetImportJob)
}

func (s *Server) handleCreateImportJob(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeImportJobRequest(w, r)
	if !ok {
		return
	}
	runner, runnerOK := s.store.(CLISyncRunner)
	jobStore, storeOK := s.store.(importJobStore)
	if !runnerOK || !storeOK {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "Historical imports are unavailable")
		return
	}
	sources, err := jobStore.GetSourcesByIdentifierOrDisplayName(req.Account)
	if err != nil {
		s.logger.Error("failed to resolve historical import account", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to resolve import account")
		return
	}
	source, err := resolveExactImportSource(sources, req.Account)
	switch {
	case errors.Is(err, errImportAmbiguousSource):
		writeError(w, http.StatusConflict, "ambiguous_account", "Import account matches multiple syncable sources")
		return
	case errors.Is(err, errImportUnsyncable):
		writeError(w, http.StatusUnprocessableEntity, "account_not_syncable", "Import account must be a Gmail or IMAP source")
		return
	case err != nil || source == nil:
		writeError(w, http.StatusNotFound, "not_found", "Import account not found")
		return
	}
	if source.SourceType == "imap" && req.Query != "" {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "query is not supported for IMAP imports")
		return
	}

	release, acquired := func() (func(), bool) {
		if s.operationGate == nil {
			return func() {}, true
		}
		return beginGateWorkBounded(r.Context(), s.operationGate, "msgvault historical import")
	}()
	if !acquired {
		writeOperationGateBusy(w, s.operationGate)
		return
	}
	jobID, err := newImportJobID()
	if err != nil {
		release()
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to create import job")
		return
	}
	finishIdleWork, idleAcquired := s.idleTracker.BeginWork()
	if !idleAcquired {
		release()
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "Historical imports are unavailable")
		return
	}
	runReq := CLISyncRequest{
		Full: true, SourceID: source.ID, SourceIDSet: true,
		Query: req.Query, NoResume: req.NoResume, Before: req.Before,
		After: req.After, Limit: req.Limit, OperationID: jobID,
	}
	s.importMu.Lock()
	if s.importsClosed {
		s.importMu.Unlock()
		finishIdleWork()
		release()
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "Historical imports are unavailable")
		return
	}
	s.importWG.Add(1)
	s.importMu.Unlock()
	op, err := jobStore.CreateSyncOperation(source.ID, jobID)
	if err != nil {
		s.importWG.Done()
		finishIdleWork()
		release()
		if errors.Is(err, store.ErrSyncAlreadyActive) {
			writeError(w, http.StatusConflict, "sync_already_active", "Import account already has an active sync")
			return
		}
		s.logger.Error("failed to persist historical import", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to create import job")
		return
	}
	go func() {
		defer s.importWG.Done()
		defer finishIdleWork()
		defer release()
		err := runner.RunCLISync(s.importContext, runReq, func(CLISyncEvent) error { return nil })
		if err != nil {
			s.logger.Error("historical import failed", "job_id", jobID, "error", err)
		}
	}()

	response, err := importJobResponse(jobStore, op)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to read import job")
		return
	}
	writeJSON(w, http.StatusAccepted, response)
}

func decodeImportJobRequest(w http.ResponseWriter, r *http.Request) (ImportJobRequest, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != applicationJSONMediaType {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return ImportJobRequest{}, false
	}
	var req ImportJobRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxImportRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Import request exceeds 16 KiB")
			return ImportJobRequest{}, false
		}
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid import request JSON")
		return ImportJobRequest{}, false
	}
	if !requireSingleJSONValue(w, decoder, "bad_request") {
		return ImportJobRequest{}, false
	}
	req.Account = strings.TrimSpace(req.Account)
	if validationErr := validateImportJobRequest(req); validationErr != "" {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", validationErr)
		return ImportJobRequest{}, false
	}
	return req, true
}

func (s *Server) handleGetImportJob(w http.ResponseWriter, r *http.Request) {
	jobStore, ok := s.store.(importJobStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "Historical imports are unavailable")
		return
	}
	op, err := jobStore.GetSyncOperation(r.PathValue("job_id"))
	if errors.Is(err, store.ErrSyncRunNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Import job not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to read import job")
		return
	}
	response, err := importJobResponse(jobStore, op)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to read import job")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func importJobResponse(jobStore importJobStore, op *store.SyncOperation) (ImportJobResponse, error) {
	if op == nil {
		return ImportJobResponse{}, store.ErrSyncRunNotFound
	}
	source, err := jobStore.GetSourceByID(op.SourceID)
	if err != nil {
		return ImportJobResponse{}, err
	}
	response := ImportJobResponse{
		JobID: op.ID, Account: source.Identifier, Status: op.Status,
		CreatedAt: op.CreatedAt.UTC(),
	}
	if op.StartedAt.Valid {
		startedAt := op.StartedAt.Time.UTC()
		response.StartedAt = &startedAt
	}
	var summary ImportJobSummary
	for _, run := range op.Runs {
		processed, added, skipped := importRunCounts(run)
		summary.Processed += processed
		summary.Added += added
		summary.Updated += run.MessagesUpdated
		summary.Skipped += skipped
		summary.Errors += run.ErrorsCount
	}
	response.Processed = summary.Processed
	response.Added = summary.Added
	response.Skipped = summary.Skipped
	if op.FinishedAt.Valid {
		finishedAt := op.FinishedAt.Time.UTC()
		response.FinishedAt = &finishedAt
	}
	switch op.Status {
	case "done":
		response.Summary = &summary
	case "failed":
		response.Error = "import failed"
	}
	return response, nil
}

func importRunCounts(run *store.SyncRun) (processed, added, skipped int64) {
	processed = run.MessagesProcessed
	added = run.MessagesAdded
	skipped = max(0, processed-added-run.MessagesUpdated)
	return processed, added, skipped
}

func newImportJobID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate import job id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func validateImportJobRequest(req ImportJobRequest) string {
	if req.Account == "" {
		return "account is required"
	}
	if req.Limit < 0 {
		return "limit must be a non-negative integer"
	}
	after, afterSet, err := parseImportDate(req.After)
	if err != nil {
		return "after must use YYYY-MM-DD"
	}
	before, beforeSet, err := parseImportDate(req.Before)
	if err != nil {
		return "before must use YYYY-MM-DD"
	}
	if afterSet && beforeSet && !after.Before(before) {
		return "after must be earlier than before"
	}
	return ""
}

func parseImportDate(raw string) (time.Time, bool, error) {
	if raw == "" {
		return time.Time{}, false, nil
	}
	parsed, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return time.Time{}, true, fmt.Errorf("parse import date: %w", err)
	}
	return parsed, true, nil
}

func resolveExactImportSource(sources []*store.Source, account string) (*store.Source, error) {
	var match *store.Source
	selectorFound := false
	for _, source := range sources {
		if source == nil {
			continue
		}
		matchesIdentifier := strings.EqualFold(source.Identifier, account)
		matchesDisplayName := source.DisplayName.Valid && strings.EqualFold(source.DisplayName.String, account)
		if !matchesIdentifier && !matchesDisplayName {
			continue
		}
		selectorFound = true
		sourceType := source.SourceType
		if sourceType == "" {
			sourceType = "gmail"
		}
		if sourceType != "gmail" && sourceType != "imap" {
			continue
		}
		if match != nil {
			return nil, errImportAmbiguousSource
		}
		match = source
	}
	if match != nil {
		return match, nil
	}
	if selectorFound {
		return nil, errImportUnsyncable
	}
	return nil, store.ErrSourceNotFound
}
