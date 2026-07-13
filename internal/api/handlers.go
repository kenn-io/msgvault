package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	msgexport "go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/chunkmatch"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"golang.org/x/oauth2"
)

// maxPageSize is the hard upper bound for any paginated endpoint.
const maxPageSize = 500

const sourceStatusItemErrorLimit = 10

// StatsResponse represents the archive statistics.
//
// TotalMessages counts active messages only (present in the source account);
// it retains this pre-existing semantic for backward compatibility. The
// archive is the system of record and also retains messages deleted from the
// source, so the canonical archived total is ActiveMessages +
// SourceDeletedMessages. Clients should prefer the explicit fields when
// presenting a total.
type StatsResponse struct {
	TotalMessages         int64             `json:"total_messages"`
	ActiveMessages        int64             `json:"active_messages"`
	SourceDeletedMessages int64             `json:"source_deleted_messages"`
	TotalThreads          int64             `json:"total_threads"`
	TotalAccounts         int64             `json:"total_accounts"`
	TotalLabels           int64             `json:"total_labels"`
	TotalAttach           int64             `json:"total_attachments"`
	DatabaseSize          int64             `json:"database_size_bytes"`
	VectorSearch          *vector.StatsView `json:"vector_search,omitempty"`
	VectorStatus          string            `json:"vector_status,omitempty"`
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

// SourceStatusResponse represents source sync status for all matching sources.
type SourceStatusResponse struct {
	Sources []SourceStatus `json:"sources"`
}

// SourceStatus represents one source and its read-only sync status.
type SourceStatus struct {
	ID                 int64          `json:"id"`
	SourceType         string         `json:"source_type"`
	Identifier         string         `json:"identifier"`
	DisplayName        *string        `json:"display_name"`
	LastSyncAt         *string        `json:"last_sync_at"`
	UpdatedAt          string         `json:"updated_at"`
	ActiveSync         *SyncRunStatus `json:"active_sync"`
	LatestSync         *SyncRunStatus `json:"latest_sync"`
	LastSuccessfulSync *SyncRunStatus `json:"last_successful_sync"`
}

// SyncRunStatus represents the API-visible details for a sync run.
type SyncRunStatus struct {
	ID                int64               `json:"id"`
	SourceID          int64               `json:"source_id"`
	StartedAt         string              `json:"started_at"`
	CompletedAt       *string             `json:"completed_at"`
	Status            string              `json:"status"`
	MessagesProcessed int64               `json:"messages_processed"`
	MessagesAdded     int64               `json:"messages_added"`
	MessagesUpdated   int64               `json:"messages_updated"`
	ErrorsCount       int64               `json:"errors_count"`
	ErrorMessage      *string             `json:"error_message"`
	CursorBefore      *string             `json:"cursor_before"`
	CursorAfter       *string             `json:"cursor_after"`
	SkippedCount      int64               `json:"skipped_count,omitempty"`
	ItemErrors        []SyncRunItemStatus `json:"item_errors,omitempty"`
}

// SyncRunItemStatus represents one recent per-item sync error.
type SyncRunItemStatus struct {
	SourceMessageID string `json:"source_message_id"`
	Phase           string `json:"phase"`
	ErrorKind       string `json:"error_kind"`
	ErrorMessage    string `json:"error_message"`
	CreatedAt       string `json:"created_at"`
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

// VectorHealth reports the vector subsystem state in health responses so
// daemon status is visible while background init runs (or after it fails).
type VectorHealth struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// OperationHealth reports the archive operation currently holding the
// daemon's operation gate. Public health only reports Busy; authenticated
// health may include Label and StartedAt.
type OperationHealth struct {
	Busy      bool       `json:"busy"`
	Label     string     `json:"label,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
}

// Analytics engine modes reported by /health. The daemon chooses its engine
// once at startup, so this reflects what aggregate endpoints actually use
// for the daemon's lifetime — not what a fresh daemon would choose now.
// AnalyticsModeSQLFallback distinguishes live SQL forced by a missing or
// unusable cache from live SQL chosen deliberately (engine = "sql",
// PostgreSQL backends).
const (
	AnalyticsModeDuckDB      = "duckdb"
	AnalyticsModeSQL         = "sql"
	AnalyticsModeSQLFallback = "sql-fallback"
	AnalyticsModePostgres    = "postgres"
)

const (
	maxHybridMatches      = 5
	hybridMatchSnippetLen = 300
)

type HealthResponse struct {
	Status    string           `json:"status"`
	Vector    *VectorHealth    `json:"vector,omitempty"`
	Operation *OperationHealth `json:"operation,omitempty"`
	// AnalyticsEngine is the analytics mode the daemon selected at startup
	// (one of the AnalyticsMode constants). Empty when the server was built
	// without one (tests, embedded uses).
	AnalyticsEngine string `json:"analytics_engine,omitempty"`
}

type MessageListResponse struct {
	Total    int64            `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
	Messages []MessageSummary `json:"messages"`
}

type AccountListResponse struct {
	Accounts []AccountInfo `json:"accounts"`
}

type StatusMessageResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type FilteredMessagesResponse struct {
	Count    int              `json:"count"`
	HasMore  bool             `json:"has_more"`
	Offset   int              `json:"offset"`
	Limit    int              `json:"limit"`
	Messages []MessageSummary `json:"messages"`
}

type GmailIDsResponse struct {
	GmailIDs []string `json:"gmail_ids"`
}

type DeepSearchResponse struct {
	Query        string              `json:"query"`
	Scope        string              `json:"scope,omitempty"`
	Messages     []MessageSummary    `json:"messages"`
	BodyContexts []BodySearchContext `json:"body_contexts,omitempty"`
	Count        int                 `json:"count"`
	HasMore      bool                `json:"has_more"`
	Offset       int                 `json:"offset"`
	Limit        int                 `json:"limit"`
}

// BodySearchContext carries exact body-match excerpts separately from the
// stable MessageSummary schema used across existing client surfaces.
type BodySearchContext struct {
	MessageID       int64    `json:"message_id"`
	ContextSnippets []string `json:"context_snippets,omitempty"`
	// ContextSnippetsTruncated reports contexts omitted by response or work caps.
	ContextSnippetsTruncated bool `json:"context_snippets_truncated,omitempty"`
}

// MessageSummary represents a message in list responses.
type MessageSummary struct {
	ID              int64    `json:"id"`
	SourceMessageID string   `json:"source_message_id,omitempty"`
	ConversationID  int64    `json:"conversation_id,omitempty"`
	Subject         string   `json:"subject"`
	MessageType     string   `json:"message_type,omitempty"`
	From            string   `json:"from"`
	FromEmail       string   `json:"from_email,omitempty"`
	FromName        string   `json:"from_name,omitempty"`
	FromPhone       string   `json:"from_phone,omitempty"`
	To              []string `json:"to"`
	Cc              []string `json:"cc,omitempty"`
	Bcc             []string `json:"bcc,omitempty"`
	SentAt          string   `json:"sent_at"`
	DeletedAt       string   `json:"deleted_at,omitempty"`
	Snippet         string   `json:"snippet"`
	Labels          []string `json:"labels"`
	HasAttach       bool     `json:"has_attachments"`
	SizeBytes       int64    `json:"size_bytes"`
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
	ID          int64  `json:"id"`
	Filename    string `json:"filename"`
	MimeType    string `json:"mime_type"`
	Size        int64  `json:"size_bytes"`
	ContentHash string `json:"content_hash,omitempty"`
	URL         string `json:"url,omitempty"`
}

func attachmentInfoFromStore(att store.APIAttachment) AttachmentInfo {
	return AttachmentInfo{
		ID:          att.ID,
		Filename:    att.Filename,
		MimeType:    att.MimeType,
		Size:        att.Size,
		ContentHash: att.ContentHash,
		URL:         att.URL,
	}
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
	Query            string                  `json:"query"`
	Mode             string                  `json:"mode"`
	Returned         int                     `json:"returned"`
	PoolSaturated    bool                    `json:"pool_saturated"`
	HasMore          bool                    `json:"has_more"`
	Generation       hybridGenerationSummary `json:"generation"`
	TookMS           int64                   `json:"took_ms"`
	ScopeLabel       string                  `json:"scope_label,omitempty"`
	ScopeSourceCount int                     `json:"scope_source_count,omitempty"`
	Results          []hybridSearchItem      `json:"results"`
}

type similarSearchResponse struct {
	SeedMessageID int64                   `json:"seed_message_id"`
	Returned      int                     `json:"returned"`
	Generation    hybridGenerationSummary `json:"generation"`
	Messages      []MessageSummary        `json:"messages"`
}

// generationSummary describes the active vector-index generation used to
// answer a hybrid/vector query.
type hybridGenerationSummary struct {
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

	Score            *scoreBreakdown     `json:"score,omitempty"`
	Matches          []hybridSearchMatch `json:"matches,omitempty"`
	MatchesTruncated bool                `json:"matches_truncated,omitempty"`
}

type hybridSearchMatch struct {
	CharOffset *int    `json:"char_offset,omitempty"`
	Snippet    string  `json:"snippet"`
	Line       *int    `json:"line,omitempty"`
	Score      float64 `json:"score"`
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

// writeIfContextError converts a context deadline/cancellation into a
// structured 503 response and returns true. A request that overran its
// server-side query budget (see requestTimeoutForPath) or was abandoned by
// the client surfaces here as context.DeadlineExceeded/Canceled; without this
// mapping those bubble up as a misleading 400/500 with a raw driver message.
func (s *Server) writeIfContextError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusServiceUnavailable, "query_timeout",
			"the query exceeded the server time limit; narrow the query and retry")
		return true
	case errors.Is(err, context.Canceled):
		writeError(w, http.StatusServiceUnavailable, "query_canceled",
			"the query was canceled before it completed")
		return true
	default:
		return false
	}
}

