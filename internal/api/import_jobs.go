package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

const (
	importJobsEndpointPath = "/api/v1/imports"
	maxImportRequestBytes  = 16 << 10
	maxImportJobs          = 16

	importJobStatusQueued  = "queued"
	importJobStatusRunning = "running"
	importJobStatusDone    = "done"
	importJobStatusFailed  = "failed"
)

var (
	errImportJobQueueFull    = errors.New("import job queue is full")
	errImportJobsClosed      = errors.New("import jobs are shut down")
	errImportAlreadyActive   = errors.New("import already active")
	errImportAmbiguousSource = errors.New("import account is ambiguous")
	errImportUnsyncable      = errors.New("import account is not syncable")
)

// ImportJobRequest describes one bounded historical Gmail or IMAP import.
type ImportJobRequest struct {
	Account  string `json:"account" minLength:"1"`
	After    string `json:"after,omitempty" pattern:"^[0-9]{4}-[0-9]{2}-[0-9]{2}$"`
	Before   string `json:"before,omitempty" pattern:"^[0-9]{4}-[0-9]{2}-[0-9]{2}$"`
	Limit    int    `json:"limit,omitempty" minimum:"0"`
	Query    string `json:"query,omitempty"`
	NoResume bool   `json:"noresume,omitempty"`
}

// ImportJobSummary contains the final archive counters for a completed job.
type ImportJobSummary struct {
	Processed int64 `json:"processed"`
	Added     int64 `json:"added"`
	Updated   int64 `json:"updated"`
	Skipped   int64 `json:"skipped"`
	Errors    int64 `json:"errors"`
}

// ImportJobResponse is the current in-memory state of a historical import.
type ImportJobResponse struct {
	JobID      string            `json:"job_id"`
	Account    string            `json:"account"`
	Status     string            `json:"status" enum:"queued,running,done,failed"`
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
	GetActiveSync(sourceID int64) (*store.SyncRun, error)
	GetLatestSync(sourceID int64) (*store.SyncRun, error)
}

type importJob struct {
	ImportJobResponse

	sourceID       int64
	baselineRunID  int64
	resumableRunID int64
}

type importJobManager struct {
	mu     sync.RWMutex
	jobs   map[string]*importJob
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed bool
}

func newImportJobManager() *importJobManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &importJobManager{
		jobs:   make(map[string]*importJob),
		ctx:    ctx,
		cancel: cancel,
	}
}

func (m *importJobManager) create(
	now time.Time,
	account string,
	sourceID int64,
	baselineRunID int64,
	resumableRunID int64,
) (ImportJobResponse, error) {
	id, err := newImportJobID()
	if err != nil {
		return ImportJobResponse{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ImportJobResponse{}, errImportJobsClosed
	}
	for _, job := range m.jobs {
		if job.sourceID == sourceID &&
			(job.Status == importJobStatusQueued || job.Status == importJobStatusRunning) {
			return ImportJobResponse{}, fmt.Errorf("%w as job %s", errImportAlreadyActive, job.JobID)
		}
	}
	m.pruneTerminalJobsLocked()
	if len(m.jobs) >= maxImportJobs {
		return ImportJobResponse{}, errImportJobQueueFull
	}

	job := &importJob{ //nolint:modernize // Keep the private sibling fields keyed by name.
		ImportJobResponse: ImportJobResponse{
			JobID:     id,
			Account:   account,
			Status:    importJobStatusQueued,
			CreatedAt: now.UTC(),
		},
		sourceID:       sourceID,
		baselineRunID:  baselineRunID,
		resumableRunID: resumableRunID,
	}
	m.jobs[id] = job
	m.wg.Add(1)
	return cloneImportJobResponse(job.ImportJobResponse), nil
}

func (m *importJobManager) pruneTerminalJobsLocked() {
	for len(m.jobs) >= maxImportJobs {
		var oldestID string
		var oldestTime time.Time
		for id, job := range m.jobs {
			if job.Status == importJobStatusQueued || job.Status == importJobStatusRunning {
				continue
			}
			if oldestID == "" || job.CreatedAt.Before(oldestTime) {
				oldestID = id
				oldestTime = job.CreatedAt
			}
		}
		if oldestID == "" {
			return
		}
		delete(m.jobs, oldestID)
	}
}

func (m *importJobManager) get(id string) (ImportJobResponse, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.jobs[id]
	if !ok {
		return ImportJobResponse{}, false
	}
	return cloneImportJobResponse(job.ImportJobResponse), true
}

func (m *importJobManager) markRunning(id string, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok || job.Status != importJobStatusQueued {
		return
	}
	startedAt := now.UTC()
	job.Status = importJobStatusRunning
	job.StartedAt = &startedAt
}

func (m *importJobManager) updateProgress(id string, run *store.SyncRun) {
	if run == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok || job.Status != importJobStatusRunning || !job.acceptsRun(run) {
		return
	}
	job.Processed, job.Added, job.Skipped = importRunCounts(run)
}

func (j *importJob) acceptsRun(run *store.SyncRun) bool {
	if j == nil || run == nil || run.SourceID != j.sourceID {
		return false
	}
	if j.resumableRunID > 0 && run.ID == j.resumableRunID {
		return true
	}
	if run.ID <= j.baselineRunID {
		return false
	}
	// SQLite records sync timestamps with one-second precision. Compare at the
	// same precision so a run created during the job's starting second is not
	// rejected merely because the in-memory job timestamp has nanoseconds.
	return j.StartedAt == nil || !run.StartedAt.Before(j.StartedAt.Truncate(time.Second))
}

func (m *importJobManager) acceptsRun(id string, run *store.SyncRun) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.jobs[id].acceptsRun(run)
}

