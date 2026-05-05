// Package remote provides HTTP client implementations for accessing a remote msgvault server.
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wesm/msgvault/internal/query"
	"github.com/wesm/msgvault/internal/search"
)

// ErrNotSupported is returned for operations not available in remote mode.
var ErrNotSupported = errors.New("operation not supported in remote mode")

// Engine implements query.Engine by making HTTP calls to a remote msgvault server.
type Engine struct {
	store *Store
}

// Compile-time check that Engine implements query.Engine.
var _ query.Engine = (*Engine)(nil)

// NewEngine creates a new remote query engine.
func NewEngine(cfg Config) (*Engine, error) {
	s, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return &Engine{store: s}, nil
}

// NewEngineFromStore creates a new remote query engine from an existing store.
func NewEngineFromStore(s *Store) *Engine {
	return &Engine{store: s}
}

// IsRemote returns true, indicating this is a remote engine.
func (e *Engine) IsRemote() bool {
	return true
}

// Close releases resources held by the engine.
func (e *Engine) Close() error {
	return e.store.Close()
}

// ============================================================================
// API Response Types
// ============================================================================

// aggregateResponse matches the API aggregate response format.
type aggregateResponse struct {
	ViewType string             `json:"view_type"`
	Rows     []aggregateRowJSON `json:"rows"`
}

// aggregateRowJSON represents a single aggregate row in JSON format.
type aggregateRowJSON struct {
	Key             string `json:"key"`
	Count           int64  `json:"count"`
	TotalSize       int64  `json:"total_size"`
	AttachmentSize  int64  `json:"attachment_size"`
	AttachmentCount int64  `json:"attachment_count"`
	TotalUnique     int64  `json:"total_unique"`
}

// totalStatsResponse matches the API total stats response format.
type totalStatsResponse struct {
	MessageCount    int64 `json:"message_count"`
	TotalSize       int64 `json:"total_size"`
	AttachmentCount int64 `json:"attachment_count"`
	AttachmentSize  int64 `json:"attachment_size"`
	LabelCount      int64 `json:"label_count"`
	AccountCount    int64 `json:"account_count"`
}

// filteredMessagesResponse matches the API filtered messages response format.
type filteredMessagesResponse struct {
	Count    int                  `json:"count"`
	HasMore  bool                 `json:"has_more"`
	Offset   int                  `json:"offset"`
	Limit    int                  `json:"limit"`
	Messages []messageSummaryJSON `json:"messages"`
}

// messageSummaryJSON represents a message summary in JSON format.
type messageSummaryJSON struct {
	ID             int64    `json:"id"`
	ConversationID int64    `json:"conversation_id,omitempty"`
	Subject        string   `json:"subject"`
	From           string   `json:"from"`
	To             []string `json:"to"`
	SentAt         string   `json:"sent_at"`
	DeletedAt      string   `json:"deleted_at,omitempty"`
	Snippet        string   `json:"snippet"`
	Labels         []string `json:"labels"`
	HasAttach      bool     `json:"has_attachments"`
	SizeBytes      int64    `json:"size_bytes"`
}

// searchFastResponse matches the API fast search response format.
type searchFastResponse struct {
	Query      string               `json:"query"`
	Messages   []messageSummaryJSON `json:"messages"`
	TotalCount int64                `json:"total_count"`
	Stats      *totalStatsResponse  `json:"stats,omitempty"`
}

// deepSearchResponse matches the API deep search response format.
type deepSearchResponse struct {
	Query    string               `json:"query"`
	Messages []messageSummaryJSON `json:"messages"`
	Count    int                  `json:"count"`
	HasMore  bool                 `json:"has_more"`
	Offset   int                  `json:"offset"`
	Limit    int                  `json:"limit"`
}

// ============================================================================
// Helper Functions
// ============================================================================

// viewTypeToString converts a query.ViewType to its API string representation.
func viewTypeToString(v query.ViewType) string {
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
		return "senders"
	}
}