func writeAPIHTTPError(w http.ResponseWriter, err *apiHTTPError) {
	resp := err.ErrorResponse
	writeError(w, err.GetStatus(), resp.Error, resp.Message)
}

func nullableTimePtr(value time.Time) *string {
	formatted := value.UTC().Format(time.RFC3339)
	return &formatted
}

// messageDetailFromQuery builds a MessageDetail response from a query-engine
// MessageDetail, formatting addresses the same way the store path does and
// emitting body_html alongside body when both are present.
func messageDetailFromQuery(qMsg *query.MessageDetail) MessageDetail {
	from := ""
	fromEmail := ""
	fromName := ""
	if len(qMsg.From) > 0 {
		from = formatQueryAddress(qMsg.From[0])
		fromEmail = qMsg.From[0].Email
		fromName = qMsg.From[0].Name
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
			ID:          att.ID,
			Filename:    att.Filename,
			MimeType:    att.MimeType,
			Size:        att.Size,
			ContentHash: att.ContentHash,
			URL:         att.URL,
		})
	}

	return MessageDetail{
		MessageSummary: MessageSummary{
			ID:              qMsg.ID,
			SourceMessageID: qMsg.SourceMessageID,
			ConversationID:  qMsg.ConversationID,
			Subject:         qMsg.Subject,
			MessageType:     qMsg.MessageType,
			From:            from,
			FromEmail:       fromEmail,
			FromName:        fromName,
			To:              toAddrs,
			Cc:              ccAddrs,
			Bcc:             bccAddrs,
			SentAt:          qMsg.SentAt.UTC().Format(time.RFC3339),
			DeletedAt:       formatDeletedAt(qMsg.DeletedAt),
			Snippet:         qMsg.Snippet,
			Labels:          labels,
			HasAttach:       qMsg.HasAttachments,
			SizeBytes:       qMsg.SizeEstimate,
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
		ID:              m.ID,
		SourceMessageID: m.SourceMessageID,
		ConversationID:  m.ConversationID,
		Subject:         m.Subject,
		MessageType:     m.MessageType,
		From:            m.From,
		FromEmail:       m.FromEmail,
		FromName:        m.FromName,
		FromPhone:       m.FromPhone,
		To:              to,
		Cc:              m.Cc,
		Bcc:             m.Bcc,
		SentAt:          m.SentAt.UTC().Format(time.RFC3339),
		DeletedAt:       formatDeletedAt(m.DeletedAt),
		Snippet:         m.Snippet,
		Labels:          labels,
		HasAttach:       m.HasAttachments,
		SizeBytes:       m.SizeEstimate,
	}
}

// handleStats returns archive statistics.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "Database not available")
		return
	}

	stats, err := s.getStats(r.Context())
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		s.logger.Error("failed to get stats", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve statistics")
		return
	}

	// Vector stats are best-effort: log errors but still include
	// whatever partial stats came back.
	_, backend, _ := s.vectorComponents()
	vs, vsErr := vector.CollectStats(r.Context(), backend)
	if vsErr != nil {
		s.logger.Warn("vector stats", "error", vsErr)
	}

	resp := statsResponseFromStore(stats)
	resp.VectorSearch = vs
	s.refreshVectorStatusIfStale(r.Context())
	if status, _ := s.VectorStatus(); status != VectorStatusDisabled {
		resp.VectorStatus = string(status)
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleListMessages returns a paginated list of messages.
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "Database not available")
		return
	}

	page, _, err := queryInt(r, "page")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if page < 1 {
		page = 1
	}
	pageSize, ok, err := queryInt(r, "page_size")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if !ok || pageSize < 1 {
		pageSize = 20
	} else if pageSize > 100 {
		pageSize = 100
	}

	offset := (page - 1) * pageSize

	messages, total, err := s.listMessages(r.Context(), offset, pageSize)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		s.logger.Error("failed to list messages", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve messages")
		return
	}

	summaries := make([]MessageSummary, len(messages))
	for i, m := range messages {
		summaries[i] = toMessageSummary(m)
	}

	writeJSON(w, http.StatusOK, MessageListResponse{
		Total:    total,
		Page:     page,
		PageSize: pageSize,
		Messages: summaries,
	})
}