func (m *importJobManager) markDone(id string, run *store.SyncRun, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok || !job.acceptsRun(run) {
		return
	}
	processed, added, skipped := importRunCounts(run)
	updated := run.MessagesUpdated
	finishedAt := now.UTC()
	if run.CompletedAt.Valid {
		finishedAt = run.CompletedAt.Time.UTC()
	}
	job.Status = importJobStatusDone
	job.Processed = processed
	job.Added = added
	job.Skipped = skipped
	job.Error = ""
	job.FinishedAt = &finishedAt
	job.Summary = &ImportJobSummary{
		Processed: processed,
		Added:     added,
		Updated:   updated,
		Skipped:   skipped,
		Errors:    run.ErrorsCount,
	}
}

func (m *importJobManager) markFailed(id string, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok || job.Status == importJobStatusDone || job.Status == importJobStatusFailed {
		return
	}
	finishedAt := now.UTC()
	job.Status = importJobStatusFailed
	job.Error = "import failed"
	job.FinishedAt = &finishedAt
}

func (m *importJobManager) shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func cloneImportJobResponse(in ImportJobResponse) ImportJobResponse {
	out := in
	if in.StartedAt != nil {
		startedAt := *in.StartedAt
		out.StartedAt = &startedAt
	}
	if in.FinishedAt != nil {
		finishedAt := *in.FinishedAt
		out.FinishedAt = &finishedAt
	}
	if in.Summary != nil {
		summary := *in.Summary
		out.Summary = &summary
	}
	return out
}

func newImportJobID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate import job id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func importRunCounts(run *store.SyncRun) (processed, added, skipped int64) {
	processed = run.MessagesProcessed
	added = run.MessagesAdded
	skipped = max(processed-added-run.MessagesUpdated, 0)
	return processed, added, skipped
}

func (s *Server) registerImportJobRoutes(api huma.API) {
	createOp := rawAPIV1Operation(
		"createImportJob",
		http.MethodPost,
		"/imports",
		"Start a bounded historical import",
	)
	createOp.RequestBody = jsonRequestBodyFor[ImportJobRequest](api)
	createOp.Responses = jsonResponsesFor[ImportJobResponse](api, http.StatusAccepted)
	addErrorResponses(api, createOp.Responses,
		http.StatusBadRequest,
		http.StatusConflict,
		http.StatusNotFound,
		http.StatusRequestEntityTooLarge,
		http.StatusTooManyRequests,
		http.StatusUnauthorized,
		http.StatusUnsupportedMediaType,
		http.StatusUnprocessableEntity,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	)
	registerRawHumaRoute(api, createOp, s.handleCreateImportJob)

	getOp := rawAPIV1Operation(
		"getImportJob",
		http.MethodGet,
		"/imports/{job_id}",
		"Get historical import status",
	)
	getOp.Responses = jsonResponsesFor[ImportJobResponse](api)
	addErrorResponses(api, getOp.Responses, http.StatusNotFound, http.StatusUnauthorized)
	registerRawHumaRoute(api, getOp, s.handleGetImportJob)
}