// sortFieldToString converts a query.SortField to its API string representation.
func sortFieldToString(f query.SortField) string {
	switch f {
	case query.SortByCount:
		return "count"
	case query.SortBySize:
		return "size"
	case query.SortByAttachmentSize:
		return "attachment_size"
	case query.SortByName:
		return "name"
	default:
		return "count"
	}
}

// sortDirectionToString converts a query.SortDirection to its API string representation.
func sortDirectionToString(d query.SortDirection) string {
	if d == query.SortAsc {
		return "asc"
	}
	return "desc"
}

// timeGranularityToString converts a query.TimeGranularity to its API string representation.
func timeGranularityToString(g query.TimeGranularity) string {
	switch g {
	case query.TimeYear:
		return "year"
	case query.TimeMonth:
		return "month"
	case query.TimeDay:
		return "day"
	default:
		return "month"
	}
}

// messageSortFieldToString converts a query.MessageSortField to its API string representation.
func messageSortFieldToString(f query.MessageSortField) string {
	switch f {
	case query.MessageSortByDate:
		return "date"
	case query.MessageSortBySize:
		return "size"
	case query.MessageSortBySubject:
		return "subject"
	default:
		return "date"
	}
}

// buildAggregateQuery builds query parameters for aggregate endpoints.
func buildAggregateQuery(viewType query.ViewType, opts query.AggregateOptions) url.Values {
	params := url.Values{}
	params.Set("view_type", viewTypeToString(viewType))
	params.Set("sort", sortFieldToString(opts.SortField))
	params.Set("direction", sortDirectionToString(opts.SortDirection))

	if opts.Limit > 0 {
		params.Set("limit", strconv.Itoa(opts.Limit))
	}
	params.Set("time_granularity", timeGranularityToString(opts.TimeGranularity))

	if opts.SourceID != nil {
		params.Set("source_id", strconv.FormatInt(*opts.SourceID, 10))
	}
	if opts.WithAttachmentsOnly {
		params.Set("attachments_only", "true")
	}
	if opts.HideDeletedFromSource {
		params.Set("hide_deleted", "true")
	}
	if opts.SearchQuery != "" {
		params.Set("search_query", opts.SearchQuery)
	}
	if opts.After != nil {
		params.Set("after", opts.After.Format(time.RFC3339))
	}
	if opts.Before != nil {
		params.Set("before", opts.Before.Format(time.RFC3339))
	}

	return params
}

// buildFilterQuery builds query parameters for filter endpoints.
func buildFilterQuery(filter query.MessageFilter) url.Values {
	params := url.Values{}

	if filter.Sender != "" {
		params.Set("sender", filter.Sender)
	}
	if filter.SenderName != "" {
		params.Set("sender_name", filter.SenderName)
	}
	if filter.Recipient != "" {
		params.Set("recipient", filter.Recipient)
	}
	if filter.RecipientName != "" {
		params.Set("recipient_name", filter.RecipientName)
	}
	if filter.Domain != "" {
		params.Set("domain", filter.Domain)
	}
	if filter.Label != "" {
		params.Set("label", filter.Label)
	}
	if filter.TimeRange.Period != "" {
		params.Set("time_period", filter.TimeRange.Period)
	}
	params.Set("time_granularity", timeGranularityToString(filter.TimeRange.Granularity))

	if filter.ConversationID != nil {
		params.Set("conversation_id", strconv.FormatInt(*filter.ConversationID, 10))
	}
	if filter.SourceID != nil {
		params.Set("source_id", strconv.FormatInt(*filter.SourceID, 10))
	}
	if filter.WithAttachmentsOnly {
		params.Set("attachments_only", "true")
	}
	if filter.HideDeletedFromSource {
		params.Set("hide_deleted", "true")
	}

	// Pagination
	if filter.Pagination.Offset > 0 {
		params.Set("offset", strconv.Itoa(filter.Pagination.Offset))
	}
	if filter.Pagination.Limit > 0 {
		params.Set("limit", strconv.Itoa(filter.Pagination.Limit))
	}

	// Sorting
	params.Set("sort", messageSortFieldToString(filter.Sorting.Field))
	params.Set("direction", sortDirectionToString(filter.Sorting.Direction))

	// Date range filters
	if filter.After != nil {
		params.Set("after", filter.After.Format(time.RFC3339))
	}
	if filter.Before != nil {
		params.Set("before", filter.Before.Format(time.RFC3339))
	}

	// EmptyValueTargets — serialize active view types as comma-separated list
	if filter.HasEmptyTargets() {
		var targets []string
		for vt, active := range filter.EmptyValueTargets {
			if active {
				targets = append(targets, viewTypeToString(vt))
			}
		}
		if len(targets) > 0 {
			params.Set("empty_targets", strings.Join(targets, ","))
		}
	}

	return params
}