// handleGetMessage returns a single message by ID.
// When the query engine is available, it returns separate body_html for rich
// rendering; otherwise it falls back to the store layer (plain Body only).
func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
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

	msg, err := s.getMessage(r.Context(), id)
	if errors.Is(err, store.ErrMessageNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Message not found")
		return
	}
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		s.logger.Error("failed to get message", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve message")
		return
	}

	detail := MessageDetail{
		MessageSummary: toMessageSummary(*msg),
		Body:           msg.Body,
	}

	attachments := make([]AttachmentInfo, 0, len(msg.Attachments))
	for _, att := range msg.Attachments {
		attachments = append(attachments, attachmentInfoFromStore(att))
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
	explain := false
	if rawExplain := r.URL.Query().Get("explain"); rawExplain != "" {
		var err error
		explain, err = strconv.ParseBool(rawExplain)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_explain",
				"Query parameter 'explain' must be a boolean")
			return
		}
	}
	parsedQuery := parseSearchQueryRequest(r, query)
	if err := parsedQuery.Err(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	parsedQuery.HideDeleted = true

	account := r.URL.Query().Get("account")
	collection := r.URL.Query().Get("collection")
	scope := cliScope{}
	if account != "" || collection != "" {
		cliStore, apiErr := s.cliStore()
		if apiErr != nil {
			writeAPIHTTPError(w, apiErr)
			return
		}
		var err error
		scope, err = resolveCLIStatsScope(cliStore, account, collection)
		if err != nil {
			writeAPIHTTPError(w, s.cliScopeError(err))
			return
		}
		sourceIDs := scope.sourceIDs()
		if len(sourceIDs) == 0 {
			writeError(w, http.StatusBadRequest, "empty_scope", cliEmptyScopeMessage(account, collection))
			return
		}
		parsedQuery.AccountIDs = append(parsedQuery.AccountIDs, sourceIDs...)
	}

	if mode == "vector" || mode == "hybrid" {
		page, _, err := queryInt(r, "page")
		if err != nil {
			s.rejectBadParam(w, err)
			return
		}
		if page > 1 {
			writeError(w, http.StatusBadRequest, "pagination_unsupported",
				"mode=vector|hybrid only supports page=1")
			return
		}
		offset, _, err := queryInt(r, "offset")
		if err != nil {
			s.rejectBadParam(w, err)
			return
		}
		if offset < 0 {
			s.rejectBadParam(w, newParamError("offset", "query parameter \"offset\" must be non-negative"))
			return
		}
		includeMatches, _, err := queryBool(r, "include_matches")
		if err != nil {
			s.rejectBadParam(w, err)
			return
		}
		minScore, _, err := queryFloat(r, "min_score")
		if err != nil {
			s.rejectBadParam(w, err)
			return
		}
		pageSize, ok, err := queryInt(r, "page_size")
		if err != nil {
			s.rejectBadParam(w, err)
			return
		}
		if !ok || pageSize < 1 {
			pageSize = 20
		}
		_, _, vectorCfg := s.vectorComponents()
		if maxPage := vectorCfg.Search.MaxPageSizeHybridClamp(); maxPage > 0 {
			if offset >= maxPage {
				writeError(w, http.StatusBadRequest, "pagination_limit",
					fmt.Sprintf("offset %d exceeds hybrid ranking window (max %d)", offset, maxPage))
				return
			}
			if offset+pageSize > maxPage {
				pageSize = maxPage - offset
			}
		}
		s.handleHybridSearch(w, r, query, parsedQuery, mode, explain, offset, pageSize, includeMatches, minScore, scope)
		return
	}

	if mode != "fts" {
		writeError(w, http.StatusBadRequest, "invalid_mode",
			fmt.Sprintf("mode must be one of fts|vector|hybrid, got %q", mode))
		return
	}

	page, _, err := queryInt(r, "page")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if page < 1 {
		page = 1
	}
	pageSize, ok, err := queryInt(r, "page_size")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if !ok || pageSize < 1 {
		pageSize = 20
	} else if pageSize > 100 {
		pageSize = 100
	}

	offset := (page - 1) * pageSize

	var (
		messages []store.APIMessage
		total    int64
	)
	useQuery := parsedQuery.HasOperators() || len(parsedQuery.AccountIDs) > 0
	if searcher, ok := s.store.(ctxMessageSearcher); ok {
		if useQuery {
			messages, total, err = searcher.SearchMessagesQueryContext(r.Context(), parsedQuery, offset, pageSize)
		} else {
			messages, total, err = searcher.SearchMessagesContext(r.Context(), query, offset, pageSize)
		}
	} else if useQuery {
		messages, total, err = s.store.SearchMessagesQuery(parsedQuery, offset, pageSize)
	} else {
		messages, total, err = s.store.SearchMessages(query, offset, pageSize)
	}
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
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

func parseSearchQueryRequest(r *http.Request, query string) *search.Query {
	parsed := search.Parse(query)
	for _, raw := range r.URL.Query()["message_type"] {
		for typ := range strings.SplitSeq(raw, ",") {
			typ = strings.TrimSpace(strings.ToLower(typ))
			if typ != "" {
				parsed.MessageTypes = append(parsed.MessageTypes, typ)
			}
		}
	}
	return parsed
}

// handleHybridSearch runs vector or hybrid search via the configured
// hybrid engine. Returns 503 when the engine is not configured or the
// index is stale/building; otherwise returns RRF-ranked hits hydrated
// through the message store.
func (s *Server) handleHybridSearch(
	w http.ResponseWriter, r *http.Request,
	q string, parsed *search.Query, mode string, explain bool,
	offset, pageSize int, includeMatches bool, minScore float64,
	scope cliScope,
) {
	hybridEngine, backend, vectorCfg := s.vectorComponents()
	if hybridEngine == nil {
		s.writeVectorUnavailable(w)
		return
	}
	ctx := r.Context()
	start := time.Now()

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

	filter, err := hybridEngine.BuildFilter(ctx, parsed)
	if err != nil {
		s.logger.Error("build hybrid filter failed", "query", q, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "filter resolution failed")
		return
	}

	fetchLimit := offset + pageSize + 1
	if maxPage := vectorCfg.Search.MaxPageSizeHybridClamp(); maxPage > 0 && fetchLimit > maxPage {
		fetchLimit = maxPage
	}
	req := hybrid.SearchRequest{
		Mode:         hybrid.Mode(mode),
		FreeText:     freeText,
		Filter:       filter,
		Limit:        fetchLimit,
		SubjectTerms: subjectTerms,
		Explain:      explain,
	}

	hits, meta, err := hybridEngine.Search(ctx, req)
	if err != nil {
		switch {
		case errors.Is(err, vector.ErrNotEnabled):
			writeError(w, http.StatusServiceUnavailable, "vector_not_enabled",
				"vector search is not configured")
		case errors.Is(err, vector.ErrIndexStale):
			writeError(w, http.StatusServiceUnavailable, "index_stale",
				"the vector index does not match the configured model; run `msgvault embeddings build --full-rebuild`")
		case errors.Is(err, vector.ErrIndexBuilding):
			writeError(w, http.StatusServiceUnavailable, "index_building",
				"the initial vector index is still being built")
		case errors.Is(err, vector.ErrEmbeddingTimeout):
			writeError(w, http.StatusServiceUnavailable, "embedding_timeout",
				"the embedding endpoint did not respond in time; retry, or raise [vector.embeddings].timeout")
		case errors.Is(err, vector.ErrIndexScopeMismatch):
			writeError(w, http.StatusBadRequest, "index_scope_mismatch", err.Error())
		default:
			s.logger.Error("hybrid search failed", "query", q, "mode", mode, "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "search failed")
		}
		return
	}

	pageStart := min(offset, len(hits))
	pageEnd := min(pageStart+pageSize, len(hits))
	hasMore := pageEnd < len(hits)
	pageHits := hits[pageStart:pageEnd]

	// Bulk-hydrate to avoid the per-hit GetMessage N+1: a single
	// summary lookup pulls the base fields + recipients + labels for
	// the whole hit set in 5 SQL round-trips, regardless of len(hits).
	// Body and attachments are skipped on the default summary-only path.
	// Opt-in match enrichment below fetches bodies only for the returned page.
	hitIDs := make([]int64, len(pageHits))
	for i, h := range pageHits {
		hitIDs[i] = h.MessageID
	}
	summaries, err := s.getMessagesSummariesByIDs(r.Context(), hitIDs)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		s.logger.Warn("hydrate hybrid hits failed", "ids", len(hitIDs), "error", err)
		summaries = nil
	}
	byID := make(map[int64]APIMessage, len(summaries))
	for _, m := range summaries {
		byID[m.ID] = m
	}
	items := make([]hybridSearchItem, 0, len(pageHits))
	for _, h := range pageHits {
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
	if includeMatches {
		s.enrichHybridMatches(ctx, backend, vectorCfg, meta.Generation.ID, meta.QueryVector, items, minScore)
	}

	writeJSON(w, http.StatusOK, hybridSearchResponse{
		Query:            q,
		Mode:             mode,
		Returned:         len(items),
		PoolSaturated:    meta.PoolSaturated,
		HasMore:          hasMore,
		ScopeLabel:       scope.displayName(),
		ScopeSourceCount: len(scope.sourceIDs()),
		Generation: hybridGenerationSummary{
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

func (s *Server) enrichHybridMatches(
	ctx context.Context,
	backend vector.Backend,
	cfg vector.Config,
	genID vector.GenerationID,
	queryVec []float32,
	items []hybridSearchItem,
	minScore float64,
) {
	scorer, ok := backend.(vector.ChunkScoringBackend)
	if !ok || len(queryVec) == 0 {
		return
	}
	for i := range items {
		msg, err := s.getMessage(ctx, items[i].ID)
		if err != nil || msg == nil {
			s.logger.Warn("hydrate hybrid match body failed", "message_id", items[i].ID, "error", err)
			continue
		}
		hits, err := scorer.ScoreMessageChunks(ctx, genID, msg.ID, queryVec)
		if err != nil {
			s.logger.Warn("score hybrid match chunks failed", "message_id", items[i].ID, "error", err)
			continue
		}
		matches, truncated := chunkmatch.Build(
			msg.Subject, embeddingBodyText(msg), cfg, hits, minScore,
			maxHybridMatches, hybridMatchSnippetLen,
		)
		items[i].Matches = make([]hybridSearchMatch, len(matches))
		for j, match := range matches {
			items[i].Matches[j] = hybridSearchMatch{
				CharOffset: match.CharOffset,
				Snippet:    match.Snippet,
				Line:       match.Line,
				Score:      match.Score,
			}
		}
		items[i].MatchesTruncated = truncated
	}
}

func embeddingBodyText(msg *APIMessage) string {
	body := embed.BodyTextForEmbedding(msg.BodyText, msg.BodyHTML)
	if body == "" {
		// Preserve compatibility with MessageStore implementations that only
		// populate the legacy selected Body field.
		return msg.Body
	}
	return body
}

func (s *Server) handleSimilarSearch(w http.ResponseWriter, r *http.Request) {
	_, backend, vectorCfg := s.vectorComponents()
	if backend == nil {
		s.writeVectorUnavailable(w)
		return
	}
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "Database not available")
		return
	}

	seedID, err := parseRequiredInt64Query(r, "message_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_message_id", err.Error())
		return
	}

	limit, _, err := queryInt(r, "limit")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if limit < 1 {
		limit = 20
	}
	if maxPage := vectorCfg.Search.MaxPageSizeHybridClamp(); maxPage > 0 && limit > maxPage {
		limit = maxPage
	}

	filter, apiErr := s.similarSearchFilter(r)
	if apiErr != nil {
		writeAPIHTTPError(w, apiErr)
		return
	}

	ctx := r.Context()
	active, err := vector.ResolveActiveForFingerprint(ctx, backend, vectorCfg.GenerationFingerprint())
	if err != nil {
		s.writeVectorSearchError(w, err, "active generation")
		return
	}
	if err := hybrid.ValidateBuildScope(vectorCfg.Embed.Scope.BuildScope(), filter); err != nil {
		s.writeVectorSearchError(w, err, "scope validation")
		return
	}

	seed, err := backend.LoadVector(ctx, seedID)
	if err != nil {
		s.writeVectorSearchError(w, err, "load seed vector")
		return
	}

	hits, err := backend.Search(ctx, active.ID, seed, limit+1, filter)
	if err != nil {
		s.writeVectorSearchError(w, err, "similar search")
		return
	}

	wantIDs := make([]int64, 0, limit)
	for _, hit := range hits {
		if hit.MessageID == seedID {
			continue
		}
		if len(wantIDs) >= limit {
			break
		}
		wantIDs = append(wantIDs, hit.MessageID)
	}

	summaries, err := s.getMessagesSummariesByIDs(r.Context(), wantIDs)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		s.logger.Warn("hydrate similar hits failed", "ids", len(wantIDs), "error", err)
		summaries = nil
	}
	byID := make(map[int64]APIMessage, len(summaries))
	for _, msg := range summaries {
		byID[msg.ID] = msg
	}
	messages := make([]MessageSummary, 0, len(wantIDs))
	for _, id := range wantIDs {
		msg, ok := byID[id]
		if !ok {
			continue
		}
		messages = append(messages, toMessageSummary(msg))
	}

	writeJSON(w, http.StatusOK, similarSearchResponse{
		SeedMessageID: seedID,
		Returned:      len(messages),
		Generation: hybridGenerationSummary{
			ID:          int64(active.ID),
			Model:       active.Model,
			Dimension:   active.Dimension,
			Fingerprint: active.Fingerprint,
			State:       string(active.State),
		},
		Messages: messages,
	})
}

func parseRequiredInt64Query(r *http.Request, name string) (int64, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return 0, fmt.Errorf("%s is required", name)
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return id, nil
}

func (s *Server) similarSearchFilter(r *http.Request) (vector.Filter, *apiHTTPError) {
	var filter vector.Filter
	if account := r.URL.Query().Get("account"); account != "" {
		cliStore, apiErr := s.cliStore()
		if apiErr != nil {
			return filter, apiErr
		}
		scope, err := resolveCLIStatsScope(cliStore, account, "")
		if err != nil {
			return filter, s.cliScopeError(err)
		}
		filter.SourceIDs = scope.sourceIDs()
	}
	if messageType := strings.TrimSpace(r.URL.Query().Get("message_type")); messageType != "" {
		filter.MessageTypes = []string{strings.ToLower(messageType)}
	}
	if after, ok, err := queryDate(r, "after"); err != nil {
		return filter, apiHTTPErrorFromParam(err)
	} else if ok {
		filter.After = &after
	}
	if before, ok, err := queryDate(r, "before"); err != nil {
		return filter, apiHTTPErrorFromParam(err)
	} else if ok {
		filter.Before = &before
	}
	if hasAttachment, ok, err := queryBool(r, "has_attachment"); err != nil {
		return filter, apiHTTPErrorFromParam(err)
	} else if ok && hasAttachment {
		filter.HasAttachment = &hasAttachment
	}
	return filter, nil
}

func parseAPITime(value string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse API time %q: %w", value, err)
	}
	return t.UTC(), nil
}

func (s *Server) writeVectorSearchError(w http.ResponseWriter, err error, operation string) {
	switch {
	case errors.Is(err, vector.ErrNotEnabled):
		writeError(w, http.StatusServiceUnavailable, "vector_not_enabled",
			"vector search is not configured")
	case errors.Is(err, vector.ErrIndexStale):
		writeError(w, http.StatusServiceUnavailable, "index_stale",
			"the vector index does not match the configured model; run `msgvault embeddings build --full-rebuild`")
	case errors.Is(err, vector.ErrIndexBuilding):
		writeError(w, http.StatusServiceUnavailable, "index_building",
			"the initial vector index is still being built")
	case errors.Is(err, vector.ErrEmbeddingTimeout):
		writeError(w, http.StatusServiceUnavailable, "embedding_timeout",
			"the embedding endpoint did not respond in time; retry, or raise [vector.embeddings].timeout")
	case errors.Is(err, vector.ErrIndexScopeMismatch):
		writeError(w, http.StatusBadRequest, "index_scope_mismatch", err.Error())
	default:
		s.logger.Error("vector search failed", "operation", operation, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", operation+" failed")
	}
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

	writeJSON(w, http.StatusOK, AccountListResponse{Accounts: accounts})
}

// handleSourceStatus returns read-only sync status for all matching sources.
func (s *Server) handleSourceStatus(w http.ResponseWriter, r *http.Request) {
	statusStore, ok := s.store.(SourceStatusStore)
	if s.store == nil || !ok {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "Database not available")
		return
	}

	sourceType := r.URL.Query().Get("source_type")
	sources, err := statusStore.ListSources(sourceType)
	if err != nil {
		s.logger.Error("failed to list sources for status",
			"source_type", sourceType,
			"error", err,
		)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve source status")
		return
	}

	statuses := make([]SourceStatus, 0, len(sources))
	for _, source := range sources {
		status, err := s.sourceStatus(statusStore, source)
		if err != nil {
			s.logger.Error("failed to build source sync status",
				"source_id", source.ID,
				"source_type", source.SourceType,
				"identifier", source.Identifier,
				"error", err,
			)
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve source status")
			return
		}
		statuses = append(statuses, status)
	}

	writeJSON(w, http.StatusOK, SourceStatusResponse{Sources: statuses})
}

func (s *Server) sourceStatus(statusStore SourceStatusStore, source *store.Source) (SourceStatus, error) {
	status := SourceStatus{
		ID:         source.ID,
		SourceType: source.SourceType,
		Identifier: source.Identifier,
		UpdatedAt:  source.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if source.DisplayName.Valid {
		status.DisplayName = new(source.DisplayName.String)
	}
	if source.LastSyncAt.Valid {
		status.LastSyncAt = nullableTimePtr(source.LastSyncAt.Time)
	}

	active, err := statusStore.GetActiveSync(source.ID)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return SourceStatus{}, fmt.Errorf("get active sync: %w", err)
	}
	status.ActiveSync = syncRunStatus(active)
	if err := s.hydrateSyncRunStatus(statusStore, status.ActiveSync); err != nil {
		return SourceStatus{}, err
	}

	latest, err := statusStore.GetLatestSync(source.ID)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return SourceStatus{}, fmt.Errorf("get latest sync: %w", err)
	}
	status.LatestSync = syncRunStatus(latest)
	if err := s.hydrateSyncRunStatus(statusStore, status.LatestSync); err != nil {
		return SourceStatus{}, err
	}

	lastSuccessful, err := statusStore.GetLastSuccessfulSync(source.ID)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return SourceStatus{}, fmt.Errorf("get last successful sync: %w", err)
	}
	status.LastSuccessfulSync = syncRunStatus(lastSuccessful)
	if err := s.hydrateSyncRunStatus(statusStore, status.LastSuccessfulSync); err != nil {
		return SourceStatus{}, err
	}

	return status, nil
}

