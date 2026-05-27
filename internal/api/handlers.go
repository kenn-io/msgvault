package api

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/remote"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"golang.org/x/oauth2"
)

// maxPageSize is the hard upper bound for any paginated endpoint.
const maxPageSize = 500

// StatsResponse represents the archive statistics.
type StatsResponse struct {
	TotalMessages int64             `json:"total_messages"`
	TotalThreads  int64             `json:"total_threads"`
	TotalAccounts int64             `json:"total_accounts"`
	TotalLabels   int64             `json:"total_labels"`
	TotalAttach   int64             `json:"total_attachments"`
	DatabaseSize  int64             `json:"database_size_bytes"`
	VectorSearch  *vector.StatsView `json:"vector_search,omitempty"`
}

// APIMessage is an alias for store.APIMessage — single source of truth for
// the message DTO shared between the store and API layers.
type APIMessage = store.APIMessage

// APIAttachment is an alias for store.APIAttachment.
type APIAttachment = store.APIAttachment

// AccountInfo represents an account in list responses.
type AccountInfo struct {
	ID          int64  `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	LastSyncAt  string `json:"last_sync_at,omitempty"`
	NextSyncAt  string `json:"next_sync_at,omitempty"`
	Schedule    string `json:"schedule,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// SchedulerStatusResponse represents scheduler status.
type SchedulerStatusResponse struct {
	Running  bool            `json:"running"`
	Accounts []AccountStatus `json:"accounts"`
}

// ErrorResponse represents an API error.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// MessageSummary represents a message in list responses.
type MessageSummary struct {
	ID             int64    `json:"id"`
	ConversationID int64    `json:"conversation_id,omitempty"`
	Subject        string   `json:"subject"`
	MessageType    string   `json:"message_type,omitempty"`
	From           string   `json:"from"`
	To             []string `json:"to"`
	Cc             []string `json:"cc,omitempty"`
	Bcc            []string `json:"bcc,omitempty"`
	SentAt         string   `json:"sent_at"`
	DeletedAt      string   `json:"deleted_at,omitempty"`
	Snippet        string   `json:"snippet"`
	Labels         []string `json:"labels"`
	HasAttach      bool     `json:"has_attachments"`
	SizeBytes      int64    `json:"size_bytes"`
}

// MessageDetail represents a full message response.
type MessageDetail struct {
	MessageSummary

	Body        string           `json:"body"`
	BodyHTML    string           `json:"body_html,omitempty"`
	Attachments []AttachmentInfo `json:"attachments"`
}

// AttachmentInfo represents attachment metadata in API responses.
type AttachmentInfo struct {
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size_bytes"`
}

// SearchResult represents search results.
type SearchResult struct {
	Query    string           `json:"query"`
	Total    int64            `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
	Messages []MessageSummary `json:"messages"`
}

// hybridSearchResponse represents results from vector or hybrid search.
// PoolSaturated is always emitted so clients can read "pool not
// saturated" as a positive signal rather than an absent field.
type hybridSearchResponse struct {
	Query         string             `json:"query"`
	Mode          string             `json:"mode"`
	Returned      int                `json:"returned"`
	PoolSaturated bool               `json:"pool_saturated"`
	Generation    generationSummary  `json:"generation"`
	TookMS        int64              `json:"took_ms"`
	Results       []hybridSearchItem `json:"results"`
}

// generationSummary describes the active vector-index generation used to
// answer a hybrid/vector query.
type generationSummary struct {
	ID          int64  `json:"id"`
	Model       string `json:"model"`
	Dimension   int    `json:"dimension"`
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
}

// hybridSearchItem is a single hit in a vector/hybrid response. It
// embeds MessageSummary (not APIMessage) so the JSON schema matches
// /api/v1/search's summary surface — callers get the same snake-case
// fields, and we do not leak full message bodies, headers, or
// attachment metadata in search results. Score is present only when
// explain=1 was requested.
type hybridSearchItem struct {
	MessageSummary

	Score *scoreBreakdown `json:"score,omitempty"`
}

// scoreBreakdown exposes fused-score components for debugging. BM25,
// Vector, and RRF are pointer-typed so that "not present in this
// signal" can be distinguished from a legitimate 0.0 score in JSON.
// In particular, mode=vector reports vector with no rrf (RRF requires
// two signals to fuse), and mode=fts reports bm25 with no rrf or vector.
type scoreBreakdown struct {
	RRF            *float64 `json:"rrf,omitempty"`
	BM25           *float64 `json:"bm25,omitempty"`
	Vector         *float64 `json:"vector,omitempty"`
	SubjectBoosted bool     `json:"subject_boosted,omitempty"`
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Headers already sent; if Encode fails mid-stream (broken pipe,
	// non-serializable value) there's no meaningful recovery beyond
	// truncating the response body.
	_ = json.NewEncoder(w).Encode(data) //nolint:errchkjson // any is the public API; mid-response error is unrecoverable
}

// writeError writes an error response.
func writeError(w http.ResponseWriter, status int, err string, message string) {
	writeJSON(w, status, ErrorResponse{Error: err, Message: message})
}

// messageDetailFromQuery builds a MessageDetail response from a query-engine
// MessageDetail, formatting addresses the same way the store path does and
// emitting body_html alongside body when both are present.
func messageDetailFromQuery(qMsg *query.MessageDetail) MessageDetail {
	from := ""
	if len(qMsg.From) > 0 {
		from = formatQueryAddress(qMsg.From[0])
	}

	toAddrs := make([]string, 0, len(qMsg.To))
	for _, a := range qMsg.To {
		toAddrs = append(toAddrs, a.Email)
	}
	ccAddrs := make([]string, 0, len(qMsg.Cc))
	for _, a := range qMsg.Cc {
		ccAddrs = append(ccAddrs, a.Email)
	}
	bccAddrs := make([]string, 0, len(qMsg.Bcc))
	for _, a := range qMsg.Bcc {
		bccAddrs = append(bccAddrs, a.Email)
	}

	labels := qMsg.Labels
	if labels == nil {
		labels = []string{}
	}

	body := qMsg.BodyText
	if body == "" {
		body = qMsg.BodyHTML
	}

	attachments := make([]AttachmentInfo, 0, len(qMsg.Attachments))
	for _, att := range qMsg.Attachments {
		attachments = append(attachments, AttachmentInfo{
			Filename: att.Filename,
			MimeType: att.MimeType,
			Size:     att.Size,
		})
	}

	return MessageDetail{
		MessageSummary: MessageSummary{
			ID:             qMsg.ID,
			ConversationID: qMsg.ConversationID,
			Subject:        qMsg.Subject,
			MessageType:    qMsg.MessageType,
			From:           from,
			To:             toAddrs,
			Cc:             ccAddrs,
			Bcc:            bccAddrs,
			SentAt:         qMsg.SentAt.UTC().Format(time.RFC3339),
			DeletedAt:      formatDeletedAt(qMsg.DeletedAt),
			Snippet:        qMsg.Snippet,
			Labels:         labels,
			HasAttach:      qMsg.HasAttachments,
			SizeBytes:      qMsg.SizeEstimate,
		},
		Body:        body,
		BodyHTML:    qMsg.BodyHTML,
		Attachments: attachments,
	}
}

// toMessageSummary converts an APIMessage to a MessageSummary for API responses.
func toMessageSummary(m APIMessage) MessageSummary {
	to := m.To
	if to == nil {
		to = []string{}
	}
	labels := m.Labels
	if labels == nil {
		labels = []string{}
	}
	return MessageSummary{
		ID:             m.ID,
		ConversationID: m.ConversationID,
		Subject:        m.Subject,
		MessageType:    m.MessageType,
		From:           m.From,
		To:             to,
		Cc:             m.Cc,
		Bcc:            m.Bcc,
		SentAt:         m.SentAt.UTC().Format(time.RFC3339),
		DeletedAt:      formatDeletedAt(m.DeletedAt),
		Snippet:        m.Snippet,
		Labels:         labels,
		HasAttach:      m.HasAttachments,
		SizeBytes:      m.SizeEstimate,
	}
}

// handleStats returns archive statistics.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "Database not available")
		return
	}

	stats, err := s.store.GetStats()
	if err != nil {
		s.logger.Error("failed to get stats", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve statistics")
		return
	}

	// Vector stats are best-effort: log errors but still include
	// whatever partial stats came back.
	vs, vsErr := vector.CollectStats(r.Context(), s.backend)
	if vsErr != nil {
		s.logger.Warn("vector stats", "error", vsErr)
	}

	resp := StatsResponse{
		TotalMessages: stats.MessageCount,
		TotalThreads:  stats.ThreadCount,
		TotalAccounts: stats.SourceCount,
		TotalLabels:   stats.LabelCount,
		TotalAttach:   stats.AttachmentCount,
		DatabaseSize:  stats.DatabaseSize,
		VectorSearch:  vs,
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleListMessages returns a paginated list of messages.
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "Database not available")
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	offset := (page - 1) * pageSize

	messages, total, err := s.store.ListMessages(offset, pageSize)
	if err != nil {
		s.logger.Error("failed to list messages", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve messages")
		return
	}

	summaries := make([]MessageSummary, len(messages))
	for i, m := range messages {
		summaries[i] = toMessageSummary(m)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"messages":  summaries,
	})
}

// handleGetMessage returns a single message by ID.
// When the query engine is available, it returns separate body_html for rich
// rendering; otherwise it falls back to the store layer (plain Body only).
func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "Message ID must be a number")
		return
	}

	if s.engine != nil {
		qMsg, err := s.engine.GetMessage(r.Context(), id)
		switch {
		case err != nil && !isEngineUnsupported(err):
			s.logger.Error("failed to get message via engine", "id", id, "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve message")
			return
		case err == nil && qMsg == nil:
			writeError(w, http.StatusNotFound, "not_found", "Message not found")
			return
		case err == nil:
			writeJSON(w, http.StatusOK, messageDetailFromQuery(qMsg))
			return
		}
		// err is unsupported sentinel — fall through to store path so
		// engines that don't implement GetMessage still serve detail
		// requests via the underlying SQLite store.
	}

	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "Database not available")
		return
	}

	msg, err := s.store.GetMessage(id)
	if err != nil {
		s.logger.Error("failed to get message", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve message")
		return
	}
	if msg == nil {
		writeError(w, http.StatusNotFound, "not_found", "Message not found")
		return
	}

	detail := MessageDetail{
		MessageSummary: toMessageSummary(*msg),
		Body:           msg.Body,
	}

	attachments := make([]AttachmentInfo, 0, len(msg.Attachments))
	for _, att := range msg.Attachments {
		attachments = append(attachments, AttachmentInfo(att))
	}
	detail.Attachments = attachments

	writeJSON(w, http.StatusOK, detail)
}

// handleSearch searches messages.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "Database not available")
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		writeError(w, http.StatusBadRequest, "missing_query", "Query parameter 'q' is required")
		return
	}

	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "fts"
	}
	explain := r.URL.Query().Get("explain") == "1"

	if mode == "vector" || mode == "hybrid" {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page > 1 {
			writeError(w, http.StatusBadRequest, "pagination_unsupported",
				"mode=vector|hybrid only supports page=1")
			return
		}
		pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
		if pageSize < 1 {
			pageSize = 20
		}
		if maxPage := s.vectorCfg.Search.MaxPageSizeHybridClamp(); maxPage > 0 && pageSize > maxPage {
			pageSize = maxPage
		}
		s.handleHybridSearch(w, r, query, mode, explain, pageSize)
		return
	}

	if mode != "fts" {
		writeError(w, http.StatusBadRequest, "invalid_mode",
			fmt.Sprintf("mode must be one of fts|vector|hybrid, got %q", mode))
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	offset := (page - 1) * pageSize

	parsedQuery := search.Parse(query)
	parsedQuery.HideDeleted = true

	var (
		messages []store.APIMessage
		total    int64
		err      error
	)
	if parsedQuery.HasOperators() {
		messages, total, err = s.store.SearchMessagesQuery(parsedQuery, offset, pageSize)
	} else {
		messages, total, err = s.store.SearchMessages(query, offset, pageSize)
	}
	if err != nil {
		s.logger.Error("search failed", "query", query, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Search failed")
		return
	}

	summaries := make([]MessageSummary, len(messages))
	for i, m := range messages {
		summaries[i] = toMessageSummary(m)
	}

	writeJSON(w, http.StatusOK, SearchResult{
		Query:    query,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
		Messages: summaries,
	})
}

// handleHybridSearch runs vector or hybrid search via the configured
// hybrid engine. Returns 503 when the engine is not configured or the
// index is stale/building; otherwise returns RRF-ranked hits hydrated
// through the message store.
func (s *Server) handleHybridSearch(
	w http.ResponseWriter, r *http.Request,
	q, mode string, explain bool, pageSize int,
) {
	if s.hybridEngine == nil {
		writeError(w, http.StatusServiceUnavailable, "vector_not_enabled",
			"vector search is not configured on this server")
		return
	}
	ctx := r.Context()
	start := time.Now()

	parsed := search.Parse(q)
	freeText := strings.Join(parsed.TextTerms, " ")
	// Vector/hybrid search requires text to embed; filter-only
	// queries have no query vector to rank by. Callers that want
	// pure structured filtering should use mode=fts instead of
	// falling through to a 500 from the engine's "empty query"
	// rejection.
	if freeText == "" {
		writeError(w, http.StatusBadRequest, "missing_free_text",
			"mode=vector|hybrid requires at least one free-text term; use mode=fts for filter-only queries")
		return
	}

	subjectTerms := make([]string, 0, len(parsed.TextTerms))
	for _, t := range parsed.TextTerms {
		subjectTerms = append(subjectTerms, strings.ToLower(t))
	}

	filter, err := s.hybridEngine.BuildFilter(ctx, parsed)
	if err != nil {
		s.logger.Error("build hybrid filter failed", "query", q, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "filter resolution failed")
		return
	}

	req := hybrid.SearchRequest{
		Mode:         hybrid.Mode(mode),
		FreeText:     freeText,
		Filter:       filter,
		Limit:        pageSize,
		SubjectTerms: subjectTerms,
		Explain:      explain,
	}

	hits, meta, err := s.hybridEngine.Search(ctx, req)
	if err != nil {
		switch {
		case errors.Is(err, vector.ErrNotEnabled):
			writeError(w, http.StatusServiceUnavailable, "vector_not_enabled",
				"vector search is not configured")
		case errors.Is(err, vector.ErrIndexStale):
			writeError(w, http.StatusServiceUnavailable, "index_stale",
				"the vector index does not match the configured model; run `msgvault build-embeddings --full-rebuild`")
		case errors.Is(err, vector.ErrIndexBuilding):
			writeError(w, http.StatusServiceUnavailable, "index_building",
				"the initial vector index is still being built")
		case errors.Is(err, vector.ErrEmbeddingTimeout):
			writeError(w, http.StatusServiceUnavailable, "embedding_timeout",
				"the embedding endpoint did not respond in time; retry, or raise [vector.embeddings].timeout")
		default:
			s.logger.Error("hybrid search failed", "query", q, "mode", mode, "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "search failed")
		}
		return
	}

	// Bulk-hydrate to avoid the per-hit GetMessage N+1: a single
	// summary lookup pulls the base fields + recipients + labels for
	// the whole hit set in 5 SQL round-trips, regardless of len(hits).
	// Body and attachments are intentionally skipped — the search
	// response only needs MessageSummary.
	hitIDs := make([]int64, len(hits))
	for i, h := range hits {
		hitIDs[i] = h.MessageID
	}
	summaries, err := s.store.GetMessagesSummariesByIDs(hitIDs)
	if err != nil {
		s.logger.Warn("hydrate hybrid hits failed", "ids", len(hitIDs), "error", err)
		summaries = nil
	}
	byID := make(map[int64]APIMessage, len(summaries))
	for _, m := range summaries {
		byID[m.ID] = m
	}
	items := make([]hybridSearchItem, 0, len(hits))
	for _, h := range hits {
		msg, ok := byID[h.MessageID]
		if !ok {
			// Hit referred to a row that disappeared between Search
			// and hydration (just-deleted, retired generation, etc.).
			// Drop it silently — same effect as the old per-hit
			// GetMessage returning nil.
			continue
		}
		item := hybridSearchItem{MessageSummary: toMessageSummary(msg)}
		if explain {
			sb := &scoreBreakdown{SubjectBoosted: h.SubjectBoosted}
			if !math.IsNaN(h.RRFScore) {
				v := h.RRFScore
				sb.RRF = &v
			}
			if !math.IsNaN(h.BM25Score) {
				v := h.BM25Score
				sb.BM25 = &v
			}
			if !math.IsNaN(h.VectorScore) {
				v := h.VectorScore
				sb.Vector = &v
			}
			item.Score = sb
		}
		items = append(items, item)
	}

	writeJSON(w, http.StatusOK, hybridSearchResponse{
		Query:         q,
		Mode:          mode,
		Returned:      len(items),
		PoolSaturated: meta.PoolSaturated,
		Generation: generationSummary{
			ID:          int64(meta.Generation.ID),
			Model:       meta.Generation.Model,
			Dimension:   meta.Generation.Dimension,
			Fingerprint: meta.Generation.Fingerprint,
			State:       string(meta.Generation.State),
		},
		TookMS:  time.Since(start).Milliseconds(),
		Results: items,
	})
}

// handleListAccounts returns all configured accounts.
func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	if s.scheduler == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler_unavailable", "Scheduler not available")
		return
	}

	s.cfgMu.RLock()
	cfgAccounts := make([]config.AccountSchedule, len(s.cfg.Accounts))
	copy(cfgAccounts, s.cfg.Accounts)
	s.cfgMu.RUnlock()

	// Build source ID lookup from the engine (database sources table).
	sourceIDs := make(map[string]int64)
	if s.engine != nil {
		if engineAccounts, err := s.engine.ListAccounts(r.Context()); err == nil {
			for _, ea := range engineAccounts {
				sourceIDs[ea.Identifier] = ea.ID
			}
		}
	}

	var accounts []AccountInfo

	// Get schedule info from config
	for _, acc := range cfgAccounts {
		info := AccountInfo{
			ID:       sourceIDs[acc.Email],
			Email:    acc.Email,
			Schedule: acc.Schedule,
			Enabled:  acc.Enabled,
		}

		// Add scheduler status
		for _, status := range s.scheduler.Status() {
			if status.Email == acc.Email {
				if !status.LastRun.IsZero() {
					info.LastSyncAt = status.LastRun.UTC().Format(time.RFC3339)
				}
				if !status.NextRun.IsZero() {
					info.NextSyncAt = status.NextRun.UTC().Format(time.RFC3339)
				}
				break
			}
		}

		accounts = append(accounts, info)
	}

	if accounts == nil {
		accounts = []AccountInfo{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": accounts,
	})
}

// handleTriggerSync manually triggers a sync for an account.
func (s *Server) handleTriggerSync(w http.ResponseWriter, r *http.Request) {
	if s.scheduler == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler_unavailable", "Scheduler not available")
		return
	}

	account := chi.URLParam(r, "account")
	if account == "" {
		writeError(w, http.StatusBadRequest, "missing_account", "Account email is required")
		return
	}

	if !s.scheduler.IsScheduled(account) {
		writeError(w, http.StatusNotFound, "not_found", "Account is not scheduled: "+account)
		return
	}

	err := s.scheduler.TriggerSync(account)
	if err != nil {
		s.logger.Error("failed to trigger sync", "account", account, "error", err)
		writeError(w, http.StatusConflict, "sync_error", err.Error())
		return
	}

	s.logger.Info("sync triggered via API", "account", account)
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "accepted",
		"message": "Sync started for " + account,
	})
}

// handleSchedulerStatus returns the scheduler status.
func (s *Server) handleSchedulerStatus(w http.ResponseWriter, r *http.Request) {
	if s.scheduler == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler_unavailable", "Scheduler not available")
		return
	}

	statuses := s.scheduler.Status()
	if statuses == nil {
		statuses = []AccountStatus{}
	}

	writeJSON(w, http.StatusOK, SchedulerStatusResponse{
		Running:  s.scheduler.IsRunning(),
		Accounts: statuses,
	})
}

// tokenFile represents the on-disk token format (matches oauth package).
type tokenFile struct {
	oauth2.Token

	Scopes   []string `json:"scopes,omitempty"`
	TenantID string   `json:"tenant_id,omitempty"`
	ClientID string   `json:"client_id,omitempty"`
}

// handleUploadToken accepts a token from a remote client and saves it.
// POST /api/v1/auth/token/{email}.
func (s *Server) handleUploadToken(w http.ResponseWriter, r *http.Request) {
	email := chi.URLParam(r, "email")
	if email == "" {
		writeError(w, http.StatusBadRequest, "missing_email", "Email address is required")
		return
	}

	// Validate email format (basic check)
	if !strings.Contains(email, "@") || !strings.Contains(email, ".") {
		writeError(w, http.StatusBadRequest, "invalid_email", "Invalid email format")
		return
	}

	// Read and validate token JSON
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_error", "Failed to read request body")
		return
	}

	var tf tokenFile
	if err := json.Unmarshal(body, &tf); err != nil {
		s.logger.Warn("invalid token JSON", "error", err)
		writeError(w, http.StatusBadRequest, "invalid_json", "Invalid token JSON format")
		return
	}

	// Validate token has required fields
	if tf.RefreshToken == "" {
		writeError(w, http.StatusBadRequest, "invalid_token", "Token must include refresh_token")
		return
	}

	// Get tokens directory from config
	tokensDir := s.cfg.TokensDir()

	// Create tokens directory if needed
	if err := fileutil.SecureMkdirAll(tokensDir, 0700); err != nil {
		s.logger.Error("failed to create tokens directory", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to create tokens directory")
		return
	}

	// Sanitize email for filename
	tokenPath := sanitizeTokenPath(tokensDir, email)

	// Marshal token back to JSON (normalized)
	data, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to serialize token")
		return
	}

	// Atomic write via temp file
	tmpFile, err := os.CreateTemp(tokensDir, ".token-*.tmp")
	if err != nil {
		s.logger.Error("failed to create temp file", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to save token")
		return
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to write token")
		return
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to close token file")
		return
	}
	if err := fileutil.SecureChmod(tmpPath, 0600); err != nil {
		_ = os.Remove(tmpPath)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to set token permissions")
		return
	}
	if err := os.Rename(tmpPath, tokenPath); err != nil {
		_ = os.Remove(tmpPath)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to save token")
		return
	}

	s.logger.Info("token uploaded via API", "email", email)
	writeJSON(w, http.StatusCreated, map[string]string{
		"status":  "created",
		"message": "Token saved for " + email,
	})
}

// sanitizeTokenPath returns a safe file path for the token.
func sanitizeTokenPath(tokensDir, email string) string {
	// Remove dangerous characters
	safe := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == '\x00' {
			return -1
		}
		return r
	}, email)

	// Build path and verify it's within tokensDir
	path := filepath.Join(tokensDir, safe+".json")
	cleanPath := filepath.Clean(path)
	cleanTokensDir := filepath.Clean(tokensDir)

	// If path escapes tokensDir, use hash-based fallback
	if !strings.HasPrefix(cleanPath, cleanTokensDir+string(os.PathSeparator)) {
		return filepath.Join(tokensDir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(email))))
	}

	return cleanPath
}

// AddAccountRequest represents a request to add an account to the config.
type AddAccountRequest struct {
	Email    string `json:"email"`
	Schedule string `json:"schedule"` // Cron expression, defaults to "0 2 * * *"
	Enabled  bool   `json:"enabled"`  // Defaults to true
}

// handleAddAccount adds an account to the config file.
// POST /api/v1/accounts.
func (s *Server) handleAddAccount(w http.ResponseWriter, r *http.Request) {
	var req AddAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.Warn("invalid account request JSON", "error", err)
		writeError(w, http.StatusBadRequest, "invalid_json", "Invalid request JSON format")
		return
	}

	// Validate email
	if req.Email == "" {
		writeError(w, http.StatusBadRequest, "missing_email", "Email is required")
		return
	}
	if !strings.Contains(req.Email, "@") || !strings.Contains(req.Email, ".") {
		writeError(w, http.StatusBadRequest, "invalid_email", "Invalid email format")
		return
	}

	// Set defaults
	if req.Schedule == "" {
		req.Schedule = "0 2 * * *" // Default: 2am daily
	}
	req.Enabled = true // Always enable — caller is export-token registering for sync

	// Validate cron expression before persisting
	if err := scheduler.ValidateCronExpr(req.Schedule); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_schedule", err.Error())
		return
	}

	s.cfgMu.Lock()

	// Check if account already exists
	for _, acc := range s.cfg.Accounts {
		if acc.Email == req.Email {
			s.cfgMu.Unlock()
			writeJSON(w, http.StatusOK, map[string]string{
				"status":  "exists",
				"message": "Account already configured for " + req.Email,
			})
			return
		}
	}

	// Add account to config
	newAccount := config.AccountSchedule{
		Email:    req.Email,
		Schedule: req.Schedule,
		Enabled:  req.Enabled,
	}
	s.cfg.Accounts = append(s.cfg.Accounts, newAccount)

	// Save config; rollback in-memory state on failure
	if err := s.cfg.Save(); err != nil {
		s.cfg.Accounts = s.cfg.Accounts[:len(s.cfg.Accounts)-1]
		s.cfgMu.Unlock()
		s.logger.Error("failed to save config", "error", err)
		writeError(w, http.StatusInternalServerError, "save_error", "Failed to save configuration")
		return
	}

	s.cfgMu.Unlock()

	// Register with live scheduler (best-effort — config is already saved)
	if s.scheduler != nil {
		if err := s.scheduler.AddAccount(req.Email, req.Schedule); err != nil {
			s.logger.Warn("account saved but scheduler registration failed",
				"email", req.Email, "error", err)
		}
	}

	s.logger.Info("account added via API", "email", req.Email, "schedule", req.Schedule)
	writeJSON(w, http.StatusCreated, map[string]string{
		"status":  "created",
		"message": "Account added for " + req.Email,
	})
}

// ============================================================================
// Raw SQL Query Endpoint
// ============================================================================

type queryRequest struct {
	SQL string `json:"sql"`
}

// handleQuery executes a raw SQL query against DuckDB views.
// POST /api/v1/query.
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	querier, ok := s.engine.(query.SQLQuerier)
	if !ok {
		writeError(w, http.StatusServiceUnavailable,
			"engine_unavailable",
			"SQL query requires DuckDB engine (analytics cache may not be built)")
		return
	}

	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "Invalid request body")
		return
	}
	if req.SQL == "" {
		writeError(w, http.StatusBadRequest, "missing_sql", "Field 'sql' is required")
		return
	}

	result, err := querier.QuerySQL(r.Context(), req.SQL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "query_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// ============================================================================
// TUI Aggregate Endpoints
// ============================================================================

// AggregateResponse represents aggregate query results.
type AggregateResponse struct {
	ViewType string             `json:"view_type"`
	Rows     []AggregateRowJSON `json:"rows"`
}

// AggregateRowJSON represents a single aggregate row in JSON format.
type AggregateRowJSON struct {
	Key             string `json:"key"`
	Count           int64  `json:"count"`
	TotalSize       int64  `json:"total_size"`
	AttachmentSize  int64  `json:"attachment_size"`
	AttachmentCount int64  `json:"attachment_count"`
	TotalUnique     int64  `json:"total_unique"`
}

// TotalStatsResponse represents detailed stats with filters.
type TotalStatsResponse struct {
	MessageCount    int64 `json:"message_count"`
	TotalSize       int64 `json:"total_size"`
	AttachmentCount int64 `json:"attachment_count"`
	AttachmentSize  int64 `json:"attachment_size"`
	LabelCount      int64 `json:"label_count"`
	AccountCount    int64 `json:"account_count"`
}

// SearchFastResponse represents fast search results with stats.
type SearchFastResponse struct {
	Query      string              `json:"query"`
	Messages   []MessageSummary    `json:"messages"`
	TotalCount int64               `json:"total_count"`
	Stats      *TotalStatsResponse `json:"stats,omitempty"`
}

// parseViewType parses a view type string into query.ViewType.
func parseViewType(s string) (query.ViewType, bool) {
	switch strings.ToLower(s) {
	case "senders":
		return query.ViewSenders, true
	case "sender_names":
		return query.ViewSenderNames, true
	case "recipients":
		return query.ViewRecipients, true
	case "recipient_names":
		return query.ViewRecipientNames, true
	case "domains":
		return query.ViewDomains, true
	case "labels":
		return query.ViewLabels, true
	case "time":
		return query.ViewTime, true
	default:
		return query.ViewSenders, false
	}
}

// viewTypeString converts a query.ViewType to its API string representation.
func viewTypeString(v query.ViewType) string {
	switch v {
	case query.ViewSenders:
		return "senders"
	case query.ViewSenderNames:
		return "sender_names"
	case query.ViewRecipients:
		return "recipients"
	case query.ViewRecipientNames:
		return "recipient_names"
	case query.ViewDomains:
		return "domains"
	case query.ViewLabels:
		return "labels"
	case query.ViewTime:
		return "time"
	default:
		return "unknown"
	}
}

// parseSortField parses a sort field string into query.SortField.
func parseSortField(s string) query.SortField {
	switch strings.ToLower(s) {
	case "count":
		return query.SortByCount
	case "size":
		return query.SortBySize
	case "attachment_size":
		return query.SortByAttachmentSize
	case "name":
		return query.SortByName
	default:
		return query.SortByCount
	}
}

// parseSortDirection parses a direction string into query.SortDirection.
func parseSortDirection(s string) query.SortDirection {
	if strings.ToLower(s) == "asc" {
		return query.SortAsc
	}
	return query.SortDesc
}

// parseTimeGranularity parses a granularity string into query.TimeGranularity.
func parseTimeGranularity(s string) query.TimeGranularity {
	switch strings.ToLower(s) {
	case "year":
		return query.TimeYear
	case "day":
		return query.TimeDay
	default:
		return query.TimeMonth
	}
}

// parseAggregateOptions extracts common aggregate options from query parameters.
func parseAggregateOptions(r *http.Request) query.AggregateOptions {
	opts := query.DefaultAggregateOptions()

	if v := r.URL.Query().Get("sort"); v != "" {
		opts.SortField = parseSortField(v)
	}
	if v := r.URL.Query().Get("direction"); v != "" {
		opts.SortDirection = parseSortDirection(v)
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err := strconv.Atoi(v); err == nil && limit > 0 {
			opts.Limit = limit
		}
	}
	if v := r.URL.Query().Get("time_granularity"); v != "" {
		opts.TimeGranularity = parseTimeGranularity(v)
	}
	if v := r.URL.Query().Get("source_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			opts.SourceID = &id
		}
	}
	if r.URL.Query().Get("attachments_only") == "true" {
		opts.WithAttachmentsOnly = true
	}
	if r.URL.Query().Get("hide_deleted") == "true" {
		opts.HideDeletedFromSource = true
	}
	if v := r.URL.Query().Get("search_query"); v != "" {
		opts.SearchQuery = v
	}
	if v := r.URL.Query().Get("after"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			u := t.UTC()
			opts.After = &u
		} else if t, err := time.Parse("2006-01-02", v); err == nil {
			u := t.UTC()
			opts.After = &u
		}
	}
	if v := r.URL.Query().Get("before"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			u := t.UTC()
			opts.Before = &u
		} else if t, err := time.Parse("2006-01-02", v); err == nil {
			u := t.UTC()
			opts.Before = &u
		}
	}

	return opts
}

// parseMessageFilter extracts filter parameters from query parameters.
func parseMessageFilter(r *http.Request) query.MessageFilter {
	var filter query.MessageFilter
	filter.Pagination.Limit = -1 // sentinel: "not provided"

	filter.Sender = r.URL.Query().Get("sender")
	filter.SenderName = r.URL.Query().Get("sender_name")
	filter.Recipient = r.URL.Query().Get("recipient")
	filter.RecipientName = r.URL.Query().Get("recipient_name")
	filter.Domain = r.URL.Query().Get("domain")
	filter.Label = r.URL.Query().Get("label")
	filter.MessageType = r.URL.Query().Get("message_type")

	if v := r.URL.Query().Get("time_period"); v != "" {
		filter.TimeRange.Period = v
	}
	if v := r.URL.Query().Get("time_granularity"); v != "" {
		filter.TimeRange.Granularity = parseTimeGranularity(v)
	}
	if v := r.URL.Query().Get("conversation_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			filter.ConversationID = &id
		}
	}
	if v := r.URL.Query().Get("source_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			filter.SourceID = &id
		}
	}
	if r.URL.Query().Get("attachments_only") == "true" {
		filter.WithAttachmentsOnly = true
	}
	if r.URL.Query().Get("hide_deleted") == "true" {
		filter.HideDeletedFromSource = true
	}

	// Date range filters (RFC3339 or 2006-01-02), normalized to UTC
	if v := r.URL.Query().Get("after"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			u := t.UTC()
			filter.After = &u
		} else if t, err := time.Parse("2006-01-02", v); err == nil {
			u := t.UTC()
			filter.After = &u
		}
	}
	if v := r.URL.Query().Get("before"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			u := t.UTC()
			filter.Before = &u
		} else if t, err := time.Parse("2006-01-02", v); err == nil {
			u := t.UTC()
			filter.Before = &u
		}
	}

	// EmptyValueTargets — comma-separated view type names
	if v := r.URL.Query().Get("empty_targets"); v != "" {
		for name := range strings.SplitSeq(v, ",") {
			if vt, ok := parseViewType(strings.TrimSpace(name)); ok {
				filter.SetEmptyTarget(vt)
			}
		}
	}

	// Pagination
	if v := r.URL.Query().Get("offset"); v != "" {
		if offset, err := strconv.Atoi(v); err == nil && offset >= 0 {
			filter.Pagination.Offset = offset
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err := strconv.Atoi(v); err == nil && limit >= 0 {
			filter.Pagination.Limit = limit
		}
	}
	// -1 sentinel means "not provided" — leave for callers that need
	// endpoint-specific defaults (fast search: 100, deep search: 100).
	// Endpoints that don't override should call applyDefaultLimit.

	// Sorting
	if v := r.URL.Query().Get("sort"); v != "" {
		switch strings.ToLower(v) {
		case "date":
			filter.Sorting.Field = query.MessageSortByDate
		case "size":
			filter.Sorting.Field = query.MessageSortBySize
		case "subject":
			filter.Sorting.Field = query.MessageSortBySubject
		}
	}
	if v := r.URL.Query().Get("direction"); v != "" {
		filter.Sorting.Direction = parseSortDirection(v)
	}

	return filter
}

// toAggregateRowJSON converts query.AggregateRow to JSON format.
func toAggregateRowJSON(row query.AggregateRow) AggregateRowJSON {
	return AggregateRowJSON{
		Key:             row.Key,
		Count:           row.Count,
		TotalSize:       row.TotalSize,
		AttachmentSize:  row.AttachmentSize,
		AttachmentCount: row.AttachmentCount,
		TotalUnique:     row.TotalUnique,
	}
}

// toTotalStatsResponse converts query.TotalStats to JSON format.
func toTotalStatsResponse(stats *query.TotalStats) *TotalStatsResponse {
	if stats == nil {
		return nil
	}
	return &TotalStatsResponse{
		MessageCount:    stats.MessageCount,
		TotalSize:       stats.TotalSize,
		AttachmentCount: stats.AttachmentCount,
		AttachmentSize:  stats.AttachmentSize,
		LabelCount:      stats.LabelCount,
		AccountCount:    stats.AccountCount,
	}
}

// toMessageSummaryFromQuery converts query.MessageSummary to API MessageSummary.
func toMessageSummaryFromQuery(m query.MessageSummary) MessageSummary {
	labels := m.Labels
	if labels == nil {
		labels = []string{}
	}
	from := m.FromEmail
	if from == "" && m.FromPhone != "" {
		from = m.FromPhone
	}
	switch {
	case m.FromName != "" && from != "":
		from = fmt.Sprintf("%s <%s>", m.FromName, from)
	case from == "" && m.FromName != "":
		from = m.FromName
	}
	return MessageSummary{
		ID:             m.ID,
		ConversationID: m.ConversationID,
		Subject:        m.Subject,
		MessageType:    m.MessageType,
		From:           from,
		To:             formatQueryAddresses(m.To),
		Cc:             formatQueryAddresses(m.Cc),
		Bcc:            formatQueryAddresses(m.Bcc),
		SentAt:         m.SentAt.UTC().Format(time.RFC3339),
		DeletedAt:      formatDeletedAt(m.DeletedAt),
		Snippet:        m.Snippet,
		Labels:         labels,
		HasAttach:      m.HasAttachments,
		SizeBytes:      m.SizeEstimate,
	}
}

func formatQueryAddresses(addrs []query.Address) []string {
	if addrs == nil {
		return []string{}
	}
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, formatQueryAddress(addr))
	}
	return out
}

func formatQueryAddress(addr query.Address) string {
	switch {
	case addr.Name != "" && addr.Email != "":
		return fmt.Sprintf("%s <%s>", addr.Name, addr.Email)
	case addr.Email != "":
		return addr.Email
	default:
		return addr.Name
	}
}

func formatDeletedAt(deletedAt *time.Time) string {
	if deletedAt == nil {
		return ""
	}
	return deletedAt.UTC().Format(time.RFC3339)
}

// handleAggregates returns aggregate data for a view type.
// GET /api/v1/aggregates?view_type=senders&sort=count&direction=desc&limit=100.
func (s *Server) handleAggregates(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "Query engine not available")
		return
	}

	viewTypeStr := r.URL.Query().Get("view_type")
	if viewTypeStr == "" {
		viewTypeStr = "senders" // Default
	}
	viewType, ok := parseViewType(viewTypeStr)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_view_type",
			"Invalid view_type. Must be one of: senders, sender_names, recipients, recipient_names, domains, labels, time")
		return
	}

	opts := parseAggregateOptions(r)

	rows, err := s.engine.Aggregate(r.Context(), viewType, opts)
	if err != nil {
		s.logger.Error("aggregate query failed", "view_type", viewTypeStr, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Aggregate query failed")
		return
	}

	jsonRows := make([]AggregateRowJSON, len(rows))
	for i, row := range rows {
		jsonRows[i] = toAggregateRowJSON(row)
	}

	writeJSON(w, http.StatusOK, AggregateResponse{
		ViewType: viewTypeString(viewType),
		Rows:     jsonRows,
	})
}

// handleSubAggregates returns sub-aggregate data after drill-down.
// GET /api/v1/aggregates/sub?view_type=labels&sender=foo@example.com.
func (s *Server) handleSubAggregates(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "Query engine not available")
		return
	}

	viewTypeStr := r.URL.Query().Get("view_type")
	if viewTypeStr == "" {
		writeError(w, http.StatusBadRequest, "missing_view_type", "view_type parameter is required")
		return
	}
	viewType, ok := parseViewType(viewTypeStr)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_view_type",
			"Invalid view_type. Must be one of: senders, sender_names, recipients, recipient_names, domains, labels, time")
		return
	}

	filter := parseMessageFilter(r)
	opts := parseAggregateOptions(r)

	rows, err := s.engine.SubAggregate(r.Context(), filter, viewType, opts)
	if err != nil {
		s.logger.Error("sub-aggregate query failed", "view_type", viewTypeStr, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Sub-aggregate query failed")
		return
	}

	jsonRows := make([]AggregateRowJSON, len(rows))
	for i, row := range rows {
		jsonRows[i] = toAggregateRowJSON(row)
	}

	writeJSON(w, http.StatusOK, AggregateResponse{
		ViewType: viewTypeString(viewType),
		Rows:     jsonRows,
	})
}

// handleFilteredMessages returns a filtered list of messages.
// GET /api/v1/messages/filter?sender=foo@example.com&offset=0&limit=500.
func (s *Server) handleFilteredMessages(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "Query engine not available")
		return
	}

	filter := parseMessageFilter(r)
	if filter.Pagination.Limit <= 0 {
		filter.Pagination.Limit = maxPageSize
	}
	// Thread fetches (conversation_id) are naturally bounded by thread
	// size, so skip the page-size cap to avoid silent truncation.
	if filter.ConversationID == nil &&
		filter.Pagination.Limit > maxPageSize {
		filter.Pagination.Limit = maxPageSize
	}

	// Fetch one extra row to determine has_more accurately.
	requestLimit := filter.Pagination.Limit
	filter.Pagination.Limit = requestLimit + 1

	messages, err := s.engine.ListMessages(r.Context(), filter)
	if err != nil {
		s.logger.Error("filtered messages query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Message query failed")
		return
	}

	hasMore := len(messages) > requestLimit
	if hasMore {
		messages = messages[:requestLimit]
	}

	summaries := make([]MessageSummary, len(messages))
	for i, m := range messages {
		summaries[i] = toMessageSummaryFromQuery(m)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"count":    len(summaries),
		"has_more": hasMore,
		"offset":   filter.Pagination.Offset,
		"limit":    requestLimit,
		"messages": summaries,
	})
}

// handleTotalStats returns detailed stats with optional filters.
// GET /api/v1/stats/total?source_id=1&attachments_only=true.
func (s *Server) handleTotalStats(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "Query engine not available")
		return
	}

	var opts query.StatsOptions

	if v := r.URL.Query().Get("source_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			opts.SourceID = &id
		}
	}
	if r.URL.Query().Get("attachments_only") == "true" {
		opts.WithAttachmentsOnly = true
	}
	if r.URL.Query().Get("hide_deleted") == "true" {
		opts.HideDeletedFromSource = true
	}
	if v := r.URL.Query().Get("search_query"); v != "" {
		opts.SearchQuery = v
	}
	if v := r.URL.Query().Get("group_by"); v != "" {
		if viewType, ok := parseViewType(v); ok {
			opts.GroupBy = viewType
		}
	}

	stats, err := s.engine.GetTotalStats(r.Context(), opts)
	if err != nil {
		s.logger.Error("total stats query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Stats query failed")
		return
	}

	writeJSON(w, http.StatusOK, toTotalStatsResponse(stats))
}

// handleFastSearch performs fast metadata search (subject, sender, recipient).
// GET /api/v1/search/fast?q=invoice&offset=0&limit=100.
func (s *Server) handleFastSearch(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "Query engine not available")
		return
	}

	queryStr := r.URL.Query().Get("q")
	if queryStr == "" {
		writeError(w, http.StatusBadRequest, "missing_query", "Query parameter 'q' is required")
		return
	}

	filter := parseMessageFilter(r)

	// Reject filter fields that the search engines cannot honor.
	// SenderName/RecipientName use display names that aren't indexed
	// for search, ConversationID scoping isn't implemented, and
	// EmptyValueTargets is an aggregate-only concept. MessageType is
	// not propagated by MergeFilterIntoQuery — accepting it here would
	// silently return unscoped results, so reject until the search
	// pipeline gains a message_type predicate.
	if filter.SenderName != "" || filter.RecipientName != "" ||
		filter.ConversationID != nil || filter.HasEmptyTargets() ||
		filter.MessageType != "" {
		writeError(w, http.StatusBadRequest, "unsupported_filter",
			"Fast search does not support sender_name, recipient_name, "+
				"conversation_id, empty_targets, or message_type filters")
		return
	}

	// Get view type for stats grouping (optional, defaults to senders)
	var statsGroupBy query.ViewType
	if v := r.URL.Query().Get("view_type"); v != "" {
		var ok bool
		statsGroupBy, ok = parseViewType(v)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_view_type",
				"Invalid view_type. Must be one of: senders, sender_names, recipients, recipient_names, domains, labels, time")
			return
		}
	}

	offset := filter.Pagination.Offset
	limit := filter.Pagination.Limit
	// Allow limit=0 for count-only requests (used by SearchFastCount).
	if limit < 0 {
		limit = 100
	} else if limit > maxPageSize {
		limit = maxPageSize
	}

	q := search.Parse(queryStr)

	result, err := s.engine.SearchFastWithStats(r.Context(), q, queryStr, filter, statsGroupBy, limit, offset)
	if err != nil {
		s.logger.Error("fast search failed", "query", queryStr, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Search failed")
		return
	}

	summaries := make([]MessageSummary, len(result.Messages))
	for i, m := range result.Messages {
		summaries[i] = toMessageSummaryFromQuery(m)
	}

	writeJSON(w, http.StatusOK, SearchFastResponse{
		Query:      queryStr,
		Messages:   summaries,
		TotalCount: result.TotalCount,
		Stats:      toTotalStatsResponse(result.Stats),
	})
}

// handleDeepSearch performs full-text body search via FTS5.
// GET /api/v1/search/deep?q=invoice&offset=0&limit=100&source_id=1&hide_deleted=true.
func (s *Server) handleDeepSearch(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "Query engine not available")
		return
	}

	queryStr := r.URL.Query().Get("q")
	if queryStr == "" {
		writeError(w, http.StatusBadRequest, "missing_query", "Query parameter 'q' is required")
		return
	}

	filter := parseMessageFilter(r)

	// Reject filter fields that MergeFilterIntoQuery cannot represent
	// in search.Query. Without this check the parameters parse
	// successfully but silently do nothing, letting deep search
	// escape the current drill-down scope. MessageType is one of these
	// silently-dropped fields.
	if filter.SenderName != "" || filter.RecipientName != "" ||
		filter.TimeRange.Period != "" || filter.ConversationID != nil ||
		filter.HasEmptyTargets() || filter.MessageType != "" {
		writeError(w, http.StatusBadRequest, "unsupported_filter",
			"Deep search does not support sender_name, recipient_name, "+
				"time_period, conversation_id, empty_targets, or "+
				"message_type filters")
		return
	}

	// Deep search uses its own pagination defaults (100 rows) rather
	// than parseMessageFilter's 500-row default for list endpoints.
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	q := search.Parse(queryStr)
	merged := query.MergeFilterIntoQuery(q, filter)

	// Fetch one extra row to determine has_more accurately.
	messages, err := s.engine.Search(r.Context(), merged, limit+1, offset)
	if err != nil {
		s.logger.Error("deep search failed", "query", queryStr, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Search failed")
		return
	}

	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}

	summaries := make([]MessageSummary, len(messages))
	for i, m := range messages {
		summaries[i] = toMessageSummaryFromQuery(m)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"query":    queryStr,
		"messages": summaries,
		"count":    len(summaries),
		"has_more": hasMore,
		"offset":   offset,
		"limit":    limit,
	})
}

// isEngineUnsupported reports whether err indicates the configured query
// engine cannot satisfy the requested operation. Postgres and remote engines
// both have methods that return sentinel errors instead of data; mapping
// those to a stable status code keeps the API honest about engine
// capabilities rather than emitting 500 for predictable misses.
func isEngineUnsupported(err error) bool {
	return errors.Is(err, query.ErrNotImplemented) || errors.Is(err, remote.ErrNotSupported)
}

// handleMessageInline serves a CID-referenced inline MIME part (e.g. an
// embedded image) from the raw message data. The CID is passed as a `cid`
// query parameter so values containing `/` (legal per RFC 5322) round-trip
// without ambiguity in the routing layer.
func (s *Server) handleMessageInline(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "Query engine not available")
		return
	}

	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "Message ID must be a number")
		return
	}

	cidParam := r.URL.Query().Get("cid")
	if cidParam == "" {
		writeError(w, http.StatusBadRequest, "missing_cid", "Missing 'cid' query parameter")
		return
	}

	raw, err := s.engine.GetMessageRaw(r.Context(), id)
	if err != nil {
		if isEngineUnsupported(err) {
			writeError(w, http.StatusNotImplemented, "not_supported", "Inline MIME parts are not available on this engine")
			return
		}
		s.logger.Error("failed to get raw MIME for inline part", "error", err, "id", id, "cid", cidParam)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load message")
		return
	}
	if raw == nil {
		writeError(w, http.StatusNotFound, "not_found", "Message raw data not found")
		return
	}

	parsed, err := mime.Parse(raw)
	if err != nil {
		s.logger.Error("failed to parse MIME for inline part", "error", err, "id", id, "cid", cidParam)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to parse message")
		return
	}

	for _, att := range parsed.Attachments {
		if !att.IsInline || att.ContentID != cidParam {
			continue
		}
		ct := strings.ToLower(strings.TrimSpace(att.ContentType))
		if !strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "image/svg") {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_type", "Inline content type not permitted")
			return
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Disposition", "inline")
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(att.Content)
		return
	}

	writeError(w, http.StatusNotFound, "not_found", "Inline part not found")
}