// buildStatsQuery builds query parameters for stats endpoints.
func buildStatsQuery(opts query.StatsOptions) url.Values {
	params := url.Values{}

	if opts.SourceID != nil {
		params.Set("source_id", strconv.FormatInt(*opts.SourceID, 10))
	}
	if opts.WithAttachmentsOnly {
		params.Set("attachments_only", "true")
	}
	if opts.HideDeletedFromSource {
		params.Set("hide_deleted", "true")
	}
	if opts.SearchQuery != "" {
		params.Set("search_query", opts.SearchQuery)
	}
	if opts.GroupBy != 0 {
		params.Set("group_by", viewTypeToString(opts.GroupBy))
	}

	return params
}

// parseAggregateResponse parses the JSON response body into aggregate rows.
func parseAggregateResponse(body []byte) ([]query.AggregateRow, error) {
	var resp aggregateResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode aggregate response: %w", err)
	}

	rows := make([]query.AggregateRow, len(resp.Rows))
	for i, r := range resp.Rows {
		rows[i] = query.AggregateRow{
			Key:             r.Key,
			Count:           r.Count,
			TotalSize:       r.TotalSize,
			AttachmentSize:  r.AttachmentSize,
			AttachmentCount: r.AttachmentCount,
			TotalUnique:     r.TotalUnique,
		}
	}
	return rows, nil
}

// parseMessageSummaries converts JSON message summaries to query.MessageSummary.
func parseMessageSummaries(msgs []messageSummaryJSON) []query.MessageSummary {
	result := make([]query.MessageSummary, len(msgs))
	for i, m := range msgs {
		sentAt := parseTime(m.SentAt)
		var deletedAt *time.Time
		if m.DeletedAt != "" {
			t := parseTime(m.DeletedAt)
			if !t.IsZero() {
				deletedAt = &t
			}
		}
		result[i] = query.MessageSummary{
			ID:             m.ID,
			ConversationID: m.ConversationID,
			Subject:        m.Subject,
			FromEmail:      m.From,
			SentAt:         sentAt,
			DeletedAt:      deletedAt,
			Snippet:        m.Snippet,
			Labels:         m.Labels,
			HasAttachments: m.HasAttach,
			SizeEstimate:   m.SizeBytes,
		}
	}
	return result
}

// ============================================================================
// Engine Interface Implementation
// ============================================================================

// Aggregate performs grouping based on the provided ViewType.
func (e *Engine) Aggregate(ctx context.Context, groupBy query.ViewType, opts query.AggregateOptions) ([]query.AggregateRow, error) {
	params := buildAggregateQuery(groupBy, opts)
	path := "/api/v1/aggregates?" + params.Encode()

	resp, err := e.store.doRequestWithContext(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, handleErrorResponse(resp)
	}

	var body []byte
	body, err = readBody(resp)
	if err != nil {
		return nil, err
	}

	return parseAggregateResponse(body)
}

