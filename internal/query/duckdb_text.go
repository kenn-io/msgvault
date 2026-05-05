package query

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/wesm/msgvault/internal/store"
)

// Compile-time interface assertion.
var _ TextEngine = (*DuckDBEngine)(nil)

// textTypeFilter returns a SQL condition restricting to text message types.
func textTypeFilter() string {
	return "msg.message_type IN ('whatsapp','imessage','sms','google_voice_text')"
}

// buildTextFilterConditions builds WHERE conditions from a TextFilter.
// All conditions use the msg. prefix and assume the standard parquetCTEs.
func (e *DuckDBEngine) buildTextFilterConditions(
	filter TextFilter,
) (string, []interface{}) {
	conditions := []string{textTypeFilter()}
	var args []interface{}

	if filter.SourceID != nil {
		conditions = append(conditions, "msg.source_id = ?")
		args = append(args, *filter.SourceID)
	}
	if filter.ContactPhone != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM p p_filter
			WHERE p_filter.id = COALESCE(msg.sender_id,
				(SELECT mr_fb.participant_id FROM mr mr_fb
				 WHERE mr_fb.message_id = msg.id AND mr_fb.recipient_type = 'from'
				 LIMIT 1))
			  AND COALESCE(
				NULLIF(p_filter.phone_number, ''),
				p_filter.email_address
			  ) = ?
		)`)
		args = append(args, filter.ContactPhone)
	}
	if filter.ContactName != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM p p_filter
			WHERE p_filter.id = COALESCE(msg.sender_id,
				(SELECT mr_fb.participant_id FROM mr mr_fb
				 WHERE mr_fb.message_id = msg.id AND mr_fb.recipient_type = 'from'
				 LIMIT 1))
			  AND COALESCE(
				NULLIF(TRIM(p_filter.display_name), ''),
				NULLIF(p_filter.phone_number, ''),
				p_filter.email_address
			  ) = ?
		)`)
		args = append(args, filter.ContactName)
	}
	if filter.SourceType != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM src
			WHERE src.id = msg.source_id AND src.source_type = ?
		)`)
		args = append(args, filter.SourceType)
	}
	if filter.Label != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM ml
			JOIN lbl ON lbl.id = ml.label_id
			WHERE ml.message_id = msg.id
			  AND lbl.name ILIKE ? ESCAPE '\'
		)`)
		args = append(args, escapeILIKE(filter.Label))
	}
	if filter.TimeRange.Period != "" {
		g := inferTimeGranularity(
			filter.TimeRange.Granularity, filter.TimeRange.Period,
		)
		conditions = append(conditions,
			fmt.Sprintf("%s = ?", timeExpr(g)))
		args = append(args, filter.TimeRange.Period)
	}
	if filter.After != nil {
		conditions = append(conditions,
			"msg.sent_at >= CAST(? AS TIMESTAMP)")
		args = append(args,
			filter.After.Format("2006-01-02 15:04:05"))
	}
	if filter.Before != nil {
		conditions = append(conditions,
			"msg.sent_at < CAST(? AS TIMESTAMP)")
		args = append(args,
			filter.Before.Format("2006-01-02 15:04:05"))
	}

	return strings.Join(conditions, " AND "), args
}