func (s *Server) hydrateSyncRunStatus(statusStore SourceStatusStore, status *SyncRunStatus) error {
	if status == nil {
		return nil
	}

	skippedCount, err := statusStore.CountSyncRunItems(status.ID, store.SyncRunItemStatusSkipped)
	if err != nil {
		return fmt.Errorf("count skipped sync items: %w", err)
	}
	status.SkippedCount = skippedCount

	items, err := statusStore.ListSyncRunItems(status.ID, store.SyncRunItemStatusError, sourceStatusItemErrorLimit)
	if err != nil {
		return fmt.Errorf("list sync item errors: %w", err)
	}
	status.ItemErrors = syncRunItemStatuses(items)
	return nil
}

func syncRunStatus(run *store.SyncRun) *SyncRunStatus {
	if run == nil {
		return nil
	}

	status := &SyncRunStatus{
		ID:                run.ID,
		SourceID:          run.SourceID,
		StartedAt:         run.StartedAt.UTC().Format(time.RFC3339),
		Status:            run.Status,
		MessagesProcessed: run.MessagesProcessed,
		MessagesAdded:     run.MessagesAdded,
		MessagesUpdated:   run.MessagesUpdated,
		ErrorsCount:       run.ErrorsCount,
	}
	if run.CompletedAt.Valid {
		status.CompletedAt = nullableTimePtr(run.CompletedAt.Time)
	}
	if run.ErrorMessage.Valid {
		status.ErrorMessage = new(run.ErrorMessage.String)
	}
	if run.CursorBefore.Valid {
		status.CursorBefore = new(run.CursorBefore.String)
	}
	if run.CursorAfter.Valid {
		status.CursorAfter = new(run.CursorAfter.String)
	}
	return status
}

func syncRunItemStatuses(items []store.SyncRunItem) []SyncRunItemStatus {
	if len(items) == 0 {
		return nil
	}
	out := make([]SyncRunItemStatus, len(items))
	for i, item := range items {
		out[i] = SyncRunItemStatus{
			SourceMessageID: item.SourceMessageID,
			Phase:           item.Phase,
			ErrorKind:       item.ErrorKind,
			ErrorMessage:    item.ErrorMessage,
			CreatedAt:       item.CreatedAt.UTC().Format(time.RFC3339),
		}
	}
	return out
}

