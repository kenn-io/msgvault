package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// ErrNotSupported is returned for operations not available through the daemon API.
var ErrNotSupported = errors.New("operation not supported through daemon API")

const (
	apiValueCount   = "count"
	apiValueLabels  = "labels"
	apiValueSubject = "subject"
)

// Engine implements query.Engine by making HTTP calls to a msgvault daemon.
type Engine struct {
	store *Client
}

// Compile-time check that Engine implements query.Engine.
var _ query.Engine = (*Engine)(nil)
var _ query.TextEngine = (*Engine)(nil)
var _ query.MessageBodySearcher = (*Engine)(nil)

// NewEngine creates a new daemon-backed query engine.
func NewEngine(cfg Config) (*Engine, error) {
	c, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return &Engine{store: c}, nil
}

// NewEngineAdapter creates a query engine from an existing daemon client.
func NewEngineAdapter(c *Client) *Engine {
	return &Engine{store: c}
}

// IsRemote returns true because this engine is backed by HTTP rather than a
// direct local database handle.
func (e *Engine) IsRemote() bool {
	return true
}

// Close releases resources held by the engine.
func (e *Engine) Close() error {
	if e == nil || e.store == nil {
		return nil
	}
	return e.store.Close()
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
		return apiValueLabels
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
		return apiValueCount
	case query.SortBySize:
		return "size"
	case query.SortByAttachmentSize:
		return "attachment_size"
	case query.SortByName:
		return "name"
	default:
		return apiValueCount
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

func textViewTypeToString(v query.TextViewType) string {
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
		return apiValueLabels
	case query.TextViewTime:
		return "time"
	default:
		return "contacts"
	}
}

func textSortFieldToString(f query.TextSortField) string {
	switch f {
	case query.TextSortByCount:
		return apiValueCount
	case query.TextSortByName:
		return "name"
	default:
		return "last_message"
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
		return apiValueSubject
	default:
		return "date"
	}
}

