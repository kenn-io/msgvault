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

// deletionAccountResolver is the optional engine capability for mapping
// staged Gmail IDs back to their owning account. Staging requires it:
// delete-staged executes a manifest against a single mailbox chosen via
// Manifest.Filters.Account, so every staged manifest must carry exactly
// one account.
type deletionAccountResolver interface {
	GetAccountsByGmailIDs(ctx context.Context, gmailIDs []string) ([]string, error)
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
	Filter         *StageDeletionFilter `json:"filter,omitempty"`
	MessageIDs     []int64              `json:"message_ids,omitempty"`
	Selection      *ExploreSelection    `json:"selection,omitempty"`
	OperationToken string               `json:"operation_token,omitempty"`
	Description    string               `json:"description,omitempty"`
	DryRun         bool                 `json:"dry_run,omitempty"`
}

// StageDeletionResponse covers both dry-run (200) and create (201).
type StageDeletionResponse struct {
	DryRun         bool     `json:"dry_run"`
	MessageCount   int      `json:"message_count"`
	Account        string   `json:"account,omitempty"`
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

type DeletionManifestDetail struct {
	ID           string              `json:"id"`
	Status       string              `json:"status"`
	CreatedAt    time.Time           `json:"created_at"`
	CreatedBy    string              `json:"created_by"`
	Description  string              `json:"description"`
	Account      string              `json:"account,omitempty"`
	MessageCount int                 `json:"message_count"`
	Summary      *deletion.Summary   `json:"summary,omitempty"`
	Execution    *deletion.Execution `json:"execution,omitempty"`
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

	// Reject unknown fields: a typo in a narrowing filter key would
	// otherwise be silently dropped while a remaining broad criterion
	// stages far more messages than intended.
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req StageDeletionRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("invalid JSON request body: %v", err))
		return
	}
	if req.Selection != nil && (!req.Filter.isEmpty() || len(req.MessageIDs) > 0) {
		writeError(w, http.StatusBadRequest, "invalid_request", "selection cannot be combined with filter or message_ids")
		return
	}
	if req.Filter.isEmpty() && len(req.MessageIDs) == 0 {
		if req.Selection == nil {
			writeError(w, http.StatusBadRequest, "empty_filter",
				"At least one filter criterion or message_ids entry is required; staging the entire archive is not supported")
			return
		}
	}
	if req.Selection == nil && !req.Filter.isEmpty() && !req.DryRun {
		writeError(w, http.StatusPreconditionRequired, "preflight_required",
			"Deletion staging requires the exact selection and operation token returned by preflight")
		return
	}

	var gmailIDs []string
	if req.Selection != nil {
		var ok bool
		gmailIDs, ok = s.resolveAuthorizedDeletionSelection(w, r, *req.Selection, req.OperationToken, req.DryRun)
		if !ok {
			return
		}
	} else {
		var httpErr *apiHTTPError
		gmailIDs, httpErr = s.resolveStageDeletionIDs(r.Context(), &req)
		if httpErr != nil {
			writeAPIHTTPError(w, httpErr)
			return
		}
	}
	if len(gmailIDs) == 0 {
		writeError(w, http.StatusBadRequest, "no_messages_matched", "No messages matched the given criteria")
		return
	}

	account, httpErr := s.resolveStageDeletionAccount(r.Context(), gmailIDs)
	if httpErr != nil {
		writeAPIHTTPError(w, httpErr)
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
			Account:        account,
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
	// delete-staged selects the mailbox to execute against from
	// Filters.Account; without it an API-staged manifest cannot be
	// executed (or worse, could be forced onto the wrong account with
	// --account).
	manifest.Filters.Account = account
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
		Account:      account,
		ID:           manifest.ID,
		Status:       string(manifest.Status),
	})
}