// handleTriggerSync manually triggers a sync for an account.
func (s *Server) handleTriggerSync(w http.ResponseWriter, r *http.Request) {
	if s.scheduler == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler_unavailable", "Scheduler not available")
		return
	}

	account := r.PathValue("account")
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
	writeJSON(w, http.StatusAccepted, StatusMessageResponse{
		Status:  "accepted",
		Message: "Sync started for " + account,
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

type TokenUploadRequest struct {
	AccessToken  string    `json:"access_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry,omitzero"`
	Scopes       []string  `json:"scopes,omitempty"`
	TenantID     string    `json:"tenant_id,omitempty"`
	ClientID     string    `json:"client_id,omitempty"`
}

// handleUploadToken accepts a token from a remote client and saves it.
// POST /api/v1/auth/token/{email}.
func (s *Server) handleUploadToken(w http.ResponseWriter, r *http.Request) {
	email := r.PathValue("email")
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
	writeJSON(w, http.StatusCreated, StatusMessageResponse{
		Status:  "created",
		Message: "Token saved for " + email,
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
			writeJSON(w, http.StatusOK, StatusMessageResponse{
				Status:  "exists",
				Message: "Account already configured for " + req.Email,
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
	writeJSON(w, http.StatusCreated, StatusMessageResponse{
		Status:  "created",
		Message: "Account added for " + req.Email,
	})
}

// ============================================================================
// Raw SQL Query Endpoint
// ============================================================================

type QueryRequest struct {
	SQL string `json:"sql"`
}

var errSQLQueryEngineUnavailable = errors.New("SQL query requires DuckDB engine (analytics cache may not be built)")

// handleQuery executes a raw SQL query against DuckDB views.
// POST /api/v1/query.
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "Invalid request body")
		return
	}
	if req.SQL == "" {
		writeError(w, http.StatusBadRequest, "missing_sql", "Field 'sql' is required")
		return
	}
	// Reject writes and multi-statement input before execution. The engine
	// enforces this too (for in-process callers), but checking here guarantees
	// the endpoint contract and returns a clear 400 without touching the query
	// runner.
	if err := query.EnsureReadOnly(req.SQL); err != nil {
		writeError(w, http.StatusBadRequest, "not_read_only", err.Error())
		return
	}

	result, err := s.runSQLQuery(r.Context(), req.SQL)
	if err != nil {
		if errors.Is(err, errSQLQueryEngineUnavailable) {
			writeError(w, http.StatusServiceUnavailable,
				"engine_unavailable",
				errSQLQueryEngineUnavailable.Error())
			return
		}
		if s.writeIfContextError(w, err) {
			return
		}
		if errors.Is(err, query.ErrQueryNotReadOnly) {
			writeError(w, http.StatusBadRequest, "not_read_only", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "query_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) runSQLQuery(ctx context.Context, sql string) (*query.QueryResult, error) {
	if s.sqlQueryRunner != nil {
		return s.sqlQueryRunner(ctx, sql)
	}
	querier, ok := s.engine.(query.SQLQuerier)
	if !ok {
		return nil, errSQLQueryEngineUnavailable
	}
	return querier.QuerySQL(ctx, sql)
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
//
// MessageCount is the total over the filtered population; unless the request
// sets hide_deleted=true it includes messages deleted from their source
// account (the archive retains them). ActiveMessages and
// SourceDeletedMessages break that total into its two populations so a client
// can label it rather than guess which semantic the number carries.
type TotalStatsResponse struct {
	MessageCount          int64 `json:"message_count"`
	ActiveMessages        int64 `json:"active_messages"`
	SourceDeletedMessages int64 `json:"source_deleted_messages"`
	TotalSize             int64 `json:"total_size"`
	AttachmentCount       int64 `json:"attachment_count"`
	AttachmentSize        int64 `json:"attachment_size"`
	LabelCount            int64 `json:"label_count"`
	AccountCount          int64 `json:"account_count"`
}

// SearchFastResponse represents fast search results with stats.
type SearchFastResponse struct {
	Query      string              `json:"query"`
	Messages   []MessageSummary    `json:"messages"`
	TotalCount int64               `json:"total_count"`
	Stats      *TotalStatsResponse `json:"stats,omitempty"`
}

type TextConversationRow struct {
	ConversationID   int64  `json:"conversation_id"`
	Title            string `json:"title"`
	SourceType       string `json:"source_type"`
	MessageCount     int64  `json:"message_count"`
	ParticipantCount int64  `json:"participant_count"`
	LastMessageAt    string `json:"last_message_at,omitempty"`
	LastPreview      string `json:"last_preview"`
}

type TextConversationsResponse struct {
	Count         int                   `json:"count"`
	HasMore       bool                  `json:"has_more"`
	Offset        int                   `json:"offset"`
	Limit         int                   `json:"limit"`
	Conversations []TextConversationRow `json:"conversations"`
}

type TextMessagesResponse struct {
	Count    int                    `json:"count"`
	HasMore  bool                   `json:"has_more"`
	Offset   int                    `json:"offset"`
	Limit    int                    `json:"limit"`
	Messages []query.MessageSummary `json:"messages"`
}

// aggregateViewTypes are the accepted view_type values, surfaced in 400 messages.
var aggregateViewTypes = []string{
	"senders", "sender_names", "recipients", "recipient_names",
	"domains", "labels", "time",
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

// Accepted values for enum query parameters, surfaced in 400 messages.
var (
	aggregateSortFields = []string{"count", "size", "attachment_size", "name"}
	messageSortFields   = []string{"date", "size", "subject"}
	textSortFields      = []string{"last_message", "count", "name"}
	sortDirections      = []string{"asc", "desc"}
	timeGranularities   = []string{"year", "month", "day"}
)

// parseSortField parses an aggregate sort field. ok is false for unknown values.
func parseSortField(s string) (query.SortField, bool) {
	switch strings.ToLower(s) {
	case "count":
		return query.SortByCount, true
	case "size":
		return query.SortBySize, true
	case "attachment_size":
		return query.SortByAttachmentSize, true
	case "name":
		return query.SortByName, true
	default:
		return query.SortByCount, false
	}
}

// parseSortDirection parses a direction string. ok is false for unknown values.
func parseSortDirection(s string) (query.SortDirection, bool) {
	switch strings.ToLower(s) {
	case "asc":
		return query.SortAsc, true
	case "desc":
		return query.SortDesc, true
	default:
		return query.SortDesc, false
	}
}

// parseTimeGranularity parses a granularity string. ok is false for unknown values.
func parseTimeGranularity(s string) (query.TimeGranularity, bool) {
	switch strings.ToLower(s) {
	case "year":
		return query.TimeYear, true
	case "month":
		return query.TimeMonth, true
	case "day":
		return query.TimeDay, true
	default:
		return query.TimeMonth, false
	}
}

func parseTextViewType(s string) (query.TextViewType, bool) {
	switch strings.ToLower(s) {
	case "conversations":
		return query.TextViewConversations, true
	case "contacts":
		return query.TextViewContacts, true
	case "contact_names":
		return query.TextViewContactNames, true
	case "sources":
		return query.TextViewSources, true
	case "labels":
		return query.TextViewLabels, true
	case "time":
		return query.TextViewTime, true
	default:
		return query.TextViewContacts, false
	}
}

func textViewTypeString(v query.TextViewType) string {
	switch v {
	case query.TextViewConversations:
		return "conversations"
	case query.TextViewContacts:
		return "contacts"
	case query.TextViewContactNames:
		return "contact_names"
	case query.TextViewSources:
		return "sources"
	case query.TextViewLabels:
		return "labels"
	case query.TextViewTime:
		return "time"
	default:
		return "unknown"
	}
}

func parseTextSortField(s string) (query.TextSortField, bool) {
	switch strings.ToLower(s) {
	case "last_message":
		return query.TextSortByLastMessage, true
	case "count":
		return query.TextSortByCount, true
	case "name":
		return query.TextSortByName, true
	default:
		return query.TextSortByLastMessage, false
	}
}

// parseAggregateOptions extracts common aggregate options from query parameters.
// Unparseable integers/dates and unknown enum values return a paramError so the
// handler can reject them with a 400; out-of-range limits are clamped.
func parseAggregateOptions(r *http.Request) (query.AggregateOptions, error) {
	opts := query.DefaultAggregateOptions()

	if v := r.URL.Query().Get("sort"); v != "" {
		field, ok := parseSortField(v)
		if !ok {
			return opts, enumParamError("sort", v, aggregateSortFields)
		}
		opts.SortField = field
	}
	if v := r.URL.Query().Get("direction"); v != "" {
		dir, ok := parseSortDirection(v)
		if !ok {
			return opts, enumParamError("direction", v, sortDirections)
		}
		opts.SortDirection = dir
	}
	limit, ok, err := queryInt(r, "limit")
	if err != nil {
		return opts, err
	}
	if ok && limit > 0 {
		opts.Limit = limit
	}
	if v := r.URL.Query().Get("time_granularity"); v != "" {
		gran, ok := parseTimeGranularity(v)
		if !ok {
			return opts, enumParamError("time_granularity", v, timeGranularities)
		}
		opts.TimeGranularity = gran
	}
	if sourceID, ok, err := queryInt64(r, "source_id"); err != nil {
		return opts, err
	} else if ok {
		opts.SourceID = &sourceID
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
	if after, ok, err := queryDate(r, "after"); err != nil {
		return opts, err
	} else if ok {
		opts.After = &after
	}
	if before, ok, err := queryDate(r, "before"); err != nil {
		return opts, err
	} else if ok {
		opts.Before = &before
	}

	return opts, nil
}

// parseMessageFilter extracts filter parameters from query parameters.
// Unparseable integers/dates and unknown enum values return a paramError so the
// handler can reject them with a 400 instead of silently ignoring the filter.
// requestWithoutParams returns a shallow copy of r whose URL query has the
// named parameters removed. The original request is left untouched. Used to
// hand a filter parser a request view that excludes params owned by another
// parser on the same endpoint.
func requestWithoutParams(r *http.Request, keys ...string) *http.Request {
	q := r.URL.Query()
	for _, k := range keys {
		q.Del(k)
	}
	clone := *r
	u := *r.URL
	u.RawQuery = q.Encode()
	clone.URL = &u
	return &clone
}

func parseMessageFilter(r *http.Request) (query.MessageFilter, error) {
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
		gran, ok := parseTimeGranularity(v)
		if !ok {
			return filter, enumParamError("time_granularity", v, timeGranularities)
		}
		filter.TimeRange.Granularity = gran
	}
	if id, ok, err := queryInt64(r, "conversation_id"); err != nil {
		return filter, err
	} else if ok {
		filter.ConversationID = &id
	}
	if id, ok, err := queryInt64(r, "source_id"); err != nil {
		return filter, err
	} else if ok {
		filter.SourceID = &id
	}
	if r.URL.Query().Get("attachments_only") == "true" {
		filter.WithAttachmentsOnly = true
	}
	if r.URL.Query().Get("hide_deleted") == "true" {
		filter.HideDeletedFromSource = true
	}

	if after, ok, err := queryDate(r, "after"); err != nil {
		return filter, err
	} else if ok {
		filter.After = &after
	}
	if before, ok, err := queryDate(r, "before"); err != nil {
		return filter, err
	} else if ok {
		filter.Before = &before
	}

	// EmptyValueTargets — comma-separated view type names
	if v := r.URL.Query().Get("empty_targets"); v != "" {
		for name := range strings.SplitSeq(v, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			vt, ok := parseViewType(name)
			if !ok {
				return filter, enumParamError("empty_targets", name, aggregateViewTypes)
			}
			filter.SetEmptyTarget(vt)
		}
	}

	// Pagination. A non-numeric value is rejected; a negative offset/limit is
	// clamped (limit=0 has a count-only meaning on some endpoints). The -1
	// limit sentinel means "not provided" — callers apply their own default.
	if offset, ok, err := queryInt(r, "offset"); err != nil {
		return filter, err
	} else if ok && offset >= 0 {
		filter.Pagination.Offset = offset
	}
	if limit, ok, err := queryInt(r, "limit"); err != nil {
		return filter, err
	} else if ok && limit >= 0 {
		filter.Pagination.Limit = limit
	}

	// Sorting
	if v := r.URL.Query().Get("sort"); v != "" {
		switch strings.ToLower(v) {
		case "date":
			filter.Sorting.Field = query.MessageSortByDate
		case "size":
			filter.Sorting.Field = query.MessageSortBySize
		case "subject":
			filter.Sorting.Field = query.MessageSortBySubject
		default:
			return filter, enumParamError("sort", v, messageSortFields)
		}
	}
	if v := r.URL.Query().Get("direction"); v != "" {
		dir, ok := parseSortDirection(v)
		if !ok {
			return filter, enumParamError("direction", v, sortDirections)
		}
		filter.Sorting.Direction = dir
	}

	return filter, nil
}

func parseTextFilter(r *http.Request) (query.TextFilter, error) {
	var filter query.TextFilter
	filter.ContactPhone = r.URL.Query().Get("contact_phone")
	filter.ContactName = r.URL.Query().Get("contact_name")
	filter.SourceType = r.URL.Query().Get("source_type")
	filter.Label = r.URL.Query().Get("label")

	if id, ok, err := queryInt64(r, "source_id"); err != nil {
		return filter, err
	} else if ok {
		filter.SourceID = &id
	}
	if v := r.URL.Query().Get("time_period"); v != "" {
		filter.TimeRange.Period = v
	}
	if v := r.URL.Query().Get("time_granularity"); v != "" {
		gran, ok := parseTimeGranularity(v)
		if !ok {
			return filter, enumParamError("time_granularity", v, timeGranularities)
		}
		filter.TimeRange.Granularity = gran
	}
	if after, ok, err := queryDate(r, "after"); err != nil {
		return filter, err
	} else if ok {
		filter.After = &after
	}
	if before, ok, err := queryDate(r, "before"); err != nil {
		return filter, err
	} else if ok {
		filter.Before = &before
	}
	if offset, ok, err := queryInt(r, "offset"); err != nil {
		return filter, err
	} else if ok && offset >= 0 {
		filter.Pagination.Offset = offset
	}
	if limit, ok, err := queryInt(r, "limit"); err != nil {
		return filter, err
	} else if ok && limit >= 0 {
		filter.Pagination.Limit = limit
	}
	if v := r.URL.Query().Get("sort"); v != "" {
		field, ok := parseTextSortField(v)
		if !ok {
			return filter, enumParamError("sort", v, textSortFields)
		}
		filter.SortField = field
	}
	if v := r.URL.Query().Get("direction"); v != "" {
		dir, ok := parseSortDirection(v)
		if !ok {
			return filter, enumParamError("direction", v, sortDirections)
		}
		filter.SortDirection = dir
	}

	return filter, nil
}

func parseTextAggregateOptions(r *http.Request) (query.TextAggregateOptions, error) {
	opts := query.TextAggregateOptions{
		SortField:       query.TextSortByCount,
		SortDirection:   query.SortDesc,
		Limit:           100,
		TimeGranularity: query.TimeMonth,
	}

	if id, ok, err := queryInt64(r, "source_id"); err != nil {
		return opts, err
	} else if ok {
		opts.SourceID = &id
	}
	if v := r.URL.Query().Get("sort"); v != "" {
		field, ok := parseTextSortField(v)
		if !ok {
			return opts, enumParamError("sort", v, textSortFields)
		}
		opts.SortField = field
	}
	if v := r.URL.Query().Get("direction"); v != "" {
		dir, ok := parseSortDirection(v)
		if !ok {
			return opts, enumParamError("direction", v, sortDirections)
		}
		opts.SortDirection = dir
	}
	if limit, ok, err := queryInt(r, "limit"); err != nil {
		return opts, err
	} else if ok && limit > 0 {
		opts.Limit = limit
	}
	if v := r.URL.Query().Get("time_granularity"); v != "" {
		gran, ok := parseTimeGranularity(v)
		if !ok {
			return opts, enumParamError("time_granularity", v, timeGranularities)
		}
		opts.TimeGranularity = gran
		opts.TimeGranularitySet = true
	}
	if v := r.URL.Query().Get("search_query"); v != "" {
		opts.SearchQuery = v
	}
	if after, ok, err := queryDate(r, "after"); err != nil {
		return opts, err
	} else if ok {
		opts.After = &after
	}
	if before, ok, err := queryDate(r, "before"); err != nil {
		return opts, err
	} else if ok {
		opts.Before = &before
	}

	return opts, nil
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

func toTextConversationRow(row query.ConversationRow) TextConversationRow {
	var lastMessageAt string
	if !row.LastMessageAt.IsZero() {
		lastMessageAt = row.LastMessageAt.UTC().Format(time.RFC3339)
	}
	return TextConversationRow{
		ConversationID:   row.ConversationID,
		Title:            row.Title,
		SourceType:       row.SourceType,
		MessageCount:     row.MessageCount,
		ParticipantCount: row.ParticipantCount,
		LastMessageAt:    lastMessageAt,
		LastPreview:      row.LastPreview,
	}
}

func (s *Server) textEngine(w http.ResponseWriter) (query.TextEngine, bool) {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "Query engine not available")
		return nil, false
	}
	textEngine, ok := s.engine.(query.TextEngine)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "text_engine_unavailable", "Text query engine not available")
		return nil, false
	}
	return textEngine, true
}

// toTotalStatsResponse converts query.TotalStats to JSON format.
func toTotalStatsResponse(stats *query.TotalStats) *TotalStatsResponse {
	if stats == nil {
		return nil
	}
	return &TotalStatsResponse{
		MessageCount:          stats.MessageCount,
		ActiveMessages:        stats.ActiveMessageCount,
		SourceDeletedMessages: stats.SourceDeletedMessageCount,
		TotalSize:             stats.TotalSize,
		AttachmentCount:       stats.AttachmentCount,
		AttachmentSize:        stats.AttachmentSize,
		LabelCount:            stats.LabelCount,
		AccountCount:          stats.AccountCount,
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
		ID:              m.ID,
		SourceMessageID: m.SourceMessageID,
		ConversationID:  m.ConversationID,
		Subject:         m.Subject,
		MessageType:     m.MessageType,
		From:            from,
		FromEmail:       m.FromEmail,
		FromName:        m.FromName,
		FromPhone:       m.FromPhone,
		To:              formatQueryAddresses(m.To),
		Cc:              formatQueryAddresses(m.Cc),
		Bcc:             formatQueryAddresses(m.Bcc),
		SentAt:          m.SentAt.UTC().Format(time.RFC3339),
		DeletedAt:       formatDeletedAt(m.DeletedAt),
		Snippet:         m.Snippet,
		Labels:          labels,
		HasAttach:       m.HasAttachments,
		SizeBytes:       m.SizeEstimate,
	}
}

func toBodySearchContext(m query.MessageSummary) BodySearchContext {
	return BodySearchContext{
		MessageID:                m.ID,
		ContextSnippets:          m.BodyContextSnippets,
		ContextSnippetsTruncated: m.BodyContextSnippetsTruncated,
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

	opts, err := parseAggregateOptions(r)
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}

	rows, err := s.engine.Aggregate(r.Context(), viewType, opts)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
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

	// The sub-aggregate endpoint reuses the message-filter parser for
	// drill-down scope, but sort/direction/limit/offset are owned by
	// parseAggregateOptions. Aggregate sort values (count, name,
	// attachment_size) are not valid message sorts, so parse the filter from
	// a request view with those aggregate-owned params removed to avoid a
	// spurious 400 from parseMessageFilter's message-sort validation.
	filter, err := parseMessageFilter(requestWithoutParams(r, "sort", "direction", "limit", "offset"))
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	opts, err := parseAggregateOptions(r)
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}

	rows, err := s.engine.SubAggregate(r.Context(), filter, viewType, opts)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
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

	filter, err := parseMessageFilter(r)
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
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
		if s.writeIfContextError(w, err) {
			return
		}
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

	writeJSON(w, http.StatusOK, FilteredMessagesResponse{
		Count:    len(summaries),
		HasMore:  hasMore,
		Offset:   filter.Pagination.Offset,
		Limit:    requestLimit,
		Messages: summaries,
	})
}

func (s *Server) handleGmailIDsByFilter(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "Query engine not available")
		return
	}

	filter, err := parseMessageFilter(r)
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	ids, err := s.engine.GetGmailIDsByFilter(r.Context(), filter)
	if err != nil {
		s.logger.Error("gmail id filter query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Gmail ID query failed")
		return
	}
	if ids == nil {
		ids = []string{}
	}

	writeJSON(w, http.StatusOK, GmailIDsResponse{GmailIDs: ids})
}

func (s *Server) handleGetAttachment(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "Query engine not available")
		return
	}

	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "Attachment ID must be a number")
		return
	}

	att, err := s.engine.GetAttachment(r.Context(), id)
	if err != nil {
		s.logger.Error("failed to get attachment", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve attachment")
		return
	}
	if att == nil {
		writeError(w, http.StatusNotFound, "not_found", "Attachment not found")
		return
	}

	writeJSON(w, http.StatusOK, AttachmentInfo{
		ID:          att.ID,
		Filename:    att.Filename,
		MimeType:    att.MimeType,
		Size:        att.Size,
		ContentHash: att.ContentHash,
		URL:         att.URL,
	})
}