// ListConversations returns conversations matching the filter,
// aggregating stats from the messages Parquet table.
func (e *DuckDBEngine) ListConversations(
	ctx context.Context, filter TextFilter,
) ([]ConversationRow, error) {
	where, args := e.buildTextFilterConditions(filter)

	// Sort clause.
	var orderBy string
	switch filter.SortField {
	case TextSortByCount:
		orderBy = "message_count"
	case TextSortByName:
		orderBy = "title"
	default: // TextSortByLastMessage
		orderBy = "last_message_at"
	}
	if filter.SortDirection == SortAsc {
		orderBy += " ASC"
	} else {
		orderBy += " DESC"
	}

	limit := filter.Pagination.Limit
	if limit == 0 {
		limit = 100
	}

	query := fmt.Sprintf(`
		WITH %s,
		conv_stats AS (
			SELECT
				msg.conversation_id,
				COUNT(*) AS message_count,
				-- TODO: use conversation_participants table once exported to Parquet
				COUNT(DISTINCT COALESCE(msg.sender_id, 0)) AS participant_count,
				MAX(msg.sent_at) AS last_message_at,
				COALESCE(SUM(CAST(msg.size_estimate AS BIGINT)), 0) AS total_size,
				FIRST(msg.snippet ORDER BY msg.sent_at DESC, msg.id DESC) AS last_preview,
				FIRST(msg.source_id) AS source_id
			FROM msg
			WHERE %s
			GROUP BY msg.conversation_id
		)
		SELECT
			conv.id,
			COALESCE(conv.title, '') AS title,
			COALESCE(src.source_type, '') AS source_type,
			cs.message_count,
			cs.participant_count,
			cs.last_message_at,
			COALESCE(cs.last_preview, '') AS last_preview
		FROM conv_stats cs
		JOIN conv ON conv.id = cs.conversation_id
		LEFT JOIN src ON src.id = cs.source_id
		ORDER BY %s
		LIMIT ? OFFSET ?
	`, e.parquetCTEs(), where, orderBy)

	args = append(args, limit, filter.Pagination.Offset)

	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []ConversationRow
	for rows.Next() {
		var row ConversationRow
		var lastAt sql.NullTime
		if err := rows.Scan(
			&row.ConversationID,
			&row.Title,
			&row.SourceType,
			&row.MessageCount,
			&row.ParticipantCount,
			&lastAt,
			&row.LastPreview,
		); err != nil {
			return nil, fmt.Errorf("scan conversation: %w", err)
		}
		if lastAt.Valid {
			row.LastMessageAt = lastAt.Time
		}
		results = append(results, row)
	}
	return results, rows.Err()
}

// textAggViewDef returns the aggregate query definition for a text view type.
func textAggViewDef(
	view TextViewType, granularity TimeGranularity,
) (aggViewDef, error) {
	switch view {
	case TextViewContacts:
		keyExpr := "COALESCE(NULLIF(p_sender.phone_number, ''), " +
			"p_sender.email_address)"
		senderJoin := `JOIN p p_sender ON p_sender.id = COALESCE(msg.sender_id,
			(SELECT mr_fb.participant_id FROM mr mr_fb
			 WHERE mr_fb.message_id = msg.id AND mr_fb.recipient_type = 'from'
			 LIMIT 1))`
		return aggViewDef{
			keyExpr:    keyExpr,
			joinClause: senderJoin,
			nullGuard:  keyExpr + " IS NOT NULL",
		}, nil
	case TextViewContactNames:
		nameExpr := "COALESCE(NULLIF(TRIM(p_sender.display_name), ''), " +
			"NULLIF(p_sender.phone_number, ''), p_sender.email_address)"
		senderJoin := `JOIN p p_sender ON p_sender.id = COALESCE(msg.sender_id,
			(SELECT mr_fb.participant_id FROM mr mr_fb
			 WHERE mr_fb.message_id = msg.id AND mr_fb.recipient_type = 'from'
			 LIMIT 1))`
		return aggViewDef{
			keyExpr:    nameExpr,
			joinClause: senderJoin,
			nullGuard:  nameExpr + " IS NOT NULL",
		}, nil
	case TextViewSources:
		return aggViewDef{
			keyExpr:    "src.source_type",
			joinClause: "JOIN src ON src.id = msg.source_id",
			nullGuard:  "src.source_type IS NOT NULL",
		}, nil
	case TextViewLabels:
		return aggViewDef{
			keyExpr: "lbl.name",
			joinClause: `JOIN ml ON ml.message_id = msg.id
				JOIN lbl ON lbl.id = ml.label_id`,
			nullGuard:  "lbl.name IS NOT NULL",
			keyColumns: []string{"lbl.name"},
		}, nil
	case TextViewTime:
		return aggViewDef{
			keyExpr:   timeExpr(granularity),
			nullGuard: "msg.sent_at IS NOT NULL",
		}, nil
	default:
		return aggViewDef{},
			fmt.Errorf("unsupported text view type: %v", view)
	}
}

