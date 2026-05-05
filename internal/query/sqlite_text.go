package query

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/wesm/msgvault/internal/store"
)

// Compile-time interface assertion.
var _ TextEngine = (*SQLiteEngine)(nil)

// sqliteTimestampLayouts lists the datetime string formats emitted by SQLite
// and the go-sqlite3 driver. More specific formats come first.
var sqliteTimestampLayouts = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04",
	"2006-01-02T15:04",
	"2006-01-02",
	time.RFC3339,
	time.RFC3339Nano,
}

// parseSQLiteTimestamp parses a datetime string from a SQLite aggregate
// (e.g., MAX(sent_at)) that the driver returns as a plain string rather
// than a time.Time value.
func parseSQLiteTimestamp(s string) (time.Time, error) {
	for _, layout := range sqliteTimestampLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized SQLite timestamp %q", s)
}

// textMsgTypeFilter returns a SQL condition restricting to text message types.
// Uses the m. table alias used in text query methods.
func textMsgTypeFilter() string {
	return "m.message_type IN ('whatsapp','imessage','sms','google_voice_text')"
}

// textMsgTypeFilterAlias returns a SQL condition restricting to text message types
// using the given table alias.
func textMsgTypeFilterAlias(alias string) string {
	return alias + ".message_type IN ('whatsapp','imessage','sms','google_voice_text')"
}

func sqliteDirection(d SortDirection) string {
	if d == SortAsc {
		return "ASC"
	}
	return "DESC"
}

// buildSQLiteTextFilterConditions builds WHERE conditions from a TextFilter.
// All conditions use the m. prefix for the messages table.
func buildSQLiteTextFilterConditions(filter TextFilter) (string, []interface{}) {
	conditions := []string{textMsgTypeFilter()}
	var args []interface{}

	if filter.SourceID != nil {
		conditions = append(conditions, "m.source_id = ?")
		args = append(args, *filter.SourceID)
	}
	if filter.ContactPhone != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM participants p_cp
			WHERE p_cp.id = COALESCE(m.sender_id,
				(SELECT mr_fb.participant_id FROM message_recipients mr_fb
				 WHERE mr_fb.message_id = m.id AND mr_fb.recipient_type = 'from'
				 LIMIT 1))
			  AND COALESCE(
				NULLIF(p_cp.phone_number, ''),
				p_cp.email_address
			  ) = ?
		)`)
		args = append(args, filter.ContactPhone)
	}
	if filter.ContactName != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM participants p_cn
			WHERE p_cn.id = COALESCE(m.sender_id,
				(SELECT mr_fb.participant_id FROM message_recipients mr_fb
				 WHERE mr_fb.message_id = m.id AND mr_fb.recipient_type = 'from'
				 LIMIT 1))
			  AND COALESCE(
				NULLIF(TRIM(p_cn.display_name), ''),
				NULLIF(p_cn.phone_number, ''),
				p_cn.email_address
			  ) = ?
		)`)
		args = append(args, filter.ContactName)
	}
	if filter.SourceType != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM sources s_st
			WHERE s_st.id = m.source_id AND s_st.source_type = ?
		)`)
		args = append(args, filter.SourceType)
	}
	if filter.Label != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM message_labels ml_lf
			JOIN labels lbl_lf ON lbl_lf.id = ml_lf.label_id
			WHERE ml_lf.message_id = m.id
			  AND LOWER(lbl_lf.name) = LOWER(?)
		)`)
		args = append(args, filter.Label)
	}
	if filter.TimeRange.Period != "" {
		granularity := filter.TimeRange.Granularity
		if granularity == TimeYear && len(filter.TimeRange.Period) > 4 {
			switch len(filter.TimeRange.Period) {
			case 7:
				granularity = TimeMonth
			case 10:
				granularity = TimeDay
			}
		}
		var timeExprStr string
		switch granularity {
		case TimeYear:
			timeExprStr = "strftime('%Y', m.sent_at)"
		case TimeMonth:
			timeExprStr = "strftime('%Y-%m', m.sent_at)"
		case TimeDay:
			timeExprStr = "strftime('%Y-%m-%d', m.sent_at)"
		default:
			timeExprStr = "strftime('%Y-%m', m.sent_at)"
		}
		conditions = append(conditions, fmt.Sprintf("%s = ?", timeExprStr))
		args = append(args, filter.TimeRange.Period)
	}
	if filter.After != nil {
		conditions = append(conditions, "m.sent_at >= ?")
		args = append(args, filter.After.Format("2006-01-02 15:04:05"))
	}
	if filter.Before != nil {
		conditions = append(conditions, "m.sent_at < ?")
		args = append(args, filter.Before.Format("2006-01-02 15:04:05"))
	}

	return strings.Join(conditions, " AND "), args
}

