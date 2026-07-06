package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/query"
)

// stageDeletionSampleSize caps the dry-run Gmail-ID preview.
const stageDeletionSampleSize = 10

// deletionMessageIDResolver is the optional engine capability for
// resolving internal message IDs to Gmail IDs. SQLite/DuckDB engines
// implement it; the daemonclient HTTP engine does not need to.
type deletionMessageIDResolver interface {
	GetGmailIDsByMessageIDs(ctx context.Context, ids []int64) ([]string, error)
}

// DeletionManifestLister lists staged deletion manifests. Implemented by
// the serve daemon's store adapter; status "" means all statuses.
type DeletionManifestLister interface {
	ListDeletionManifests(ctx context.Context, status deletion.Status) ([]*deletion.Manifest, error)
}

// DeletionManifestCanceller resolves and cancels staged deletion
// manifests. GetDeletionManifest returns the directory-derived status;
// not-found errors wrap deletion.ErrManifestNotFound.
type DeletionManifestCanceller interface {
	GetDeletionManifest(ctx context.Context, id string) (*deletion.Manifest, deletion.Status, error)
	CancelDeletionManifest(ctx context.Context, id string) error
}

// StageDeletionFilter selects messages to stage. All fields optional,
// but the effective request must contain at least one criterion.
type StageDeletionFilter struct {
	Sender        string `json:"sender,omitempty"`
	SenderName    string `json:"sender_name,omitempty"`
	Recipient     string `json:"recipient,omitempty"`
	RecipientName string `json:"recipient_name,omitempty"`
	Domain        string `json:"domain,omitempty"`
	Label         string `json:"label,omitempty"`
	SourceID      *int64 `json:"source_id,omitempty"`
	After         string `json:"after,omitempty"`
	Before        string `json:"before,omitempty"`
}

func (f *StageDeletionFilter) isEmpty() bool {
	return f == nil || (f.Sender == "" && f.SenderName == "" && f.Recipient == "" &&
		f.RecipientName == "" && f.Domain == "" && f.Label == "" &&
		f.SourceID == nil && f.After == "" && f.Before == "")
}

func (f *StageDeletionFilter) toMessageFilter() (query.MessageFilter, *apiHTTPError) {
	var mf query.MessageFilter
	mf.Sender = f.Sender
	mf.SenderName = f.SenderName
	mf.Recipient = f.Recipient
	mf.RecipientName = f.RecipientName
	mf.Domain = f.Domain
	mf.Label = f.Label
	mf.SourceID = f.SourceID
	if f.After != "" {
		ts, err := parseAPITime(f.After)
		if err != nil {
			return mf, newAPIHTTPError(http.StatusBadRequest, "invalid_date",
				fmt.Sprintf("filter field %q must be an RFC3339 or YYYY-MM-DD date, got %q", "after", f.After))
		}
		mf.After = &ts
	}
	if f.Before != "" {
		ts, err := parseAPITime(f.Before)
		if err != nil {
			return mf, newAPIHTTPError(http.StatusBadRequest, "invalid_date",
				fmt.Sprintf("filter field %q must be an RFC3339 or YYYY-MM-DD date, got %q", "before", f.Before))
		}
		mf.Before = &ts
	}
	return mf, nil
}

// StageDeletionRequest is the POST /api/v1/deletions body.
type StageDeletionRequest struct {
	Filter      *StageDeletionFilter `json:"filter,omitempty"`
	MessageIDs  []int64              `json:"message_ids,omitempty"`
	Description string               `json:"description,omitempty"`
	DryRun      bool                 `json:"dry_run,omitempty"`
}

// StageDeletionResponse covers both dry-run (200) and create (201).
type StageDeletionResponse struct {
	DryRun         bool     `json:"dry_run"`
	MessageCount   int      `json:"message_count"`
	SampleGmailIDs []string `json:"sample_gmail_ids,omitempty"`
	ID             string   `json:"id,omitempty"`
	Status         string   `json:"status,omitempty"`
}