// TextAggregate aggregates text messages by the given view type.
func (e *DuckDBEngine) TextAggregate(
	ctx context.Context,
	viewType TextViewType,
	opts TextAggregateOptions,
) ([]AggregateRow, error) {
	def, err := textAggViewDef(viewType, opts.TimeGranularity)
	if err != nil {
		return nil, err
	}

	// Build WHERE clause with text type filter.
	conditions := []string{textTypeFilter()}
	var args []interface{}

	if opts.SourceID != nil {
		conditions = append(conditions, "msg.source_id = ?")
		args = append(args, *opts.SourceID)
	}
	if opts.After != nil {
		conditions = append(conditions,
			"msg.sent_at >= CAST(? AS TIMESTAMP)")
		args = append(args,
			opts.After.Format("2006-01-02 15:04:05"))
	}
	if opts.Before != nil {
		conditions = append(conditions,
			"msg.sent_at < CAST(? AS TIMESTAMP)")
		args = append(args,
			opts.Before.Format("2006-01-02 15:04:05"))
	}

	// Search filter on key columns.
	if opts.SearchQuery != "" {
		searchConds, searchArgs := e.buildAggregateSearchConditions(
			opts.SearchQuery, def.keyColumns...)
		conditions = append(conditions, searchConds...)
		args = append(args, searchArgs...)
	}

	whereClause := strings.Join(conditions, " AND ")

	aggOpts := AggregateOptions{
		SortField:       textSortFieldToSortField(opts.SortField),
		SortDirection:   opts.SortDirection,
		Limit:           opts.Limit,
		TimeGranularity: opts.TimeGranularity,
	}

	return e.runAggregation(ctx, def, whereClause, args, aggOpts)
}

// ListConversationMessages returns messages within a conversation,
// ordered chronologically (ASC) for timeline display.
func (e *DuckDBEngine) ListConversationMessages(
	ctx context.Context, convID int64, filter TextFilter,
) ([]MessageSummary, error) {
	// Use SQLite directly for timeline messages — Parquet doesn't
	// include message_bodies, and timelines need the full body text.
	if e.sqliteEngine != nil {
		return e.sqliteEngine.ListConversationMessages(
			ctx, convID, filter,
		)
	}

	// Fallback to Parquet (snippet only, no body text).
	// NOTE: search results will only show snippets, not full body
	// text, since Parquet files do not contain message bodies.
	where, args := e.buildTextFilterConditions(filter)
	where += " AND msg.conversation_id = ?"
	args = append(args, convID)

	limit := filter.Pagination.Limit
	if limit == 0 {
		limit = 500
	}

	direction := "ASC"
	if filter.SortDirection == SortDesc {
		direction = "DESC"
	}

	query := fmt.Sprintf(`
		WITH %s,
		filtered_msgs AS (
			SELECT msg.id
			FROM msg
			WHERE %s
			ORDER BY msg.sent_at %s
			LIMIT ? OFFSET ?
		),
		msg_sender AS (
			SELECT mr.message_id,
				FIRST(p.email_address) AS from_email,
				FIRST(COALESCE(mr.display_name, p.display_name, '')) AS from_name,
				FIRST(COALESCE(p.phone_number, '')) AS from_phone
			FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.recipient_type = 'from'
			  AND mr.message_id IN (SELECT id FROM filtered_msgs)
			GROUP BY mr.message_id
		),
		direct_sender AS (
			SELECT msg.id AS message_id,
				COALESCE(p.email_address, '') AS from_email,
				COALESCE(p.display_name, '') AS from_name,
				COALESCE(p.phone_number, '') AS from_phone
			FROM msg
			JOIN filtered_msgs fm ON fm.id = msg.id
			JOIN p ON p.id = msg.sender_id
			WHERE msg.sender_id IS NOT NULL
			  AND msg.id NOT IN (SELECT message_id FROM msg_sender)
		)
		SELECT
			msg.id,
			COALESCE(msg.source_message_id, '') AS source_message_id,
			COALESCE(msg.conversation_id, 0) AS conversation_id,
			COALESCE(c.source_conversation_id, '') AS source_conversation_id,
			COALESCE(msg.subject, '') AS subject,
			COALESCE(msg.snippet, '') AS snippet,
			COALESCE(ms.from_email, ds.from_email, '') AS from_email,
			COALESCE(ms.from_name, ds.from_name, '') AS from_name,
			COALESCE(ms.from_phone, ds.from_phone, '') AS from_phone,
			msg.sent_at,
			COALESCE(msg.size_estimate, 0) AS size_estimate,
			COALESCE(msg.has_attachments, false) AS has_attachments,
			COALESCE(msg.attachment_count, 0) AS attachment_count,
			msg.deleted_from_source_at,
			COALESCE(msg.message_type, '') AS message_type,
			COALESCE(c.title, '') AS conv_title
		FROM msg
		JOIN filtered_msgs fm ON fm.id = msg.id
		LEFT JOIN msg_sender ms ON ms.message_id = msg.id
		LEFT JOIN direct_sender ds ON ds.message_id = msg.id
		LEFT JOIN conv c ON c.id = msg.conversation_id
		ORDER BY msg.sent_at %s
	`, e.parquetCTEs(), where, direction, direction)

	args = append(args, limit, filter.Pagination.Offset)

	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list conversation messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanMessageSummaries(rows)
}