// ListConversations returns conversations matching the filter.
func (e *SQLiteEngine) ListConversations(
	ctx context.Context, filter TextFilter,
) ([]ConversationRow, error) {
	where, args := buildSQLiteTextFilterConditions(filter)

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
		SELECT
			c.id,
			COALESCE(c.title, '') AS title,
			COALESCE(s.source_type, '') AS source_type,
			COUNT(*) AS message_count,
			COUNT(DISTINCT COALESCE(m.sender_id, 0)) AS participant_count,
			MAX(m.sent_at) AS last_message_at,
			COALESCE(
				(SELECT m2.snippet FROM messages m2
				 WHERE m2.conversation_id = c.id
				   AND %s
				 ORDER BY m2.sent_at DESC, m2.id DESC LIMIT 1),
				''
			) AS last_preview,
			COALESCE(SUM(m.size_estimate), 0) AS total_size
		FROM conversations c
		JOIN messages m ON m.conversation_id = c.id
		LEFT JOIN sources s ON s.id = m.source_id
		WHERE %s
		GROUP BY c.id, c.title, s.source_type
		ORDER BY %s
		LIMIT ? OFFSET ?
	`, textMsgTypeFilterAlias("m2"), where, orderBy)

	args = append(args, limit, filter.Pagination.Offset)

	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []ConversationRow
	for rows.Next() {
		var row ConversationRow
		var lastAtStr sql.NullString
		var totalSize int64
		if err := rows.Scan(
			&row.ConversationID,
			&row.Title,
			&row.SourceType,
			&row.MessageCount,
			&row.ParticipantCount,
			&lastAtStr,
			&row.LastPreview,
			&totalSize,
		); err != nil {
			return nil, fmt.Errorf("scan conversation: %w", err)
		}
		// MAX(sent_at) returns a string from SQLite; parse it manually so that
		// the scan works regardless of column affinity on the aggregated value.
		if lastAtStr.Valid && lastAtStr.String != "" {
			if t, err := parseSQLiteTimestamp(lastAtStr.String); err == nil {
				row.LastMessageAt = t
			}
		}
		results = append(results, row)
	}
	return results, rows.Err()
}

// textAggSQLiteDimension returns the dimension definition for a text aggregate view.
func textAggSQLiteDimension(
	view TextViewType, granularity TimeGranularity,
) (aggDimension, error) {
	switch view {
	case TextViewContacts:
		keyExpr := "COALESCE(NULLIF(p_agg.phone_number, ''), p_agg.email_address)"
		senderJoin := `JOIN participants p_agg ON p_agg.id = COALESCE(m.sender_id,
			(SELECT mr_fb.participant_id FROM message_recipients mr_fb
			 WHERE mr_fb.message_id = m.id AND mr_fb.recipient_type = 'from'
			 LIMIT 1))`
		return aggDimension{
			keyExpr:   keyExpr,
			joins:     senderJoin,
			whereExpr: keyExpr + " IS NOT NULL",
		}, nil
	case TextViewContactNames:
		nameExpr := "COALESCE(NULLIF(TRIM(p_agg.display_name), ''), " +
			"NULLIF(p_agg.phone_number, ''), p_agg.email_address)"
		senderJoin := `JOIN participants p_agg ON p_agg.id = COALESCE(m.sender_id,
			(SELECT mr_fb.participant_id FROM message_recipients mr_fb
			 WHERE mr_fb.message_id = m.id AND mr_fb.recipient_type = 'from'
			 LIMIT 1))`
		return aggDimension{
			keyExpr:   nameExpr,
			joins:     senderJoin,
			whereExpr: nameExpr + " IS NOT NULL",
		}, nil
	case TextViewSources:
		return aggDimension{
			keyExpr:   "s_agg.source_type",
			joins:     "JOIN sources s_agg ON s_agg.id = m.source_id",
			whereExpr: "s_agg.source_type IS NOT NULL",
		}, nil
	case TextViewLabels:
		return aggDimension{
			keyExpr: "lbl_agg.name",
			joins: `JOIN message_labels ml_agg ON ml_agg.message_id = m.id
				JOIN labels lbl_agg ON lbl_agg.id = ml_agg.label_id`,
			whereExpr: "lbl_agg.name IS NOT NULL",
		}, nil
	case TextViewTime:
		var timeExprStr string
		switch granularity {
		case TimeYear:
			timeExprStr = "strftime('%Y', m.sent_at)"
		case TimeMonth:
			timeExprStr = "strftime('%Y-%m', m.sent_at)"
		case TimeDay:
			timeExprStr = "strftime('%Y-%m-%d', m.sent_at)"
		default:
			timeExprStr = "strftime('%Y-%m', m.sent_at)"
		}
		return aggDimension{
			keyExpr:   timeExprStr,
			joins:     "",
			whereExpr: "m.sent_at IS NOT NULL",
		}, nil
	default:
		return aggDimension{}, fmt.Errorf("unsupported text view type: %v", view)
	}
}

// TextAggregate aggregates text messages by the given view type.
func (e *SQLiteEngine) TextAggregate(
	ctx context.Context,
	viewType TextViewType,
	opts TextAggregateOptions,
) ([]AggregateRow, error) {
	dim, err := textAggSQLiteDimension(viewType, opts.TimeGranularity)
	if err != nil {
		return nil, err
	}

	conditions := []string{textMsgTypeFilter()}
	var args []interface{}

	if opts.SourceID != nil {
		conditions = append(conditions, "m.source_id = ?")
		args = append(args, *opts.SourceID)
	}
	if opts.After != nil {
		conditions = append(conditions, "m.sent_at >= ?")
		args = append(args, opts.After.Format("2006-01-02 15:04:05"))
	}
	if opts.Before != nil {
		conditions = append(conditions, "m.sent_at < ?")
		args = append(args, opts.Before.Format("2006-01-02 15:04:05"))
	}
	if opts.SearchQuery != "" {
		likeTerm := "%" + escapeSQLiteLike(opts.SearchQuery) + "%"
		conditions = append(conditions, "(m.subject LIKE ? OR m.snippet LIKE ?)")
		args = append(args, likeTerm, likeTerm)
	}

	aggOpts := AggregateOptions{
		SortField:       textSortFieldToSortField(opts.SortField),
		SortDirection:   opts.SortDirection,
		Limit:           opts.Limit,
		TimeGranularity: opts.TimeGranularity,
	}

	sort, err := sortClause(aggOpts)
	if err != nil {
		return nil, err
	}

	limit := aggOpts.Limit
	if limit == 0 {
		limit = 100
	}

	filterWhere := strings.Join(conditions, " AND ")
	query := buildAggregateSQL(dim, "", filterWhere, sort)
	args = append(args, limit)
	return e.executeAggregateQuery(ctx, query, args)
}

// ListConversationMessages returns messages within a conversation,
// ordered chronologically (ASC) for timeline display.
func (e *SQLiteEngine) ListConversationMessages(
	ctx context.Context, convID int64, filter TextFilter,
) ([]MessageSummary, error) {
	where, args := buildSQLiteTextFilterConditions(filter)
	where += " AND m.conversation_id = ?"
	args = append(args, convID)

	limit := filter.Pagination.Limit
	if limit == 0 {
		limit = 500
	}

	query := fmt.Sprintf(`
		SELECT
			m.id,
			COALESCE(m.source_message_id, '') AS source_message_id,
			COALESCE(m.conversation_id, 0) AS conversation_id,
			COALESCE(c.source_conversation_id, '') AS source_conversation_id,
			COALESCE(m.subject, '') AS subject,
			COALESCE(m.snippet, '') AS snippet,
			COALESCE(p_sender.email_address, '') AS from_email,
			COALESCE(p_sender.display_name, '') AS from_name,
			COALESCE(p_sender.phone_number, '') AS from_phone,
			m.sent_at,
			COALESCE(m.size_estimate, 0) AS size_estimate,
			m.has_attachments,
			m.attachment_count,
			m.deleted_from_source_at,
			COALESCE(m.message_type, '') AS message_type,
			COALESCE(c.title, '') AS conv_title,
			COALESCE(mb.body_text, m.snippet, '') AS body_text
		FROM messages m
		LEFT JOIN participants p_sender ON p_sender.id = m.sender_id
		LEFT JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN message_bodies mb ON mb.message_id = m.id
		WHERE %s
		ORDER BY m.sent_at %s
		LIMIT ? OFFSET ?
	`, where, sqliteDirection(filter.SortDirection))

	args = append(args, limit, filter.Pagination.Offset)

	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list conversation messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanMessageSummariesWithBody(rows)
}

// TextSearch performs plain full-text search over text messages.
// Uses FTS5 if available; otherwise returns empty results.
func (e *SQLiteEngine) TextSearch(
	ctx context.Context, query string, limit, offset int,
) ([]MessageSummary, error) {
	if query == "" {
		return nil, nil
	}
	if !e.hasFTSTable(ctx) {
		return nil, nil
	}
	if limit == 0 {
		limit = 50
	}

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
		WHERE fts.messages_fts MATCH ?
		  AND m.message_type IN ('whatsapp','imessage','sms','google_voice_text')
		  AND %s
		ORDER BY m.sent_at DESC
		LIMIT ? OFFSET ?
	`, store.LiveMessagesWhere("m", true))

	rows, err := e.db.QueryContext(ctx, sqlQuery, query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("text search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanMessageSummaries(rows)
}

// GetTextStats returns aggregate stats for text messages.
func (e *SQLiteEngine) GetTextStats(
	ctx context.Context, opts TextStatsOptions,
) (*TotalStats, error) {
	stats := &TotalStats{}

	conditions := []string{textMsgTypeFilter()}
	var args []interface{}

	if opts.SourceID != nil {
		conditions = append(conditions, "m.source_id = ?")
		args = append(args, *opts.SourceID)
	}
	if opts.SearchQuery != "" {
		likeTerm := "%" + escapeSQLiteLike(opts.SearchQuery) + "%"
		conditions = append(conditions, "(m.subject LIKE ? OR m.snippet LIKE ?)")
		args = append(args, likeTerm, likeTerm)
	}

	whereClause := strings.Join(conditions, " AND ")

	msgQuery := fmt.Sprintf(`
		SELECT
			COUNT(*) AS message_count,
			COALESCE(SUM(m.size_estimate), 0) AS total_size,
			COUNT(DISTINCT m.source_id) AS account_count
		FROM messages m
		WHERE %s
	`, whereClause)

	if err := e.db.QueryRowContext(ctx, msgQuery, args...).Scan(
		&stats.MessageCount,
		&stats.TotalSize,
		&stats.AccountCount,
	); err != nil {
		return nil, fmt.Errorf("text stats query: %w", err)
	}

	attQuery := fmt.Sprintf(`
		SELECT
			COUNT(*) AS attachment_count,
			COALESCE(SUM(a.size), 0) AS attachment_size
		FROM attachments a
		JOIN messages m ON m.id = a.message_id
		WHERE %s
	`, whereClause)

	if err := e.db.QueryRowContext(ctx, attQuery, args...).Scan(
		&stats.AttachmentCount,
		&stats.AttachmentSize,
	); err != nil {
		return nil, fmt.Errorf("text attachment stats query: %w", err)
	}

	labelQuery := fmt.Sprintf(`
		SELECT COUNT(DISTINCT lbl.name)
		FROM messages m
		JOIN message_labels ml ON ml.message_id = m.id
		JOIN labels lbl ON lbl.id = ml.label_id
		WHERE %s
	`, whereClause)

	if err := e.db.QueryRowContext(ctx, labelQuery, args...).Scan(
		&stats.LabelCount,
	); err != nil {
		stats.LabelCount = 0
	}

	return stats, nil
}