func (s *Server) handleCreateImportJob(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != applicationJSONMediaType {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Content-Type must be application/json")
		return
	}

	var req ImportJobRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxImportRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				"Import request exceeds 16 KiB")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid import request JSON")
		return
	}
	if !requireSingleJSONValue(w, decoder, "bad_request") {
		return
	}
	req.Account = strings.TrimSpace(req.Account)
	if validationErr := validateImportJobRequest(req); validationErr != "" {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", validationErr)
		return
	}

	runner, runnerOK := s.store.(CLISyncRunner)
	jobStore, storeOK := s.store.(importJobStore)
	if !runnerOK || !storeOK || s.importJobs == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			"Historical imports are unavailable")
		return
	}
	sources, err := jobStore.GetSourcesByIdentifierOrDisplayName(req.Account)
	if err != nil {
		s.logger.Error("failed to resolve historical import account", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"Failed to resolve import account")
		return
	}
	source, err := resolveExactImportSource(sources, req.Account)
	if errors.Is(err, errImportAmbiguousSource) {
		writeError(w, http.StatusConflict, "ambiguous_account",
			"Import account matches multiple syncable sources")
		return
	}
	if errors.Is(err, errImportUnsyncable) {
		writeError(w, http.StatusUnprocessableEntity, "account_not_syncable",
			"Import account must be a Gmail or IMAP source")
		return
	}
	if err != nil || source == nil {
		writeError(w, http.StatusNotFound, "not_found", "Import account not found")
		return
	}
	baselineRunID := int64(0)
	resumableRunID := int64(0)
	latest, err := jobStore.GetLatestSync(source.ID)
	if err == nil {
		baselineRunID = latest.ID
		if !req.NoResume && latest.Status == store.SyncStatusRunning {
			resumableRunID = latest.ID
		}
	} else if !errors.Is(err, store.ErrSyncRunNotFound) {
		s.logger.Error("failed to read historical import baseline", "source_id", source.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"Failed to prepare import job")
		return
	}

	runReq := CLISyncRequest{
		Full:        true,
		SourceID:    source.ID,
		SourceIDSet: true,
		Query:       req.Query,
		NoResume:    req.NoResume,
		Before:      req.Before,
		After:       req.After,
		Limit:       req.Limit,
	}
	job, err := s.importJobs.create(
		s.clockNow(), source.Identifier, source.ID, baselineRunID, resumableRunID,
	)
	if err != nil {
		if errors.Is(err, errImportJobQueueFull) {
			writeError(w, http.StatusTooManyRequests, "import_queue_full",
				"Historical import queue is full")
			return
		}
		if errors.Is(err, errImportAlreadyActive) {
			writeError(w, http.StatusConflict, "import_already_active", err.Error())
			return
		}
		if errors.Is(err, errImportJobsClosed) {
			writeError(w, http.StatusServiceUnavailable, "service_unavailable",
				"Historical imports are unavailable")
			return
		}
		s.logger.Error("failed to create historical import job", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"Failed to create import job")
		return
	}

	go func() {
		defer s.importJobs.wg.Done()
		s.runImportJob(job.JobID, runner, jobStore, runReq)
	}()
	writeJSON(w, http.StatusAccepted, job)
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
		matchesDisplayName := source.DisplayName.Valid &&
			strings.EqualFold(source.DisplayName.String, account)
		if !matchesIdentifier && !matchesDisplayName {
			continue
		}
		selectorFound = true
		if source.SourceType != "gmail" && source.SourceType != "imap" {
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

func (s *Server) runImportJob(
	jobID string,
	runner CLISyncRunner,
	jobStore importJobStore,
	req CLISyncRequest,
) {
	ctx := s.importJobs.ctx
	done, ok := s.beginBackgroundOperationGateWork(ctx, "msgvault historical import")
	if !ok {
		s.importJobs.markFailed(jobID, s.clockNow())
		return
	}
	defer done()

	s.importJobs.markRunning(jobID, s.clockNow())
	if err := runner.RunCLISync(ctx, req, func(CLISyncEvent) error { return nil }); err != nil {
		s.importJobs.markFailed(jobID, s.clockNow())
		s.logger.Error("historical import job failed", "job_id", jobID, "error", err)
		return
	}
	latest, err := jobStore.GetLatestSync(req.SourceID)
	if err != nil || latest == nil || latest.Status != store.SyncStatusCompleted ||
		!s.importJobs.acceptsRun(jobID, latest) {
		s.importJobs.markFailed(jobID, s.clockNow())
		s.logger.Error("historical import job completed without a final sync summary",
			"job_id", jobID, "error", err)
		return
	}
	s.importJobs.markDone(jobID, latest, s.clockNow())
}

func (s *Server) handleGetImportJob(w http.ResponseWriter, r *http.Request) {
	if s.importJobs == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			"Historical imports are unavailable")
		return
	}
	jobID := r.PathValue("job_id")
	job, ok := s.importJobs.get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Import job not found")
		return
	}
	if job.Status == importJobStatusRunning {
		if jobStore, ok := s.store.(importJobStore); ok {
			active, err := jobStore.GetActiveSync(s.importJobSourceID(jobID))
			if err == nil && s.importJobs.acceptsRun(jobID, active) {
				s.importJobs.updateProgress(jobID, active)
				job, _ = s.importJobs.get(jobID)
			} else if !errors.Is(err, store.ErrSyncRunNotFound) {
				s.logger.Warn("failed to refresh historical import progress",
					"job_id", jobID, "error", err)
			}
		}
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) importJobSourceID(jobID string) int64 {
	s.importJobs.mu.RLock()
	defer s.importJobs.mu.RUnlock()
	job := s.importJobs.jobs[jobID]
	if job == nil {
		return 0
	}
	return job.sourceID
}