// SubAggregate performs aggregation on a filtered subset of messages.
func (e *Engine) SubAggregate(ctx context.Context, filter query.MessageFilter, groupBy query.ViewType, opts query.AggregateOptions) ([]query.AggregateRow, error) {
	// Merge filter params with aggregate options
	params := buildFilterQuery(filter)
	params.Set("view_type", viewTypeToString(groupBy))
	params.Set("sort", sortFieldToString(opts.SortField))
	params.Set("direction", sortDirectionToString(opts.SortDirection))
	if opts.Limit > 0 {
		params.Set("limit", strconv.Itoa(opts.Limit))
	}
	// Use TimeGranularity from opts, not from filter (fixes roborev finding)
	params.Set("time_granularity", timeGranularityToString(opts.TimeGranularity))
	if opts.SearchQuery != "" {
		params.Set("search_query", opts.SearchQuery)
	}

	path := "/api/v1/aggregates/sub?" + params.Encode()

	resp, err := e.store.doRequestWithContext(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, handleErrorResponse(resp)
	}

	body, err := readBody(resp)
	if err != nil {
		return nil, err
	}

	return parseAggregateResponse(body)
}

// ListMessages returns messages matching the filter criteria.
func (e *Engine) ListMessages(ctx context.Context, filter query.MessageFilter) ([]query.MessageSummary, error) {
	params := buildFilterQuery(filter)
	path := "/api/v1/messages/filter?" + params.Encode()

	resp, err := e.store.doRequestWithContext(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, handleErrorResponse(resp)
	}

	var fmr filteredMessagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&fmr); err != nil {
		return nil, fmt.Errorf("decode messages response: %w", err)
	}

	return parseMessageSummaries(fmr.Messages), nil
}

// GetMessage returns a single message by ID.
func (e *Engine) GetMessage(ctx context.Context, id int64) (*query.MessageDetail, error) {
	msg, err := e.store.GetMessage(id)
	if err != nil {
		return nil, err
	}
	if msg == nil {
		return nil, nil
	}

	// Convert store.APIMessage to query.MessageDetail
	detail := &query.MessageDetail{
		ID:             msg.ID,
		ConversationID: msg.ConversationID,
		Subject:        msg.Subject,
		Snippet:        msg.Snippet,
		SentAt:         msg.SentAt,
		SizeEstimate:   msg.SizeEstimate,
		Labels:         msg.Labels,
		BodyText:       msg.Body, // API returns combined body
	}

	// Parse From address
	if msg.From != "" {
		detail.From = []query.Address{{Email: msg.From}}
	}

	// Parse To addresses
	for _, to := range msg.To {
		detail.To = append(detail.To, query.Address{Email: to})
	}

	// Convert attachments
	for _, att := range msg.Attachments {
		detail.Attachments = append(detail.Attachments, query.AttachmentInfo{
			Filename: att.Filename,
			MimeType: att.MimeType,
			Size:     att.Size,
		})
	}

	detail.HasAttachments = len(detail.Attachments) > 0

	return detail, nil
}

// GetMessageBySourceID returns a message by its source message ID.
// This operation is not supported in remote mode.
func (e *Engine) GetMessageBySourceID(ctx context.Context, sourceMessageID string) (*query.MessageDetail, error) {
	return nil, ErrNotSupported
}

// GetMessageSummariesByIDs loops GetMessage over the supplied IDs.
// Remote mode pays one HTTP round-trip per id; a future remote API
// could add /messages?ids=... to collapse this into a single call.
// For now, this method exists to satisfy the query.Engine contract
// — the local SQLite/DuckDB engines are the ones doing the bulk
// optimization for vector/hybrid search hydration.
func (e *Engine) GetMessageSummariesByIDs(ctx context.Context, ids []int64) ([]query.MessageSummary, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([]query.MessageSummary, 0, len(ids))
	for _, id := range ids {
		md, err := e.GetMessage(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("get message %d: %w", id, err)
		}
		if md == nil {
			continue
		}
		summary := query.MessageSummary{
			ID:                   md.ID,
			SourceMessageID:      md.SourceMessageID,
			ConversationID:       md.ConversationID,
			SourceConversationID: md.SourceConversationID,
			Subject:              md.Subject,
			Snippet:              md.Snippet,
			SentAt:               md.SentAt,
			SizeEstimate:         md.SizeEstimate,
			HasAttachments:       md.HasAttachments,
			AttachmentCount:      len(md.Attachments),
			Labels:               md.Labels,
		}
		// Carry sender details from the first From address so remote
		// MCP search_messages/find_similar_messages responses don't
		// silently drop who sent each hit. FromPhone is omitted — the
		// HTTP remote API does not expose it today; callers that need
		// it must fall back to a local engine.
		if len(md.From) > 0 {
			summary.FromEmail = md.From[0].Email
			summary.FromName = md.From[0].Name
		}
		out = append(out, summary)
	}
	return out, nil
}