// handleGetAttachmentContent streams a stored attachment's raw bytes by its
// SHA-256 content hash. The /content suffix keeps this binary response distinct
// from GET /attachments/{id}, which returns attachment metadata as JSON.
func (s *Server) handleGetAttachmentContent(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "Query engine not available")
		return
	}

	hash := r.PathValue("hash")

	if err := msgexport.ValidateContentHash(hash); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_hash", "Attachment hash must be a 64-character hex SHA-256")
		return
	}

	attachments, err := s.engine.GetAttachmentsByHash(r.Context(), hash)
	if err != nil {
		s.logger.Error("failed to look up attachment by hash", "error", err, "hash", hash)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to look up attachment")
		return
	}
	if len(attachments) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "Attachment not found")
		return
	}
	att := &attachments[0]

	var content io.ReadCloser
	var contentLength int64
	if s.blobStore != nil {
		content, contentLength, err = s.blobStore.OpenStream(r.Context(), hash)
	}
	if s.blobStore == nil || errors.Is(err, os.ErrNotExist) {
		content, contentLength, att, err = openLooseAttachmentCandidates(
			s.cfg.AttachmentsDir(), hash, attachments,
		)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not_found", "Attachment content not available")
			return
		}
		s.logger.Error("failed to open attachment content", "error", err, "hash", hash)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to open attachment")
		return
	}
	contentType := att.MimeType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", contentDisposition(att.Filename))
	w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")

	_, copyErr := io.Copy(w, content)
	if err := errors.Join(copyErr, content.Close()); err != nil {
		// Status and headers are already committed, so only logging is possible.
		s.logger.Error("failed to stream attachment", "error", err, "hash", hash)
	}
}