func (s *Server) resolveAuthorizedDeletionSelection(
	w http.ResponseWriter,
	r *http.Request,
	selection ExploreSelection,
	token string,
	dryRun bool,
) ([]string, bool) {
	if token == "" {
		writeError(w, http.StatusPreconditionRequired, "preflight_required",
			"operation_token from deletion preflight is required")
		return nil, false
	}
	if selection.Mode != "explicit" && selection.Mode != "all_matching" {
		writeError(w, http.StatusBadRequest, "invalid_selection", "selection mode must be explicit or all_matching")
		return nil, false
	}
	predicate, err := prepareExplorePredicate(selection.Predicate)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_selection_predicate", err.Error())
		return nil, false
	}
	if selection.CacheRevision == "" {
		writeError(w, http.StatusBadRequest, "invalid_selection", "cache_revision is required")
		return nil, false
	}
	if predicate.request.SearchMode == exploreSearchModeSemantic || predicate.request.SearchMode == exploreSearchModeHybrid {
		if selection.CandidateSnapshotID == "" {
			writeError(w, http.StatusBadRequest, "candidate_snapshot_required",
				"Semantic and hybrid deletion staging require the server-issued candidate snapshot")
			return nil, false
		}
		predicate.request.CandidateSnapshotID = selection.CandidateSnapshotID
	}
	searchSpec, _, resolved := s.resolveExploreSearch(r.Context(), w, predicate.request)
	if !resolved || !requireCompleteCandidatePool(w, searchSpec) {
		return nil, false
	}
	predicate.query.Search = searchSpec
	selectionRequest := query.ExploreSelectionRequest{
		Explore: predicate.query, ExcludedKeys: selection.Exclusions, IncludeDeletableMessageIDs: true,
	}
	if selection.Mode == "explicit" {
		if selection.RowKeys == nil {
			writeError(w, http.StatusBadRequest, "invalid_selection", "explicit selection requires row_keys")
			return nil, false
		}
		selectionRequest.IncludedKeys = selection.RowKeys
	}
	analyzer, ok := s.engine.(query.Explorer)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return nil, false
	}
	stats, err := analyzer.ExploreSelectionStats(r.Context(), selectionRequest)
	if err != nil {
		s.writeExploreError(w, err)
		return nil, false
	}
	if selection.CacheRevision != stats.CacheRevision {
		writeError(w, http.StatusConflict, "archive_revision_changed", "The committed analytical cache changed; run preflight again")
		return nil, false
	}
	if searchSpec.Mode != query.SearchNone && !sameSearchProvenance(selection.SearchProvenance, stats.SearchProvenance) {
		writeError(w, http.StatusConflict, "search_revision_changed", "The search index revision changed; run preflight again")
		return nil, false
	}
	selectionHash := hashCanonicalValue(&selection, false)
	state := s.exploreState
	if state == nil {
		writeError(w, http.StatusConflict, "operation_token_invalid", "Deletion preflight expired; run preflight again")
		return nil, false
	}
	grant, authorized := state.operation(token, selectionHash)
	if !authorized || grant.Revision != stats.CacheRevision || grant.Count != stats.Count {
		writeError(w, http.StatusConflict, "operation_token_invalid", "Deletion preflight expired or no longer matches; run preflight again")
		return nil, false
	}
	if stats.Count == 0 {
		writeError(w, http.StatusBadRequest, "no_messages_matched", "No messages matched the reviewed selection")
		return nil, false
	}
	if stats.DeletableCount != stats.Count {
		writeError(w, http.StatusConflict, "selection_not_deletable",
			"The reviewed selection contains items that cannot be deleted from their source")
		return nil, false
	}
	resolver, ok := s.engine.(deletionMessageIDResolver)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable",
			"selection deletion staging is not supported by this query engine")
		return nil, false
	}
	gmailIDs, err := resolver.GetGmailIDsByMessageIDs(r.Context(), stats.DeletableMessageIDs)
	if err != nil {
		s.logger.Error("stage deletion selection resolution failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Gmail ID query failed")
		return nil, false
	}
	if int64(len(gmailIDs)) != stats.Count {
		writeError(w, http.StatusConflict, "selection_changed",
			"The reviewed selection changed before staging; run preflight again")
		return nil, false
	}
	if !dryRun {
		consumed, taken := state.takeOperation(token, selectionHash)
		if !taken || consumed.Revision != grant.Revision || consumed.Count != grant.Count {
			writeError(w, http.StatusConflict, "operation_token_invalid", "Deletion preflight was already used or expired; run preflight again")
			return nil, false
		}
	}
	return gmailIDs, true
}

// resolveStageDeletionAccount maps the staged Gmail IDs to their owning
// account and requires exactly one: deletion manifests execute against a
// single mailbox, so selections spanning multiple Gmail accounts must be
// split into per-account requests (e.g. scoped with filter.source_id).
func (s *Server) resolveStageDeletionAccount(ctx context.Context, gmailIDs []string) (string, *apiHTTPError) {
	resolver, ok := s.engine.(deletionAccountResolver)
	if !ok {
		return "", newAPIHTTPError(http.StatusServiceUnavailable, "engine_unavailable",
			"deletion staging is not supported by this query engine")
	}
	accounts, err := resolver.GetAccountsByGmailIDs(ctx, gmailIDs)
	if err != nil {
		s.logger.Error("stage deletion account resolution failed", "error", err)
		return "", newAPIHTTPError(http.StatusInternalServerError, "internal_error", "Account resolution failed")
	}
	switch len(accounts) {
	case 1:
		return accounts[0], nil
	case 0:
		// The IDs were just resolved from live Gmail messages, so an
		// empty account set means the archive changed underneath us.
		return "", newAPIHTTPError(http.StatusConflict, "account_resolution_conflict",
			"Staged messages no longer resolve to a Gmail account; retry the request")
	default:
		return "", newAPIHTTPError(http.StatusBadRequest, "multi_account_selection",
			fmt.Sprintf("selection spans multiple Gmail accounts (%s); deletion manifests execute against a single mailbox — scope the request with filter.source_id or stage per account",
				strings.Join(accounts, ", ")))
	}
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
	sort.SliceStable(summaries, func(i, j int) bool {
		return summaries[i].CreatedAt.After(summaries[j].CreatedAt)
	})
	writeJSON(w, http.StatusOK, ListDeletionsResponse{Manifests: summaries})
}

func (s *Server) handleGetDeletion(w http.ResponseWriter, r *http.Request) {
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
	manifest, status, err := canceller.GetDeletionManifest(r.Context(), id)
	if errors.Is(err, deletion.ErrManifestNotFound) {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("deletion manifest %q not found", id))
		return
	}
	if err != nil {
		s.logger.Error("failed to load deletion manifest", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load deletion manifest")
		return
	}
	writeJSON(w, http.StatusOK, DeletionManifestDetail{
		ID: manifest.ID, Status: string(status), CreatedAt: manifest.CreatedAt,
		CreatedBy: manifest.CreatedBy, Description: manifest.Description,
		Account: manifest.Filters.Account, MessageCount: len(manifest.GmailIDs),
		Summary: manifest.Summary, Execution: manifest.Execution,
	})
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