func aggregateRowsFromGenerated(resp *generated.AggregateResponse) []query.AggregateRow {
	if resp == nil {
		return nil
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
	return rows
}

func optionalTimeRFC3339(value *time.Time) *string {
	if value == nil {
		return nil
	}
	out := value.Format(time.RFC3339)
	return &out
}

func optionalStatsGroupBy(value query.ViewType) *string {
	if value == 0 {
		return nil
	}
	return optionalString(viewTypeToString(value))
}

func emptyValueTargetsString(filter query.MessageFilter) *string {
	if !filter.HasEmptyTargets() {
		return nil
	}
	var targets []string
	for vt, active := range filter.EmptyValueTargets {
		if active {
			targets = append(targets, viewTypeToString(vt))
		}
	}
	if len(targets) == 0 {
		return nil
	}
	out := strings.Join(targets, ",")
	return &out
}

func generatedFilterMessagesQuery(filter query.MessageFilter, paginated bool) generated.FilterMessagesQuery {
	out := generated.FilterMessagesQuery{
		Sender:          optionalString(filter.Sender),
		SenderName:      optionalString(filter.SenderName),
		Recipient:       optionalString(filter.Recipient),
		RecipientName:   optionalString(filter.RecipientName),
		Domain:          optionalString(filter.Domain),
		Label:           optionalString(filter.Label),
		MessageType:     optionalString(filter.MessageType),
		TimePeriod:      optionalString(filter.TimeRange.Period),
		TimeGranularity: optionalString(timeGranularityToString(filter.TimeRange.Granularity)),
		ConversationID:  filter.ConversationID,
		SourceID:        filter.SourceID,
		AttachmentsOnly: optionalBool(filter.WithAttachmentsOnly),
		HideDeleted:     optionalBool(filter.HideDeletedFromSource),
		After:           optionalTimeRFC3339(filter.After),
		Before:          optionalTimeRFC3339(filter.Before),
		EmptyTargets:    emptyValueTargetsString(filter),
	}
	if paginated {
		out.Offset = optionalPositiveInt64(filter.Pagination.Offset)
		out.Limit = optionalPositiveInt64(filter.Pagination.Limit)
		out.Sort = optionalString(messageSortFieldToString(filter.Sorting.Field))
		out.Direction = optionalString(sortDirectionToString(filter.Sorting.Direction))
	}
	return out
}

func gmailIDsFilterQuery(filter query.MessageFilter) *generated.GetGmailIDsByFilterQuery {
	base := generatedFilterMessagesQuery(filter, false)
	out := generated.GetGmailIDsByFilterQuery(base)
	out.Limit = optionalPositiveInt64(filter.Pagination.Limit)
	return &out
}

func filterMessagesQuery(filter query.MessageFilter) *generated.FilterMessagesQuery {
	out := generatedFilterMessagesQuery(filter, true)
	return &out
}

func textConversationsQuery(filter query.TextFilter) *generated.ListTextConversationsQuery {
	return &generated.ListTextConversationsQuery{
		SourceID:        copyInt64(filter.SourceID),
		ContactPhone:    optionalString(filter.ContactPhone),
		ContactName:     optionalString(filter.ContactName),
		SourceType:      optionalString(filter.SourceType),
		Label:           optionalString(filter.Label),
		TimePeriod:      optionalString(filter.TimeRange.Period),
		TimeGranularity: optionalString(timeGranularityToString(filter.TimeRange.Granularity)),
		After:           optionalTimeRFC3339(filter.After),
		Before:          optionalTimeRFC3339(filter.Before),
		Offset:          optionalPositiveInt64(filter.Pagination.Offset),
		Limit:           optionalPositiveInt64(filter.Pagination.Limit),
		Sort:            optionalString(textSortFieldToString(filter.SortField)),
		Direction:       optionalString(sortDirectionToString(filter.SortDirection)),
	}
}

func textConversationMessagesQuery(filter query.TextFilter) *generated.ListTextConversationMessagesQuery {
	base := textConversationsQuery(filter)
	return &generated.ListTextConversationMessagesQuery{
		SourceID:        base.SourceID,
		ContactPhone:    base.ContactPhone,
		ContactName:     base.ContactName,
		SourceType:      base.SourceType,
		Label:           base.Label,
		TimePeriod:      base.TimePeriod,
		TimeGranularity: base.TimeGranularity,
		After:           base.After,
		Before:          base.Before,
		Offset:          base.Offset,
		Limit:           base.Limit,
		Sort:            base.Sort,
		Direction:       base.Direction,
	}
}

func textAggregateQuery(viewType query.TextViewType, opts query.TextAggregateOptions) *generated.GetTextAggregatesQuery {
	return &generated.GetTextAggregatesQuery{
		ViewType:        optionalString(textViewTypeToString(viewType)),
		Sort:            optionalString(textSortFieldToString(opts.SortField)),
		Direction:       optionalString(sortDirectionToString(opts.SortDirection)),
		Limit:           optionalPositiveInt64(opts.Limit),
		TimeGranularity: optionalTextAggregateTimeGranularity(opts),
		SourceID:        copyInt64(opts.SourceID),
		SearchQuery:     optionalString(opts.SearchQuery),
		After:           optionalTimeRFC3339(opts.After),
		Before:          optionalTimeRFC3339(opts.Before),
	}
}

func optionalTextAggregateTimeGranularity(opts query.TextAggregateOptions) *string {
	if !opts.HasTimeGranularity() {
		return nil
	}
	return optionalString(timeGranularityToString(opts.TimeGranularity))
}

func fastSearchQuery(queryStr string, filter query.MessageFilter, statsGroupBy query.ViewType, limit, offset int) *generated.FastSearchQuery {
	fields := generatedFilterMessagesQuery(filter, false)
	return &generated.FastSearchQuery{
		Q:               queryStr,
		ViewType:        optionalString(viewTypeToString(statsGroupBy)),
		Sender:          fields.Sender,
		SenderName:      fields.SenderName,
		Recipient:       fields.Recipient,
		RecipientName:   fields.RecipientName,
		Domain:          fields.Domain,
		Label:           fields.Label,
		TimePeriod:      fields.TimePeriod,
		TimeGranularity: fields.TimeGranularity,
		ConversationID:  fields.ConversationID,
		SourceID:        fields.SourceID,
		AttachmentsOnly: fields.AttachmentsOnly,
		HideDeleted:     fields.HideDeleted,
		After:           fields.After,
		Before:          fields.Before,
		EmptyTargets:    fields.EmptyTargets,
		Offset:          int64FromInt(offset),
		Limit:           int64FromInt(limit),
		Sort:            optionalString(messageSortFieldToString(filter.Sorting.Field)),
		Direction:       optionalString(sortDirectionToString(filter.Sorting.Direction)),
	}
}

func fastSearchScopedQueryString(q *search.Query, queryStr string, filter query.MessageFilter) (string, bool) {
	if filter.MessageType == "" {
		return queryStr, false
	}

	var queryTypes []string
	if q != nil {
		queryTypes = q.MessageTypes
	}
	messageTypes, noMatches := query.ScopedMessageTypes(queryTypes, filter.MessageType)
	if noMatches {
		return "", true
	}
	if q == nil {
		if queryStr == "" {
			return "message_type:" + filter.MessageType, false
		}
		return strings.TrimSpace("message_type:" + filter.MessageType + " " + queryStr), false
	}

	scoped := *q
	scoped.MessageTypes = messageTypes
	return buildSearchQueryString(&scoped), false
}

func deepSearchQuery(queryStr string, q *search.Query, limit, offset int) (*generated.DeepSearchQuery, error) {
	out := &generated.DeepSearchQuery{
		Q:      queryStr,
		Offset: int64FromInt(offset),
		Limit:  int64FromInt(limit),
	}

	// Forward filter-only fields that can't be represented in the
	// query string syntax (AccountIDs, HideDeleted). The daemon API
	// accepts a single source_id; CLI/MCP layers reject collection
	// scope for this query path, but defend here against any future caller
	// that bypasses those checks rather than silently dropping IDs.
	if len(q.AccountIDs) > 1 {
		return nil, fmt.Errorf(
			"daemon search does not support multi-account scope; "+
				"got %d account IDs", len(q.AccountIDs))
	}
	if len(q.AccountIDs) == 1 {
		out.SourceID = &q.AccountIDs[0]
	}
	out.HideDeleted = optionalBool(q.HideDeleted)

	return out, nil
}

func int64FromInt(value int) *int64 {
	out := int64(value)
	return &out
}

func messageSummariesFromGenerated(msgs []generated.MessageSummary) []query.MessageSummary {
	if msgs == nil {
		return nil
	}
	result := make([]query.MessageSummary, len(msgs))
	for i, message := range msgs {
		result[i] = querySummaryFromAPIMessage(generatedMessageToAPIMessage(message))
	}
	return result
}

func bodySearchSummariesFromGenerated(
	msgs []generated.MessageSummary,
	contexts []generated.BodySearchContext,
) ([]query.MessageSummary, error) {
	result := messageSummariesFromGenerated(msgs)
	byID := make(map[int64]int, len(result))
	for i, message := range result {
		if _, duplicate := byID[message.ID]; duplicate {
			return nil, fmt.Errorf("daemon returned duplicate body-search message %d", message.ID)
		}
		byID[message.ID] = i
	}
	seen := make(map[int64]struct{}, len(contexts))
	for _, bodyContext := range contexts {
		index, ok := byID[bodyContext.MessageID]
		if !ok {
			return nil, fmt.Errorf("daemon returned body context for unknown message %d", bodyContext.MessageID)
		}
		if _, duplicate := seen[bodyContext.MessageID]; duplicate {
			return nil, fmt.Errorf("daemon returned duplicate body context for message %d", bodyContext.MessageID)
		}
		seen[bodyContext.MessageID] = struct{}{}
		result[index].BodyContextSnippets = bodyContext.ContextSnippets
		truncated := false
		if bodyContext.ContextSnippetsTruncated != nil {
			truncated = *bodyContext.ContextSnippetsTruncated
		}
		if len(bodyContext.ContextSnippets) == 0 && !truncated {
			return nil, fmt.Errorf("daemon returned empty untruncated body context for message %d", bodyContext.MessageID)
		}
		result[index].BodyContextSnippetsTruncated = truncated
	}
	for _, message := range result {
		if _, ok := seen[message.ID]; !ok {
			return nil, fmt.Errorf("daemon omitted body context for message %d", message.ID)
		}
	}
	return result, nil
}

func queryAttachmentFromGenerated(att *generated.AttachmentInfo) *query.AttachmentInfo {
	if att == nil {
		return nil
	}
	return &query.AttachmentInfo{
		ID:          att.ID,
		Filename:    att.Filename,
		MimeType:    att.MimeType,
		Size:        att.SizeBytes,
		ContentHash: stringValue(att.ContentHash),
		URL:         stringValue(att.URL),
	}
}

func querySummaryFromAPIMessage(msg store.APIMessage) query.MessageSummary {
	fromEmail := msg.FromEmail
	if fromEmail == "" {
		fromEmail = msg.From
	}
	return query.MessageSummary{
		ID:                   msg.ID,
		SourceMessageID:      msg.SourceMessageID,
		ConversationID:       msg.ConversationID,
		SourceConversationID: msg.SourceConversationID,
		Subject:              msg.Subject,
		MessageType:          msg.MessageType,
		FromEmail:            fromEmail,
		FromName:             msg.FromName,
		FromPhone:            msg.FromPhone,
		To:                   apiMessageAddresses(msg.To),
		Cc:                   apiMessageAddresses(msg.Cc),
		Bcc:                  apiMessageAddresses(msg.Bcc),
		SentAt:               msg.SentAt,
		DeletedAt:            msg.DeletedAt,
		Snippet:              msg.Snippet,
		Labels:               msg.Labels,
		HasAttachments:       msg.HasAttachments,
		SizeEstimate:         msg.SizeEstimate,
	}
}

func totalStatsFromGenerated(resp *generated.TotalStatsResponse) *query.TotalStats {
	if resp == nil {
		return nil
	}
	return &query.TotalStats{
		MessageCount:              resp.MessageCount,
		ActiveMessageCount:        resp.ActiveMessages,
		SourceDeletedMessageCount: resp.SourceDeletedMessages,
		TotalSize:                 resp.TotalSize,
		AttachmentCount:           resp.AttachmentCount,
		AttachmentSize:            resp.AttachmentSize,
		LabelCount:                resp.LabelCount,
		AccountCount:              resp.AccountCount,
	}
}

func textConversationRowsFromGenerated(rows []generated.TextConversationRow) []query.ConversationRow {
	if rows == nil {
		return nil
	}
	out := make([]query.ConversationRow, len(rows))
	for i, row := range rows {
		var lastMessageAt time.Time
		if row.LastMessageAt != nil {
			lastMessageAt = parseTime(*row.LastMessageAt)
		}
		out[i] = query.ConversationRow{
			ConversationID:   row.ConversationID,
			Title:            row.Title,
			SourceType:       row.SourceType,
			MessageCount:     row.MessageCount,
			ParticipantCount: row.ParticipantCount,
			LastMessageAt:    lastMessageAt,
			LastPreview:      row.LastPreview,
		}
	}
	return out
}

func queryMessageSummariesFromCLIGenerated(msgs []generated.CLIQueryMessageSummary) []query.MessageSummary {
	if msgs == nil {
		return nil
	}
	out := make([]query.MessageSummary, len(msgs))
	for i, msg := range msgs {
		out[i] = queryMessageSummaryFromGenerated(msg)
	}
	return out
}

// ============================================================================
// Engine Interface Implementation
// ============================================================================

// Aggregate performs grouping based on the provided ViewType.
func (e *Engine) Aggregate(ctx context.Context, groupBy query.ViewType, opts query.AggregateOptions) ([]query.AggregateRow, error) {
	resp, err := APIResponse(e.store, func(client *apiclient.Client) (*generated.GetAggregatesResp, error) {
		return client.GetAggregatesWithResponse(ctx, &generated.GetAggregatesRequestOptions{
			Query: &generated.GetAggregatesQuery{
				ViewType:        optionalString(viewTypeToString(groupBy)),
				Sort:            optionalString(sortFieldToString(opts.SortField)),
				Direction:       optionalString(sortDirectionToString(opts.SortDirection)),
				Limit:           optionalPositiveInt64(opts.Limit),
				TimeGranularity: optionalString(timeGranularityToString(opts.TimeGranularity)),
				SourceID:        opts.SourceID,
				AttachmentsOnly: optionalBool(opts.WithAttachmentsOnly),
				HideDeleted:     optionalBool(opts.HideDeletedFromSource),
				SearchQuery:     optionalString(opts.SearchQuery),
				After:           optionalTimeRFC3339(opts.After),
				Before:          optionalTimeRFC3339(opts.Before),
			},
		})
	})
	if err != nil {
		return nil, err
	}

	return aggregateRowsFromGenerated(resp.JSON200), nil
}

// SubAggregate performs aggregation on a filtered subset of messages.
func (e *Engine) SubAggregate(ctx context.Context, filter query.MessageFilter, groupBy query.ViewType, opts query.AggregateOptions) ([]query.AggregateRow, error) {
	limit := optionalPositiveInt64(filter.Pagination.Limit)
	if opts.Limit > 0 {
		limit = optionalPositiveInt64(opts.Limit)
	}
	resp, err := APIResponse(e.store, func(client *apiclient.Client) (*generated.GetSubAggregatesResp, error) {
		return client.GetSubAggregatesWithResponse(ctx, &generated.GetSubAggregatesRequestOptions{
			Query: &generated.GetSubAggregatesQuery{
				ViewType:        viewTypeToString(groupBy),
				Sender:          optionalString(filter.Sender),
				SenderName:      optionalString(filter.SenderName),
				Recipient:       optionalString(filter.Recipient),
				RecipientName:   optionalString(filter.RecipientName),
				Domain:          optionalString(filter.Domain),
				Label:           optionalString(filter.Label),
				MessageType:     optionalString(filter.MessageType),
				TimePeriod:      optionalString(filter.TimeRange.Period),
				TimeGranularity: optionalString(timeGranularityToString(opts.TimeGranularity)),
				ConversationID:  filter.ConversationID,
				SourceID:        filter.SourceID,
				AttachmentsOnly: optionalBool(filter.WithAttachmentsOnly),
				HideDeleted:     optionalBool(filter.HideDeletedFromSource),
				After:           optionalTimeRFC3339(filter.After),
				Before:          optionalTimeRFC3339(filter.Before),
				EmptyTargets:    emptyValueTargetsString(filter),
				Offset:          optionalPositiveInt64(filter.Pagination.Offset),
				Limit:           limit,
				Sort:            optionalString(sortFieldToString(opts.SortField)),
				Direction:       optionalString(sortDirectionToString(opts.SortDirection)),
				SearchQuery:     optionalString(opts.SearchQuery),
			},
		})
	})
	if err != nil {
		return nil, err
	}

	return aggregateRowsFromGenerated(resp.JSON200), nil
}

// ListMessages returns messages matching the filter criteria.
func (e *Engine) ListMessages(ctx context.Context, filter query.MessageFilter) ([]query.MessageSummary, error) {
	resp, err := APIResponse(e.store, func(client *apiclient.Client) (*generated.FilterMessagesResp, error) {
		return client.FilterMessagesWithResponse(ctx, &generated.FilterMessagesRequestOptions{
			Query: filterMessagesQuery(filter),
		})
	})
	if err != nil {
		return nil, err
	}
	return messageSummariesFromGenerated(resp.JSON200.Messages), nil
}

// GetMessage returns a single message by ID.
func (e *Engine) GetMessage(ctx context.Context, id int64) (*query.MessageDetail, error) {
	msg, err := e.store.GetMessage(id)
	if errors.Is(err, store.ErrMessageNotFound) {
		return nil, nil //nolint:nilnil // engine API uses (nil, nil) for not-found
	}
	if err != nil {
		return nil, err
	}

	return queryDetailFromAPIMessage(msg), nil
}

func queryDetailFromAPIMessage(msg *store.APIMessage) *query.MessageDetail {
	if msg == nil {
		return nil
	}
	detail := &query.MessageDetail{
		ID:                   msg.ID,
		SourceMessageID:      msg.SourceMessageID,
		ConversationID:       msg.ConversationID,
		SourceConversationID: msg.SourceConversationID,
		Subject:              msg.Subject,
		MessageType:          msg.MessageType,
		Snippet:              msg.Snippet,
		SentAt:               msg.SentAt,
		DeletedAt:            msg.DeletedAt,
		SizeEstimate:         msg.SizeEstimate,
		HasAttachments:       msg.HasAttachments,
		Labels:               msg.Labels,
		BodyText:             msg.Body,
		From:                 apiMessageFromAddress(msg),
		To:                   apiMessageAddresses(msg.To),
		Cc:                   apiMessageAddresses(msg.Cc),
		Bcc:                  apiMessageAddresses(msg.Bcc),
	}
	for _, att := range msg.Attachments {
		detail.Attachments = append(detail.Attachments, query.AttachmentInfo{
			ID:          att.ID,
			Filename:    att.Filename,
			MimeType:    att.MimeType,
			Size:        att.Size,
			ContentHash: att.ContentHash,
			URL:         att.URL,
		})
	}
	if len(detail.Attachments) > 0 {
		detail.HasAttachments = true
	}
	return detail
}

func apiMessageFromAddress(msg *store.APIMessage) []query.Address {
	identifier := msg.FromEmail
	if identifier == "" {
		identifier = msg.FromPhone
	}
	if identifier == "" {
		identifier = msg.From
	}
	if identifier == "" && msg.FromName == "" {
		return nil
	}
	return []query.Address{{Email: identifier, Name: msg.FromName}}
}

func apiMessageAddresses(values []string) []query.Address {
	if values == nil {
		return nil
	}
	out := make([]query.Address, len(values))
	for i, value := range values {
		out[i] = query.Address{Email: value}
	}
	return out
}

// GetMessageBySourceID returns a message by its source message ID.
// This operation is not supported through the daemon API.
func (e *Engine) GetMessageBySourceID(ctx context.Context, sourceMessageID string) (*query.MessageDetail, error) {
	return nil, ErrNotSupported
}

// GetMessageSummariesByIDs loops GetMessage over the supplied IDs.
// The daemon API pays one HTTP round-trip per id; a future endpoint
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
		// Carry sender details from the first From address so daemon-backed
		// MCP search_messages/find_similar_messages responses don't
		// silently drop who sent each hit. FromPhone is omitted — the
		// daemon API does not expose it today; callers that need
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
func (e *Engine) GetMessageRaw(ctx context.Context, id int64) ([]byte, error) {
	raw, _, err := e.store.GetCLIMessageRaw(ctx, strconv.FormatInt(id, 10))
	return raw, err
}

// GetAttachment returns attachment metadata by ID.
func (e *Engine) GetAttachment(ctx context.Context, id int64) (*query.AttachmentInfo, error) {
	resp, err := APIResponseWithNotFound(
		e.store,
		func(client *apiclient.Client) (*generated.GetAttachmentResp, error) {
			return client.GetAttachmentWithResponse(ctx, &generated.GetAttachmentRequestOptions{
				PathParams: &generated.GetAttachmentPath{ID: id},
			})
		},
		func(*generated.GetAttachmentResp) error {
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.JSON200 == nil {
		return nil, nil //nolint:nilnil // query.Engine.GetAttachment uses nil,nil for not found
	}
	return queryAttachmentFromGenerated(resp.JSON200), nil
}

// GetAttachmentsByHash is not exposed through the daemon query adapter. Raw
// attachment downloads use the daemon client's dedicated binary store path.
func (e *Engine) GetAttachmentsByHash(context.Context, string) ([]query.AttachmentInfo, error) {
	return nil, ErrNotSupported
}

// Search performs full-text search including message body.
func (e *Engine) Search(ctx context.Context, q *search.Query, limit, offset int) ([]query.MessageSummary, error) {
	// Build query string from search.Query
	queryStr := buildSearchQueryString(q)
	if queryStr == "" {
		return nil, nil
	}

	queryParams, err := deepSearchQuery(queryStr, q, limit, offset)
	if err != nil {
		return nil, err
	}

	resp, err := APIResponse(e.store, func(client *apiclient.Client) (*generated.DeepSearchResp, error) {
		return client.DeepSearchWithResponse(ctx, &generated.DeepSearchRequestOptions{
			Query: queryParams,
		})
	})
	if err != nil {
		return nil, err
	}
	return messageSummariesFromGenerated(resp.JSON200.Messages), nil
}

// SearchMessageBodies requests the daemon's exact body-only search scope and
// requires the response to echo that scope. Requiring the echo fails closed
// against older daemons that ignore the additive query parameter and would
// otherwise return generic composite-search false positives.
func (e *Engine) SearchMessageBodies(ctx context.Context, q *search.Query, limit, offset int) ([]query.MessageSummary, error) {
	if q == nil || len(q.TextTerms) == 0 {
		return nil, errors.New("message body search requires at least one free-text term")
	}
	queryStr := buildSearchQueryString(q)
	queryParams, err := deepSearchQuery(queryStr, q, limit, offset)
	if err != nil {
		return nil, err
	}
	queryParams.Scope = optionalString("body")

	resp, err := APIResponse(e.store, func(client *apiclient.Client) (*generated.DeepSearchResp, error) {
		return client.DeepSearchWithResponse(ctx, &generated.DeepSearchRequestOptions{Query: queryParams})
	})
	if err != nil {
		return nil, err
	}
	if resp.JSON200.Scope == nil || *resp.JSON200.Scope != "body" {
		return nil, errors.New("daemon did not confirm body-only search scope; upgrade the daemon to API schema 1.3.0 or newer")
	}
	return bodySearchSummariesFromGenerated(resp.JSON200.Messages, resp.JSON200.BodyContexts)
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
	scopedQueryStr, noMatches := fastSearchScopedQueryString(q, queryStr, filter)
	if noMatches {
		return &query.SearchFastResult{Stats: &query.TotalStats{}}, nil
	}

	resp, err := APIResponse(e.store, func(client *apiclient.Client) (*generated.FastSearchResp, error) {
		return client.FastSearchWithResponse(ctx, &generated.FastSearchRequestOptions{
			Query: fastSearchQuery(scopedQueryStr, filter, statsGroupBy, limit, offset),
		})
	})
	if err != nil {
		return nil, err
	}
	return &query.SearchFastResult{
		Messages:   messageSummariesFromGenerated(resp.JSON200.Messages),
		TotalCount: resp.JSON200.TotalCount,
		Stats:      totalStatsFromGenerated(resp.JSON200.Stats),
	}, nil
}

func (e *Engine) GetGmailIDsByFilter(ctx context.Context, filter query.MessageFilter) ([]string, error) {
	resp, err := APIResponse(e.store, func(client *apiclient.Client) (*generated.GetGmailIDsByFilterResp, error) {
		return client.GetGmailIDsByFilterWithResponse(ctx, &generated.GetGmailIDsByFilterRequestOptions{
			Query: gmailIDsFilterQuery(filter),
		})
	})
	if err != nil {
		return nil, err
	}
	if resp.JSON200.GmailIds == nil {
		return []string{}, nil
	}
	return resp.JSON200.GmailIds, nil
}

func (e *Engine) SearchByDomains(ctx context.Context, domains []string, after, before *time.Time, limit, offset int) ([]query.MessageSummary, error) {
	resp, err := APIResponse(e.store, func(client *apiclient.Client) (*generated.SearchMessagesByDomainsResp, error) {
		return client.SearchMessagesByDomainsWithResponse(ctx, &generated.SearchMessagesByDomainsRequestOptions{
			Query: &generated.SearchMessagesByDomainsQuery{
				Domains: strings.Join(domains, ","),
				After:   optionalTimeRFC3339(after),
				Before:  optionalTimeRFC3339(before),
				Limit:   optionalPositiveInt64(limit),
				Offset:  optionalPositiveInt64(offset),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, nil
	}
	return messageSummariesFromGenerated(resp.JSON200.Messages), nil
}

// ListAccounts returns all archive source accounts.
func (e *Engine) ListAccounts(ctx context.Context) ([]query.AccountInfo, error) {
	accounts, err := e.store.GetCLIAccounts(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]query.AccountInfo, len(accounts))
	for i, acc := range accounts {
		result[i] = query.AccountInfo{
			ID:          acc.ID,
			SourceType:  acc.Type,
			Identifier:  acc.Email,
			DisplayName: acc.DisplayName,
		}
	}
	return result, nil
}

func (e *Engine) ListConversations(ctx context.Context, filter query.TextFilter) ([]query.ConversationRow, error) {
	resp, err := APIResponse(e.store, func(client *apiclient.Client) (*generated.ListTextConversationsResp, error) {
		return client.ListTextConversationsWithResponse(ctx, &generated.ListTextConversationsRequestOptions{
			Query: textConversationsQuery(filter),
		})
	})
	if err != nil {
		return nil, err
	}
	return textConversationRowsFromGenerated(resp.JSON200.Conversations), nil
}

func (e *Engine) TextAggregate(ctx context.Context, viewType query.TextViewType, opts query.TextAggregateOptions) ([]query.AggregateRow, error) {
	resp, err := APIResponse(e.store, func(client *apiclient.Client) (*generated.GetTextAggregatesResp, error) {
		return client.GetTextAggregatesWithResponse(ctx, &generated.GetTextAggregatesRequestOptions{
			Query: textAggregateQuery(viewType, opts),
		})
	})
	if err != nil {
		return nil, err
	}
	return aggregateRowsFromGenerated(resp.JSON200), nil
}

func (e *Engine) ListConversationMessages(ctx context.Context, convID int64, filter query.TextFilter) ([]query.MessageSummary, error) {
	resp, err := APIResponse(e.store, func(client *apiclient.Client) (*generated.ListTextConversationMessagesResp, error) {
		return client.ListTextConversationMessagesWithResponse(ctx, &generated.ListTextConversationMessagesRequestOptions{
			PathParams: &generated.ListTextConversationMessagesPath{ID: convID},
			Query:      textConversationMessagesQuery(filter),
		})
	})
	if err != nil {
		return nil, err
	}
	return queryMessageSummariesFromCLIGenerated(resp.JSON200.Messages), nil
}

func (e *Engine) TextSearch(ctx context.Context, queryStr string, limit, offset int) ([]query.MessageSummary, error) {
	resp, err := APIResponse(e.store, func(client *apiclient.Client) (*generated.SearchTextMessagesResp, error) {
		return client.SearchTextMessagesWithResponse(ctx, &generated.SearchTextMessagesRequestOptions{
			Query: &generated.SearchTextMessagesQuery{
				Q:      queryStr,
				Limit:  optionalPositiveInt64(limit),
				Offset: optionalPositiveInt64(offset),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return queryMessageSummariesFromCLIGenerated(resp.JSON200.Messages), nil
}

func (e *Engine) GetTextStats(ctx context.Context, opts query.TextStatsOptions) (*query.TotalStats, error) {
	resp, err := APIResponse(e.store, func(client *apiclient.Client) (*generated.GetTextStatsResp, error) {
		return client.GetTextStatsWithResponse(ctx, &generated.GetTextStatsRequestOptions{
			Query: &generated.GetTextStatsQuery{
				SourceID:    copyInt64(opts.SourceID),
				SearchQuery: optionalString(opts.SearchQuery),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return totalStatsFromGenerated(resp.JSON200), nil
}

// GetTotalStats returns overall database statistics.
func (e *Engine) GetTotalStats(ctx context.Context, opts query.StatsOptions) (*query.TotalStats, error) {
	resp, err := APIResponse(e.store, func(client *apiclient.Client) (*generated.GetTotalStatsResp, error) {
		return client.GetTotalStatsWithResponse(ctx, &generated.GetTotalStatsRequestOptions{
			Query: &generated.GetTotalStatsQuery{
				SourceID:        opts.SourceID,
				AttachmentsOnly: optionalBool(opts.WithAttachmentsOnly),
				HideDeleted:     optionalBool(opts.HideDeletedFromSource),
				SearchQuery:     optionalString(opts.SearchQuery),
				GroupBy:         optionalStatsGroupBy(opts.GroupBy),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return totalStatsFromGenerated(resp.JSON200), nil
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
	for _, typ := range q.MessageTypes {
		parts = append(parts, "message_type:"+typ)
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

	var result strings.Builder
	for i, part := range parts {
		if i > 0 {
			result.WriteString(" ")
		}
		result.WriteString(part)
	}
	return result.String()
}