func openLooseAttachmentCandidates(
	attachmentsDir string,
	contentHash string,
	attachments []query.AttachmentInfo,
) (io.ReadCloser, int64, *query.AttachmentInfo, error) {
	content, contentLength, err := openLooseAttachmentContent(attachmentsDir, contentHash, "")
	if err == nil {
		return content, contentLength, &attachments[0], nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil, err
	}

	seen := make(map[string]struct{}, len(attachments))
	var firstPathError error
	for i := range attachments {
		storagePath := attachments[i].StoragePath
		if storagePath == "" {
			continue
		}
		if _, ok := seen[storagePath]; ok {
			continue
		}
		seen[storagePath] = struct{}{}

		content, contentLength, err = openLooseAttachmentContent(attachmentsDir, contentHash, storagePath)
		if err == nil {
			return content, contentLength, &attachments[i], nil
		}
		if !errors.Is(err, os.ErrNotExist) && firstPathError == nil {
			firstPathError = err
		}
	}
	if firstPathError != nil {
		return nil, 0, nil, firstPathError
	}
	return nil, 0, nil, os.ErrNotExist
}

func openLooseAttachmentContent(attachmentsDir, contentHash, storagePath string) (io.ReadCloser, int64, error) {
	var path string
	var err error
	if storagePath == "" {
		path, err = msgexport.StoragePath(attachmentsDir, contentHash)
	} else {
		path, err = resolveRecordedAttachmentPath(attachmentsDir, storagePath)
	}
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, 0, fmt.Errorf("attachment storage path %q is not a regular file", storagePath)
	}
	return f, info.Size(), nil
}

func resolveRecordedAttachmentPath(attachmentsDir, storagePath string) (string, error) {
	lowerPath := strings.ToLower(storagePath)
	if strings.HasPrefix(lowerPath, "http://") || strings.HasPrefix(lowerPath, "https://") {
		return "", errors.New("attachment storage path must be local")
	}
	localPath := filepath.Clean(filepath.FromSlash(storagePath))
	if !filepath.IsLocal(localPath) {
		return "", fmt.Errorf("attachment storage path %q escapes attachments directory", storagePath)
	}
	basePath, err := filepath.Abs(attachmentsDir)
	if err != nil {
		return "", fmt.Errorf("resolve attachments directory: %w", err)
	}
	basePath, err = filepath.EvalSymlinks(basePath)
	if err != nil {
		return "", fmt.Errorf("resolve attachments directory: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(filepath.Join(basePath, localPath))
	if err != nil {
		return "", err
	}
	relativePath, err := filepath.Rel(basePath, resolvedPath)
	if err != nil || !filepath.IsLocal(relativePath) {
		return "", fmt.Errorf("attachment storage path %q escapes attachments directory", storagePath)
	}
	return resolvedPath, nil
}

func contentDisposition(filename string) string {
	if filename == "" {
		return "attachment"
	}
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r >= 0x7f || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, filename)
	value := fmt.Sprintf("attachment; filename=%q", ascii)
	if ascii != filename {
		value += "; filename*=UTF-8''" + url.PathEscape(filename)
	}
	return value
}

func (s *Server) handleSearchByDomains(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "Query engine not available")
		return
	}

	domains := parseDomainValues(r.URL.Query()["domains"])
	if len(domains) == 0 {
		writeError(w, http.StatusBadRequest, "missing_domains", "At least one domain is required")
		return
	}

	filter, err := parseMessageFilter(r)
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if filter.Pagination.Limit <= 0 {
		filter.Pagination.Limit = maxPageSize
	}
	if filter.Pagination.Limit > maxPageSize {
		filter.Pagination.Limit = maxPageSize
	}

	requestLimit := filter.Pagination.Limit
	messages, err := s.engine.SearchByDomains(
		r.Context(),
		domains,
		filter.After,
		filter.Before,
		requestLimit+1,
		filter.Pagination.Offset,
	)
	if err != nil {
		s.logger.Error("domain search failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Domain search failed")
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

	writeJSON(w, http.StatusOK, FilteredMessagesResponse{
		Count:    len(summaries),
		HasMore:  hasMore,
		Offset:   filter.Pagination.Offset,
		Limit:    requestLimit,
		Messages: summaries,
	})
}

func parseDomainValues(values []string) []string {
	var domains []string
	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			domains = append(domains, part)
		}
	}
	return domains
}