// GetMessageRaw returns raw MIME data for a message.
// This operation is not supported in remote mode.
func (e *Engine) GetMessageRaw(ctx context.Context, id int64) ([]byte, error) {
	return nil, ErrNotSupported
}

// GetAttachment returns attachment metadata by ID.
// This operation is not supported in remote mode.
func (e *Engine) GetAttachment(ctx context.Context, id int64) (*query.AttachmentInfo, error) {
	return nil, ErrNotSupported
}

// Search performs full-text search including message body.
func (e *Engine) Search(ctx context.Context, q *search.Query, limit, offset int) ([]query.MessageSummary, error) {
	// Build query string from search.Query
	queryStr := buildSearchQueryString(q)
	if queryStr == "" {
		return nil, nil
	}

	params := url.Values{}
	params.Set("q", queryStr)
	params.Set("offset", strconv.Itoa(offset))
	params.Set("limit", strconv.Itoa(limit))

	// Forward filter-only fields that can't be represented in the
	// query string syntax (AccountIDs, HideDeleted). The remote API
	// accepts a single source_id; CLI/MCP layers reject collection
	// scope in remote mode, but defend here against any future caller
	// that bypasses those checks rather than silently dropping IDs.
	if len(q.AccountIDs) > 1 {
		return nil, fmt.Errorf(
			"remote search does not support multi-account scope; "+
				"got %d account IDs", len(q.AccountIDs))
	}
	if len(q.AccountIDs) == 1 {
		params.Set("source_id", strconv.FormatInt(q.AccountIDs[0], 10))
	}
	if q.HideDeleted {
		params.Set("hide_deleted", "true")
	}

	path := "/api/v1/search/deep?" + params.Encode()

	resp, err := e.store.doRequestWithContext(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, handleErrorResponse(resp)
	}

	var dsr deepSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&dsr); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}

	return parseMessageSummaries(dsr.Messages), nil
}

// SearchFast searches message metadata only (no body text).
func (e *Engine) SearchFast(ctx context.Context, q *search.Query, filter query.MessageFilter, limit, offset int) ([]query.MessageSummary, error) {
	result, err := e.SearchFastWithStats(ctx, q, buildSearchQueryString(q), filter, query.ViewSenders, limit, offset)
	if err != nil {
		return nil, err
	}
	return result.Messages, nil
}

// SearchFastCount returns the total count of messages matching a search query.
func (e *Engine) SearchFastCount(ctx context.Context, q *search.Query, filter query.MessageFilter) (int64, error) {
	// Use SearchFastWithStats with limit 0 to get count only
	result, err := e.SearchFastWithStats(ctx, q, buildSearchQueryString(q), filter, query.ViewSenders, 0, 0)
	if err != nil {
		return 0, err
	}
	return result.TotalCount, nil
}

// SearchFastWithStats performs a fast metadata search and returns paginated results,
// total count, and aggregate stats in a single operation.
func (e *Engine) SearchFastWithStats(ctx context.Context, q *search.Query, queryStr string,
	filter query.MessageFilter, statsGroupBy query.ViewType, limit, offset int) (*query.SearchFastResult, error) {

	params := buildFilterQuery(filter)
	params.Set("q", queryStr)
	params.Set("offset", strconv.Itoa(offset))
	params.Set("limit", strconv.Itoa(limit))
	params.Set("view_type", viewTypeToString(statsGroupBy))

	path := "/api/v1/search/fast?" + params.Encode()

	resp, err := e.store.doRequestWithContext(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, handleErrorResponse(resp)
	}

	var sfr searchFastResponse
	if err := json.NewDecoder(resp.Body).Decode(&sfr); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}

	result := &query.SearchFastResult{
		Messages:   parseMessageSummaries(sfr.Messages),
		TotalCount: sfr.TotalCount,
	}

	if sfr.Stats != nil {
		result.Stats = &query.TotalStats{
			MessageCount:    sfr.Stats.MessageCount,
			TotalSize:       sfr.Stats.TotalSize,
			AttachmentCount: sfr.Stats.AttachmentCount,
			AttachmentSize:  sfr.Stats.AttachmentSize,
			LabelCount:      sfr.Stats.LabelCount,
			AccountCount:    sfr.Stats.AccountCount,
		}
	}

	return result, nil
}