// TextSearch performs plain full-text search over text messages via FTS5.
// Returns empty results if SQLite is not available.
//
// Known limitation: results contain snippets but not full BodyText, so the
// chat timeline will show truncated previews for search results rather than
// the complete message body.
func (e *DuckDBEngine) TextSearch(
	ctx context.Context, query string, limit, offset int,
) ([]MessageSummary, error) {
	if e.sqliteDB == nil {
		return nil, nil
	}
	if query == "" {
		return nil, nil
	}
	if limit == 0 {
		limit = 50
	}

	// Use FTS5 MATCH on messages_fts, filtered to text message types.
	sqlQuery := fmt.Sprintf(`
		SELECT
			m.id,
			COALESCE(m.source_message_id, '') AS source_message_id,
			COALESCE(m.conversation_id, 0) AS conversation_id,
			'' AS source_conversation_id,
			COALESCE(m.subject, '') AS subject,
			COALESCE(m.snippet, '') AS snippet,
			COALESCE(p.email_address, '') AS from_email,
			COALESCE(p.display_name, '') AS from_name,
			COALESCE(p.phone_number, '') AS from_phone,
			m.sent_at,
			COALESCE(m.size_estimate, 0) AS size_estimate,
			COALESCE(m.has_attachments, 0) AS has_attachments,
			0 AS attachment_count,
			m.deleted_from_source_at,
			COALESCE(m.message_type, '') AS message_type,
			COALESCE(c.title, '') AS conv_title
		FROM messages_fts fts
		JOIN messages m ON m.id = fts.rowid
		LEFT JOIN participants p ON p.id = m.sender_id
		LEFT JOIN conversations c ON c.id = m.conversation_id
		WHERE messages_fts MATCH ?
		  AND m.message_type IN ('whatsapp','imessage','sms','google_voice_text')
		  AND %s
		ORDER BY m.sent_at DESC
		LIMIT ? OFFSET ?
	`, store.LiveMessagesWhere("m", true))

	rows, err := e.sqliteDB.QueryContext(ctx, sqlQuery,
		query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("text search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanMessageSummaries(rows)
}

// GetTextStats returns aggregate stats for text messages.
func (e *DuckDBEngine) GetTextStats(
	ctx context.Context, opts TextStatsOptions,
) (*TotalStats, error) {
	stats := &TotalStats{}

	conditions := []string{textTypeFilter()}
	var args []interface{}

	if opts.SourceID != nil {
		conditions = append(conditions, "msg.source_id = ?")
		args = append(args, *opts.SourceID)
	}
	if opts.SearchQuery != "" {
		termPattern := "%" + escapeILIKE(opts.SearchQuery) + "%"
		conditions = append(conditions,
			"(msg.subject ILIKE ? ESCAPE '\\' OR msg.snippet ILIKE ? ESCAPE '\\')")
		args = append(args, termPattern, termPattern)
	}

	whereClause := strings.Join(conditions, " AND ")

	msgQuery := fmt.Sprintf(`
		WITH %s
		SELECT
			COUNT(*) AS message_count,
			COALESCE(SUM(CAST(msg.size_estimate AS BIGINT)), 0) AS total_size,
			CAST(COALESCE(SUM(att.attachment_count), 0) AS BIGINT) AS attachment_count,
			CAST(COALESCE(SUM(att.attachment_size), 0) AS BIGINT) AS attachment_size,
			COUNT(DISTINCT msg.source_id) AS account_count
		FROM msg
		LEFT JOIN att ON att.message_id = msg.id
		WHERE %s
	`, e.parquetCTEs(), whereClause)

	var attachmentSize sql.NullFloat64
	err := e.db.QueryRowContext(ctx, msgQuery, args...).Scan(
		&stats.MessageCount,
		&stats.TotalSize,
		&stats.AttachmentCount,
		&attachmentSize,
		&stats.AccountCount,
	)
	if err != nil {
		return nil, fmt.Errorf("text stats query: %w", err)
	}
	if attachmentSize.Valid {
		stats.AttachmentSize = int64(attachmentSize.Float64)
	}

	// Label count for text messages.
	labelQuery := fmt.Sprintf(`
		WITH %s
		SELECT COUNT(DISTINCT lbl.name)
		FROM msg
		JOIN ml ON ml.message_id = msg.id
		JOIN lbl ON lbl.id = ml.label_id
		WHERE %s
	`, e.parquetCTEs(), whereClause)

	if err := e.db.QueryRowContext(ctx, labelQuery, args...).Scan(
		&stats.LabelCount,
	); err != nil {
		stats.LabelCount = 0
	}

	return stats, nil
}

// scanMessageSummariesWithBody scans rows that include a body_text column
// as the 17th field. Used by ListConversationMessages for chat timelines.
func scanMessageSummariesWithBody(rows *sql.Rows) ([]MessageSummary, error) {
	var results []MessageSummary
	for rows.Next() {
		var msg MessageSummary
		var sentAt sql.NullTime
		var deletedAt sql.NullTime
		if err := rows.Scan(
			&msg.ID,
			&msg.SourceMessageID,
			&msg.ConversationID,
			&msg.SourceConversationID,
			&msg.Subject,
			&msg.Snippet,
			&msg.FromEmail,
			&msg.FromName,
			&msg.FromPhone,
			&sentAt,
			&msg.SizeEstimate,
			&msg.HasAttachments,
			&msg.AttachmentCount,
			&deletedAt,
			&msg.MessageType,
			&msg.ConversationTitle,
			&msg.BodyText,
		); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if sentAt.Valid {
			msg.SentAt = sentAt.Time
		}
		if deletedAt.Valid {
			msg.DeletedAt = &deletedAt.Time
		}
		results = append(results, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}
	return results, nil
}

// scanMessageSummaries scans rows into MessageSummary slices.
// Shared by TextSearch and Parquet-based timeline fallback.
func scanMessageSummaries(rows *sql.Rows) ([]MessageSummary, error) {
	var results []MessageSummary
	for rows.Next() {
		var msg MessageSummary
		var sentAt sql.NullTime
		var deletedAt sql.NullTime
		if err := rows.Scan(
			&msg.ID,
			&msg.SourceMessageID,
			&msg.ConversationID,
			&msg.SourceConversationID,
			&msg.Subject,
			&msg.Snippet,
			&msg.FromEmail,
			&msg.FromName,
			&msg.FromPhone,
			&sentAt,
			&msg.SizeEstimate,
			&msg.HasAttachments,
			&msg.AttachmentCount,
			&deletedAt,
			&msg.MessageType,
			&msg.ConversationTitle,
		); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if sentAt.Valid {
			msg.SentAt = sentAt.Time
		}
		if deletedAt.Valid {
			msg.DeletedAt = &deletedAt.Time
		}
		results = append(results, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}
	return results, nil
}