// DeletionManifestSummary is one row of GET /api/v1/deletions.
type DeletionManifestSummary struct {
	ID           string    `json:"id"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	CreatedBy    string    `json:"created_by"`
	Description  string    `json:"description"`
	MessageCount int       `json:"message_count"`
}

// ListDeletionsResponse is the GET /api/v1/deletions body.
type ListDeletionsResponse struct {
	Manifests []DeletionManifestSummary `json:"manifests"`
}

// CancelDeletionResponse is the DELETE /api/v1/deletions/{id} body.
type CancelDeletionResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func (s *Server) handleStageDeletion(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "Query engine not available")
		return
	}
	saver, ok := s.store.(CLIDeletionManifestSaver)
	if !ok {
		writeAPIHTTPError(w, cliStoreUnavailableError())
		return
	}

	var req StageDeletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON request body")
		return
	}
	if req.Filter.isEmpty() && len(req.MessageIDs) == 0 {
		writeError(w, http.StatusBadRequest, "empty_filter",
			"At least one filter criterion or message_ids entry is required; staging the entire archive is not supported")
		return
	}

	gmailIDs, httpErr := s.resolveStageDeletionIDs(r.Context(), &req)
	if httpErr != nil {
		writeAPIHTTPError(w, httpErr)
		return
	}
	if len(gmailIDs) == 0 {
		writeError(w, http.StatusBadRequest, "no_messages_matched", "No messages matched the given criteria")
		return
	}

	if req.DryRun {
		sample := gmailIDs
		if len(sample) > stageDeletionSampleSize {
			sample = sample[:stageDeletionSampleSize]
		}
		writeJSON(w, http.StatusOK, StageDeletionResponse{
			DryRun:         true,
			MessageCount:   len(gmailIDs),
			SampleGmailIDs: sample,
		})
		return
	}

	description := strings.TrimSpace(req.Description)
	if description == "" {
		description = "staged via API"
	}
	manifest := deletion.NewManifest(description, gmailIDs)
	manifest.CreatedBy = "api"
	manifest.Filters = manifestFiltersFromRequest(req.Filter)
	raw, err := json.Marshal(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request is not serializable")
		return
	}
	manifest.RawFilter = raw

	if err := saver.SaveCLIDeletionManifest(r.Context(), manifest); err != nil {
		s.logger.Error("failed to save staged deletion manifest", "id", manifest.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "stage_deletion_failed", "Failed to save deletion manifest")
		return
	}
	writeJSON(w, http.StatusCreated, StageDeletionResponse{
		MessageCount: len(gmailIDs),
		ID:           manifest.ID,
		Status:       string(manifest.Status),
	})
}

// resolveStageDeletionIDs unions filter-resolved and explicitly listed
// message IDs into a deduplicated, order-preserving Gmail-ID list.
func (s *Server) resolveStageDeletionIDs(ctx context.Context, req *StageDeletionRequest) ([]string, *apiHTTPError) {
	var out []string
	seen := make(map[string]struct{})
	appendIDs := func(ids []string) {
		for _, id := range ids {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}

	if !req.Filter.isEmpty() {
		mf, httpErr := req.Filter.toMessageFilter()
		if httpErr != nil {
			return nil, httpErr
		}
		ids, err := s.engine.GetGmailIDsByFilter(ctx, mf)
		if err != nil {
			s.logger.Error("stage deletion filter query failed", "error", err)
			return nil, newAPIHTTPError(http.StatusInternalServerError, "internal_error", "Gmail ID query failed")
		}
		appendIDs(ids)
	}
	if len(req.MessageIDs) > 0 {
		resolver, ok := s.engine.(deletionMessageIDResolver)
		if !ok {
			return nil, newAPIHTTPError(http.StatusServiceUnavailable, "engine_unavailable",
				"message_ids staging is not supported by this query engine")
		}
		ids, err := resolver.GetGmailIDsByMessageIDs(ctx, req.MessageIDs)
		if err != nil {
			s.logger.Error("stage deletion message-id query failed", "error", err)
			return nil, newAPIHTTPError(http.StatusInternalServerError, "internal_error", "Gmail ID query failed")
		}
		appendIDs(ids)
	}
	return out, nil
}

func (s *Server) handleListDeletions(w http.ResponseWriter, r *http.Request) {
	lister, ok := s.store.(DeletionManifestLister)
	if !ok {
		writeAPIHTTPError(w, cliStoreUnavailableError())
		return
	}
	var status deletion.Status
	if raw := r.URL.Query().Get("status"); raw != "" {
		status = deletion.Status(raw)
		if !deletion.IsValidStatus(status) {
			writeError(w, http.StatusBadRequest, "invalid_status",
				"status must be one of pending, in_progress, completed, failed, cancelled")
			return
		}
	}
	manifests, err := lister.ListDeletionManifests(r.Context(), status)
	if err != nil {
		s.logger.Error("failed to list deletion manifests", "error", err)
		writeError(w, http.StatusInternalServerError, "list_deletions_failed", "Failed to list deletion manifests")
		return
	}
	summaries := make([]DeletionManifestSummary, 0, len(manifests))
	for _, m := range manifests {
		summaries = append(summaries, DeletionManifestSummary{
			ID:           m.ID,
			Status:       string(m.Status),
			CreatedAt:    m.CreatedAt,
			CreatedBy:    m.CreatedBy,
			Description:  m.Description,
			MessageCount: len(m.GmailIDs),
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].CreatedAt.After(summaries[j].CreatedAt)
	})
	writeJSON(w, http.StatusOK, ListDeletionsResponse{Manifests: summaries})
}

func (s *Server) handleCancelDeletion(w http.ResponseWriter, r *http.Request) {
	canceller, ok := s.store.(DeletionManifestCanceller)
	if !ok {
		writeAPIHTTPError(w, cliStoreUnavailableError())
		return
	}
	id := r.PathValue("id")
	if err := deletion.ValidateManifestID(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_manifest_id", err.Error())
		return
	}
	_, status, err := canceller.GetDeletionManifest(r.Context(), id)
	if errors.Is(err, deletion.ErrManifestNotFound) {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("deletion manifest %q not found", id))
		return
	}
	if err != nil {
		s.logger.Error("failed to load deletion manifest", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load deletion manifest")
		return
	}
	if status != deletion.StatusPending && status != deletion.StatusInProgress {
		writeError(w, http.StatusConflict, "not_cancellable",
			fmt.Sprintf("deletion manifest %q has status %q and cannot be cancelled", id, status))
		return
	}
	if err := canceller.CancelDeletionManifest(r.Context(), id); err != nil {
		s.logger.Error("failed to cancel deletion manifest", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "cancel_deletion_failed", "Failed to cancel deletion manifest")
		return
	}
	writeJSON(w, http.StatusOK, CancelDeletionResponse{ID: id, Status: string(deletion.StatusCancelled)})
}

// manifestFiltersFromRequest maps the request fields that
// deletion.Filters can represent; RawFilter preserves the rest.
func manifestFiltersFromRequest(f *StageDeletionFilter) deletion.Filters {
	var out deletion.Filters
	if f == nil {
		return out
	}
	if f.Sender != "" {
		out.Senders = []string{f.Sender}
	}
	if f.Domain != "" {
		out.SenderDomains = []string{f.Domain}
	}
	if f.Recipient != "" {
		out.Recipients = []string{f.Recipient}
	}
	if f.Label != "" {
		out.Labels = []string{f.Label}
	}
	out.After = f.After
	out.Before = f.Before
	return out
}