// GetGmailIDsByFilter returns Gmail message IDs matching a filter.
// This operation is not supported in remote mode.
func (e *Engine) GetGmailIDsByFilter(ctx context.Context, filter query.MessageFilter) ([]string, error) {
	return nil, ErrNotSupported
}

func (e *Engine) SearchByDomains(ctx context.Context, domains []string, after, before *time.Time, limit, offset int) ([]query.MessageSummary, error) {
	return nil, ErrNotSupported
}

// ListAccounts returns all configured accounts.
func (e *Engine) ListAccounts(ctx context.Context) ([]query.AccountInfo, error) {
	accounts, err := e.store.ListAccounts()
	if err != nil {
		return nil, err
	}

	result := make([]query.AccountInfo, len(accounts))
	for i, acc := range accounts {
		result[i] = query.AccountInfo{
			ID:          acc.ID,
			Identifier:  acc.Email,
			DisplayName: acc.DisplayName,
		}
	}
	return result, nil
}

// GetTotalStats returns overall database statistics.
func (e *Engine) GetTotalStats(ctx context.Context, opts query.StatsOptions) (*query.TotalStats, error) {
	params := buildStatsQuery(opts)
	path := "/api/v1/stats/total?" + params.Encode()

	resp, err := e.store.doRequestWithContext(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, handleErrorResponse(resp)
	}

	var tsr totalStatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tsr); err != nil {
		return nil, fmt.Errorf("decode stats response: %w", err)
	}

	return &query.TotalStats{
		MessageCount:    tsr.MessageCount,
		TotalSize:       tsr.TotalSize,
		AttachmentCount: tsr.AttachmentCount,
		AttachmentSize:  tsr.AttachmentSize,
		LabelCount:      tsr.LabelCount,
		AccountCount:    tsr.AccountCount,
	}, nil
}

// buildSearchQueryString reconstructs a search query string from a parsed Query.
// This is needed because the API expects the raw query string.
func buildSearchQueryString(q *search.Query) string {
	if q == nil {
		return ""
	}

	var parts []string

	parts = append(parts, q.TextTerms...)
	for _, addr := range q.FromAddrs {
		parts = append(parts, "from:"+addr)
	}
	for _, addr := range q.ToAddrs {
		parts = append(parts, "to:"+addr)
	}
	for _, addr := range q.CcAddrs {
		parts = append(parts, "cc:"+addr)
	}
	for _, addr := range q.BccAddrs {
		parts = append(parts, "bcc:"+addr)
	}
	for _, term := range q.SubjectTerms {
		parts = append(parts, "subject:"+term)
	}
	for _, label := range q.Labels {
		parts = append(parts, "label:"+label)
	}
	if q.HasAttachment != nil && *q.HasAttachment {
		parts = append(parts, "has:attachment")
	}
	if q.BeforeDate != nil {
		parts = append(parts, "before:"+q.BeforeDate.Format("2006-01-02"))
	}
	if q.AfterDate != nil {
		parts = append(parts, "after:"+q.AfterDate.Format("2006-01-02"))
	}
	if q.LargerThan != nil {
		parts = append(parts, fmt.Sprintf("larger:%d", *q.LargerThan))
	}
	if q.SmallerThan != nil {
		parts = append(parts, fmt.Sprintf("smaller:%d", *q.SmallerThan))
	}

	result := ""
	for i, part := range parts {
		if i > 0 {
			result += " "
		}
		result += part
	}
	return result
}

// readBody reads the response body into a byte slice.
func readBody(resp *http.Response) ([]byte, error) {
	return io.ReadAll(resp.Body)
}