// handleTotalStats returns detailed stats with optional filters.
// GET /api/v1/stats/total?source_id=1&attachments_only=true.
func (s *Server) handleTotalStats(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "Query engine not available")
		return
	}

	var opts query.StatsOptions

	if id, ok, err := queryInt64(r, "source_id"); err != nil {
		s.rejectBadParam(w, err)
		return
	} else if ok {
		opts.SourceID = &id
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
		viewType, ok := parseViewType(v)
		if !ok {
			s.rejectBadParam(w, enumParamError("group_by", v, aggregateViewTypes))
			return
		}
		opts.GroupBy = viewType
	}

	stats, err := s.engine.GetTotalStats(r.Context(), opts)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
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

	filter, err := parseMessageFilter(r)
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	q := search.Parse(queryStr)
	if err := q.Err(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}

	// Reject filter fields that the search engines cannot honor.
	// SenderName/RecipientName use display names that aren't indexed
	// for search, ConversationID scoping isn't implemented, and
	// EmptyValueTargets is an aggregate-only concept. The parsed
	// message_type: operator is honored by the query engine; the
	// filter parameter form is still list-search-only.
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

	result, err := s.engine.SearchFastWithStats(r.Context(), q, queryStr, filter, statsGroupBy, limit, offset)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
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

// handleDeepSearch performs composite full-text search, or exact body-only
// search when scope=body is requested.
// GET /api/v1/search/deep?q=invoice&scope=body&offset=0&limit=100&source_id=1&hide_deleted=true.
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
	scope := r.URL.Query().Get("scope")
	if scope != "" && scope != "body" {
		writeError(w, http.StatusBadRequest, "invalid_scope", "Invalid search scope. Must be 'body' or omitted")
		return
	}

	filter, err := parseMessageFilter(r)
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	q := search.Parse(queryStr)
	if err := q.Err(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	if scope == "body" && len(q.TextTerms) == 0 {
		writeError(w, http.StatusBadRequest, "missing_free_text",
			"Body-scoped search requires at least one free-text term")
		return
	}

	// Reject filter fields that this deep-search engine path cannot
	// honor. Without this check the parameters parse
	// successfully but silently do nothing, letting deep search
	// escape the current drill-down scope.
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

	merged := query.MergeFilterIntoQuery(q, filter)

	// Fetch one extra row to determine has_more accurately.
	var messages []query.MessageSummary
	if scope == "body" {
		bodySearcher, ok := s.engine.(query.MessageBodySearcher)
		if !ok {
			writeError(w, http.StatusNotImplemented, "body_search_unavailable",
				"Query engine does not support exact message body search")
			return
		}
		messages, err = bodySearcher.SearchMessageBodies(r.Context(), merged, limit+1, offset)
	} else {
		messages, err = s.engine.Search(r.Context(), merged, limit+1, offset)
	}
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		if scope == "body" && errors.Is(err, query.ErrMessageBodySearchInvalidQuery) {
			writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		if scope == "body" && (errors.Is(err, query.ErrMessageBodySearchUnavailable) ||
			errors.Is(err, query.ErrMessageBodySearchIndexStale)) {
			writeError(w, http.StatusServiceUnavailable, "body_search_index_unavailable", err.Error())
			return
		}
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
	var bodyContexts []BodySearchContext
	if scope == "body" {
		bodyContexts = make([]BodySearchContext, len(messages))
		for i, message := range messages {
			bodyContexts[i] = toBodySearchContext(message)
		}
	}

	writeJSON(w, http.StatusOK, DeepSearchResponse{
		Query:        queryStr,
		Scope:        scope,
		Messages:     summaries,
		BodyContexts: bodyContexts,
		Count:        len(summaries),
		HasMore:      hasMore,
		Offset:       offset,
		Limit:        limit,
	})
}

func (s *Server) handleTextConversations(w http.ResponseWriter, r *http.Request) {
	textEngine, ok := s.textEngine(w)
	if !ok {
		return
	}

	filter, err := parseTextFilter(r)
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if filter.Pagination.Limit <= 0 {
		filter.Pagination.Limit = 100
	}
	if filter.Pagination.Limit > maxPageSize {
		filter.Pagination.Limit = maxPageSize
	}

	requestLimit := filter.Pagination.Limit
	filter.Pagination.Limit = requestLimit + 1
	rows, err := textEngine.ListConversations(r.Context(), filter)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		s.logger.Error("text conversations query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Text conversations query failed")
		return
	}

	hasMore := len(rows) > requestLimit
	if hasMore {
		rows = rows[:requestLimit]
	}

	conversations := make([]TextConversationRow, len(rows))
	for i, row := range rows {
		conversations[i] = toTextConversationRow(row)
	}

	writeJSON(w, http.StatusOK, TextConversationsResponse{
		Count:         len(conversations),
		HasMore:       hasMore,
		Offset:        filter.Pagination.Offset,
		Limit:         requestLimit,
		Conversations: conversations,
	})
}

func (s *Server) handleTextAggregates(w http.ResponseWriter, r *http.Request) {
	textEngine, ok := s.textEngine(w)
	if !ok {
		return
	}

	viewTypeStr := r.URL.Query().Get("view_type")
	if viewTypeStr == "" {
		viewTypeStr = "contacts"
	}
	viewType, ok := parseTextViewType(viewTypeStr)
	if !ok || viewType == query.TextViewConversations {
		writeError(w, http.StatusBadRequest, "invalid_view_type",
			"Invalid view_type. Must be one of: contacts, contact_names, sources, labels, time")
		return
	}

	opts, err := parseTextAggregateOptions(r)
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	rows, err := textEngine.TextAggregate(r.Context(), viewType, opts)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		s.logger.Error("text aggregate query failed", "view_type", viewTypeStr, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Text aggregate query failed")
		return
	}

	jsonRows := make([]AggregateRowJSON, len(rows))
	for i, row := range rows {
		jsonRows[i] = toAggregateRowJSON(row)
	}

	writeJSON(w, http.StatusOK, AggregateResponse{
		ViewType: textViewTypeString(viewType),
		Rows:     jsonRows,
	})
}

func (s *Server) handleTextConversationMessages(w http.ResponseWriter, r *http.Request) {
	textEngine, ok := s.textEngine(w)
	if !ok {
		return
	}

	idStr := r.PathValue("id")
	conversationID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || conversationID < 1 {
		writeError(w, http.StatusBadRequest, "invalid_id", "Conversation ID must be a positive integer")
		return
	}

	filter, err := parseTextFilter(r)
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if filter.Pagination.Limit <= 0 {
		filter.Pagination.Limit = maxPageSize
	}
	if filter.Pagination.Limit > maxPageSize {
		filter.Pagination.Limit = maxPageSize
	}

	requestLimit := filter.Pagination.Limit
	filter.Pagination.Limit = requestLimit + 1
	messages, err := textEngine.ListConversationMessages(r.Context(), conversationID, filter)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		s.logger.Error("text conversation messages query failed", "conversation_id", conversationID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Text conversation messages query failed")
		return
	}

	hasMore := len(messages) > requestLimit
	if hasMore {
		messages = messages[:requestLimit]
	}
	if messages == nil {
		messages = []query.MessageSummary{}
	}

	writeJSON(w, http.StatusOK, TextMessagesResponse{
		Count:    len(messages),
		HasMore:  hasMore,
		Offset:   filter.Pagination.Offset,
		Limit:    requestLimit,
		Messages: messages,
	})
}

func (s *Server) handleTextSearch(w http.ResponseWriter, r *http.Request) {
	textEngine, ok := s.textEngine(w)
	if !ok {
		return
	}

	queryStr := r.URL.Query().Get("q")
	if queryStr == "" {
		writeError(w, http.StatusBadRequest, "missing_query", "Query parameter 'q' is required")
		return
	}

	offset, _, err := queryInt(r, "offset")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if offset < 0 {
		offset = 0
	}
	limit, _, err := queryInt(r, "limit")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}

	messages, err := textEngine.TextSearch(r.Context(), queryStr, limit+1, offset)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		s.logger.Error("text search failed", "query", queryStr, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Text search failed")
		return
	}

	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}
	if messages == nil {
		messages = []query.MessageSummary{}
	}

	writeJSON(w, http.StatusOK, TextMessagesResponse{
		Count:    len(messages),
		HasMore:  hasMore,
		Offset:   offset,
		Limit:    limit,
		Messages: messages,
	})
}

func (s *Server) handleTextStats(w http.ResponseWriter, r *http.Request) {
	textEngine, ok := s.textEngine(w)
	if !ok {
		return
	}

	var opts query.TextStatsOptions
	if id, ok, err := queryInt64(r, "source_id"); err != nil {
		s.rejectBadParam(w, err)
		return
	} else if ok {
		opts.SourceID = &id
	}
	opts.SearchQuery = r.URL.Query().Get("search_query")

	stats, err := textEngine.GetTextStats(r.Context(), opts)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		s.logger.Error("text stats query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Text stats query failed")
		return
	}

	writeJSON(w, http.StatusOK, toTotalStatsResponse(stats))
}

// isEngineUnsupported reports whether err indicates the configured query
// engine cannot satisfy the requested operation. Postgres and remote engines
// both have methods that return sentinel errors instead of data; mapping
// those to a stable status code keeps the API honest about engine
// capabilities rather than emitting 500 for predictable misses.
func isEngineUnsupported(err error) bool {
	return errors.Is(err, query.ErrNotImplemented) || errors.Is(err, daemonclient.ErrNotSupported)
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

	idStr := r.PathValue("id")
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
