package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/wesm/msgvault/internal/search"
)

// APIMessage represents a message for API responses.
type APIMessage struct {
	ID             int64
	ConversationID int64
	Subject        string
	From           string
	To             []string
	Cc             []string
	Bcc            []string
	SentAt         time.Time
	Snippet        string
	Labels         []string
	HasAttachments bool
	SizeEstimate   int64
	DeletedAt      *time.Time
	Body           string
	Headers        map[string]string
	Attachments    []APIAttachment
}

// APIAttachment represents attachment metadata for API responses.
type APIAttachment struct {
	Filename string
	MimeType string
	Size     int64
}

// ListMessages returns a paginated list of messages with batch-loaded recipients and labels.
func (s *Store) ListMessages(offset, limit int) ([]APIMessage, int64, error) {
	// Get total count. Use the canonical live-messages predicate so
	// dedup-hidden rows (deleted_at) are excluded alongside source-
	// deleted rows.
	var total int64
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE " + LiveMessagesWhere("", true),
	).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	// Query messages with sender info
	query := fmt.Sprintf(`
		SELECT
			m.id,
			COALESCE(m.conversation_id, 0) as conversation_id,
			COALESCE(m.subject, '') as subject,
			COALESCE(p.email_address, '') as from_email,
			COALESCE(m.sent_at, m.received_at, m.internal_date) as sent_at,
			COALESCE(m.snippet, '') as snippet,
			m.has_attachments,
			m.size_estimate
		FROM messages m
		LEFT JOIN message_recipients mr ON mr.message_id = m.id AND mr.recipient_type = 'from'
		LEFT JOIN participants p ON p.id = mr.participant_id
		WHERE %s
		ORDER BY COALESCE(m.sent_at, m.received_at, m.internal_date) DESC
		LIMIT ? OFFSET ?
	`, LiveMessagesWhere("m", true))

	rows, err := s.db.Query(query, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	// Use scanMessageRows for robust date parsing
	messages, ids, err := scanMessageRows(rows)
	if err != nil {
		return nil, 0, err
	}

	if len(ids) == 0 {
		return messages, total, nil
	}

	// Batch-load recipients and labels for all messages
	if err := s.batchPopulate(messages, ids); err != nil {
		return nil, 0, err
	}

	return messages, total, nil
}

// GetMessage returns a single message with full details.
// Only this method accesses message_bodies (single PK lookup).
func (s *Store) GetMessage(id int64) (*APIMessage, error) {
	query := `
		SELECT
			m.id,
			COALESCE(m.conversation_id, 0) as conversation_id,
			COALESCE(m.subject, '') as subject,
			COALESCE(p.email_address, '') as from_email,
			COALESCE(m.sent_at, m.received_at, m.internal_date) as sent_at,
			COALESCE(m.snippet, '') as snippet,
			m.has_attachments,
			m.size_estimate,
			m.deleted_from_source_at
		FROM messages m
		LEFT JOIN message_recipients mr ON mr.message_id = m.id AND mr.recipient_type = 'from'
		LEFT JOIN participants p ON p.id = mr.participant_id
		WHERE m.id = ?
	`

	var m APIMessage
	var sentAtStr sql.NullString
	var deletedAtStr sql.NullString
	err := s.db.QueryRow(query, id).Scan(&m.ID, &m.ConversationID, &m.Subject, &m.From, &sentAtStr, &m.Snippet, &m.HasAttachments, &m.SizeEstimate, &deletedAtStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if sentAtStr.Valid && sentAtStr.String != "" {
		m.SentAt = parseSQLiteTime(sentAtStr.String)
	}
	if deletedAtStr.Valid && deletedAtStr.String != "" {
		deletedAt := parseSQLiteTime(deletedAtStr.String)
		m.DeletedAt = &deletedAt
	}

	// Get recipients (single message, per-row is fine)
	m.To, err = s.getRecipients(m.ID, "to")
	if err != nil {
		return nil, err
	}
	m.Cc, err = s.getRecipients(m.ID, "cc")
	if err != nil {
		return nil, err
	}
	m.Bcc, err = s.getRecipients(m.ID, "bcc")
	if err != nil {
		return nil, err
	}

	// Get labels (single message, per-row is fine)
	m.Labels, err = s.getLabels(m.ID)
	if err != nil {
		return nil, err
	}

	// Get body (single PK lookup — only place we touch message_bodies)
	var bodyText, bodyHTML sql.NullString
	err = s.db.QueryRow("SELECT body_text, body_html FROM message_bodies WHERE message_id = ?", id).Scan(&bodyText, &bodyHTML)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("get message body: %w", err)
	}
	if bodyText.Valid {
		m.Body = bodyText.String
	} else if bodyHTML.Valid {
		m.Body = bodyHTML.String
	}

	// Get attachments
	attRows, err := s.db.Query("SELECT filename, mime_type, size FROM attachments WHERE message_id = ?", id)
	if err == nil {
		defer func() { _ = attRows.Close() }()
		for attRows.Next() {
			var att APIAttachment
			if err := attRows.Scan(&att.Filename, &att.MimeType, &att.Size); err == nil {
				m.Attachments = append(m.Attachments, att)
			}
		}
	}

	m.Headers = make(map[string]string)

	return &m, nil
}

// GetMessagesSummariesByIDs returns summary-level (no body, no
// attachments) APIMessage rows for the supplied IDs in the same order
// as ids. Missing IDs are silently dropped — callers are expected to
// have already filtered for live messages, and a missing row in the
// summary set is just "ignore this hit". Recipients and labels are
// batch-loaded with the same shape as SearchMessages, so the worst
// case is 5 SQL round-trips regardless of len(ids). This is the
// designated hydration path for vector/hybrid search hits, where
// callers loop over many MessageIDs and never need body or
// attachments — calling GetMessage in that loop costs ~7 queries per
// hit (body + attachments + 3 recipients + labels + base) and
// dominates p50 search latency past a handful of results.
func (s *Store) GetMessagesSummariesByIDs(ids []int64) ([]APIMessage, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	q := fmt.Sprintf(`
		SELECT
			m.id,
			COALESCE(m.conversation_id, 0) as conversation_id,
			COALESCE(m.subject, '') as subject,
			COALESCE(p.email_address, '') as from_email,
			COALESCE(m.sent_at, m.received_at, m.internal_date) as sent_at,
			COALESCE(m.snippet, '') as snippet,
			m.has_attachments,
			m.size_estimate
		FROM messages m
		LEFT JOIN message_recipients mr ON mr.message_id = m.id AND mr.recipient_type = 'from'
		LEFT JOIN participants p ON p.id = mr.participant_id
		WHERE m.id IN (%s) AND %s
	`, strings.Join(placeholders, ","), LiveMessagesWhere("m", true))
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("get message summaries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	messages, foundIDs, err := scanMessageRows(rows)
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, nil
	}
	if err := s.batchPopulate(messages, foundIDs); err != nil {
		return nil, err
	}

	// Re-order to match the caller's id order so search rank is
	// preserved end-to-end.
	indexByID := make(map[int64]int, len(messages))
	for i, m := range messages {
		indexByID[m.ID] = i
	}
	ordered := make([]APIMessage, 0, len(ids))
	for _, id := range ids {
		if idx, ok := indexByID[id]; ok {
			ordered = append(ordered, messages[idx])
		}
	}
	return ordered, nil
}

// SearchMessages searches messages using full-text search, with batch-loaded recipients and labels.
func (s *Store) SearchMessages(query string, offset, limit int) ([]APIMessage, int64, error) {
	ftsJoin, ftsWhere, ftsOrder, orderArgCount := s.dialect.FTSSearchClause()

	ftsQuery := fmt.Sprintf(`
		SELECT
			m.id,
			COALESCE(m.conversation_id, 0) as conversation_id,
			COALESCE(m.subject, '') as subject,
			COALESCE(p.email_address, '') as from_email,
			COALESCE(m.sent_at, m.received_at, m.internal_date) as sent_at,
			COALESCE(m.snippet, '') as snippet,
			m.has_attachments,
			m.size_estimate
		FROM messages m
		%s
		LEFT JOIN message_recipients mr ON mr.message_id = m.id AND mr.recipient_type = 'from'
		LEFT JOIN participants p ON p.id = mr.participant_id
		WHERE %s AND %s
		ORDER BY %s
		LIMIT ? OFFSET ?
	`, ftsJoin, ftsWhere, LiveMessagesWhere("m", true), ftsOrder)

	// Bind the search term once for WHERE, plus orderArgCount more times
	// for any ? placeholders the dialect put in the order-by fragment.
	searchArgs := make([]interface{}, 0, 3+orderArgCount)
	searchArgs = append(searchArgs, query)
	for i := 0; i < orderArgCount; i++ {
		searchArgs = append(searchArgs, query)
	}
	searchArgs = append(searchArgs, limit, offset)

	rows, err := s.db.Query(ftsQuery, searchArgs...)
	if err != nil {
		// FTS might not be available, fall back to LIKE search
		return s.searchMessagesLike(query, offset, limit)
	}
	defer func() { _ = rows.Close() }()

	messages, ids, err := scanMessageRows(rows)
	if err != nil {
		return nil, 0, err
	}

	if len(ids) == 0 {
		return []APIMessage{}, 0, nil
	}

	// Get total count
	var total int64
	countQuery := fmt.Sprintf(`
		SELECT COUNT(*)
		FROM messages m
		%s
		WHERE %s AND %s
	`, ftsJoin, ftsWhere, LiveMessagesWhere("m", true))
	if err := s.db.QueryRow(countQuery, query).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count FTS results: %w", err)
	}

	// Batch-load recipients and labels
	if err := s.batchPopulate(messages, ids); err != nil {
		return nil, 0, err
	}

	return messages, total, nil
}

// SearchMessagesQuery searches messages using a parsed query with
// support for structured operators (from:, to:, label:, etc.).
func (s *Store) SearchMessagesQuery(
	q *search.Query, offset, limit int,
) ([]APIMessage, int64, error) {
	var conditions []string
	var args []interface{}

	conditions = append(conditions, LiveMessagesWhere("m", true))

	// FTS text terms. ftsEnabled is the authoritative signal that FTS is
	// active — ftsJoin may be empty on dialects (e.g. PostgreSQL) whose
	// tsvector lives on the main table and needs no extra join.
	ftsEnabled := len(q.TextTerms) > 0
	var ftsJoin, ftsOrder, ftsExpr string
	var ftsOrderArgCount int
	if ftsEnabled {
		ftsExpr = buildFTSExpression(q.TextTerms)
		join, where, orderBy, orderArgCount := s.dialect.FTSSearchClause()
		ftsJoin = join
		ftsOrder = orderBy
		ftsOrderArgCount = orderArgCount
		conditions = append(conditions, where)
		args = append(args, ftsExpr)
	}

	// from: filter
	for _, addr := range q.FromAddrs {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM message_recipients mr2
			JOIN participants p2 ON p2.id = mr2.participant_id
			WHERE mr2.message_id = m.id
			AND mr2.recipient_type = 'from'
			AND LOWER(p2.email_address) LIKE ? ESCAPE '\'
		)`)
		args = append(args,
			"%"+escapeLike(strings.ToLower(addr))+"%")
	}

	// to: filter
	for _, addr := range q.ToAddrs {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM message_recipients mr2
			JOIN participants p2 ON p2.id = mr2.participant_id
			WHERE mr2.message_id = m.id
			AND mr2.recipient_type = 'to'
			AND LOWER(p2.email_address) LIKE ? ESCAPE '\'
		)`)
		args = append(args,
			"%"+escapeLike(strings.ToLower(addr))+"%")
	}

	// cc: filter
	for _, addr := range q.CcAddrs {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM message_recipients mr2
			JOIN participants p2 ON p2.id = mr2.participant_id
			WHERE mr2.message_id = m.id
			AND mr2.recipient_type = 'cc'
			AND LOWER(p2.email_address) LIKE ? ESCAPE '\'
		)`)
		args = append(args,
			"%"+escapeLike(strings.ToLower(addr))+"%")
	}

	// bcc: filter
	for _, addr := range q.BccAddrs {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM message_recipients mr2
			JOIN participants p2 ON p2.id = mr2.participant_id
			WHERE mr2.message_id = m.id
			AND mr2.recipient_type = 'bcc'
			AND LOWER(p2.email_address) LIKE ? ESCAPE '\'
		)`)
		args = append(args,
			"%"+escapeLike(strings.ToLower(addr))+"%")
	}

	// label: filter
	for _, lbl := range q.Labels {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM message_labels ml2
			JOIN labels l2 ON l2.id = ml2.label_id
			WHERE ml2.message_id = m.id
			AND LOWER(l2.name) LIKE ? ESCAPE '\'
		)`)
		args = append(args,
			"%"+escapeLike(strings.ToLower(lbl))+"%")
	}

	// subject: filter
	for _, term := range q.SubjectTerms {
		conditions = append(conditions,
			`m.subject LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(term)+"%")
	}

	// has:attachment
	if q.HasAttachment != nil && *q.HasAttachment {
		conditions = append(conditions,
			"m.has_attachments = 1")
	}

	// larger: / smaller:
	if q.LargerThan != nil {
		conditions = append(conditions, "m.size_estimate > ?")
		args = append(args, *q.LargerThan)
	}
	if q.SmallerThan != nil {
		conditions = append(conditions, "m.size_estimate < ?")
		args = append(args, *q.SmallerThan)
	}

	// after: / before:
	if q.AfterDate != nil {
		conditions = append(conditions,
			"COALESCE(m.sent_at, m.received_at, m.internal_date) >= ?")
		args = append(args, q.AfterDate.Format(time.RFC3339))
	}
	if q.BeforeDate != nil {
		conditions = append(conditions,
			"COALESCE(m.sent_at, m.received_at, m.internal_date) < ?")
		args = append(args, q.BeforeDate.Format(time.RFC3339))
	}

	whereClause := strings.Join(conditions, " AND ")

	// Count query.
	countSQL := fmt.Sprintf(`
		SELECT COUNT(*)
		FROM messages m
		%s
		WHERE %s
	`, ftsJoin, whereClause)

	var total int64
	if err := s.db.QueryRow(countSQL, args...).Scan(&total); err != nil {
		if ftsEnabled {
			return s.searchMessagesQueryNoFTS(q, offset, limit)
		}
		return nil, 0, fmt.Errorf("count search results: %w", err)
	}

	// Results query.
	orderBy := "COALESCE(m.sent_at, m.received_at, m.internal_date) DESC"
	if ftsEnabled {
		orderBy = ftsOrder + ", " + orderBy
	}
	searchSQL := fmt.Sprintf(`
		SELECT
			m.id,
			COALESCE(m.conversation_id, 0) as conversation_id,
			COALESCE(m.subject, '') as subject,
			COALESCE(p.email_address, '') as from_email,
			COALESCE(m.sent_at, m.received_at, m.internal_date) as sent_at,
			COALESCE(m.snippet, '') as snippet,
			m.has_attachments,
			m.size_estimate
		FROM messages m
		%s
		LEFT JOIN message_recipients mr
			ON mr.message_id = m.id AND mr.recipient_type = 'from'
		LEFT JOIN participants p ON p.id = mr.participant_id
		WHERE %s
		ORDER BY %s
		LIMIT ? OFFSET ?
	`, ftsJoin, whereClause, orderBy)

	// If the dialect's order-by fragment has ? placeholders, bind the FTS
	// expression that many extra times — right after the WHERE args and
	// before LIMIT/OFFSET so Rebind assigns them the correct positions.
	resultArgs := make([]interface{}, 0, len(args)+ftsOrderArgCount+2)
	resultArgs = append(resultArgs, args...)
	for i := 0; i < ftsOrderArgCount; i++ {
		resultArgs = append(resultArgs, ftsExpr)
	}
	resultArgs = append(resultArgs, limit, offset)
	rows, err := s.db.Query(searchSQL, resultArgs...)
	if err != nil {
		// FTS5 not available -- fall back if we used it.
		if ftsEnabled {
			return s.searchMessagesQueryNoFTS(q, offset, limit)
		}
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	messages, ids, err := scanMessageRows(rows)
	if err != nil {
		return nil, 0, err
	}

	if len(ids) > 0 {
		if err := s.batchPopulate(messages, ids); err != nil {
			return nil, 0, err
		}
	}

	return messages, total, nil
}

// buildFTSExpression builds an FTS5 MATCH expression from text terms.
func buildFTSExpression(terms []string) string {
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " AND ")
}

// searchMessagesQueryNoFTS is a fallback when FTS5 is unavailable.
func (s *Store) searchMessagesQueryNoFTS(
	q *search.Query, offset, limit int,
) ([]APIMessage, int64, error) {
	fallbackQ := *q
	fallbackQ.SubjectTerms = append(fallbackQ.SubjectTerms, q.TextTerms...)
	fallbackQ.TextTerms = nil
	return s.SearchMessagesQuery(&fallbackQ, offset, limit)
}

// escapeLike escapes SQL LIKE special characters (%, _) so they are
// matched literally. The escaped string should be used with ESCAPE '\'.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// searchMessagesLike is a fallback search using LIKE with batch-loaded recipients and labels.
func (s *Store) searchMessagesLike(query string, offset, limit int) ([]APIMessage, int64, error) {
	likePattern := "%" + escapeLike(query) + "%"

	countQuery := fmt.Sprintf(`
		SELECT COUNT(*) FROM messages
		WHERE %s
		AND (subject LIKE ? ESCAPE '\' OR snippet LIKE ? ESCAPE '\')
	`, LiveMessagesWhere("", true))
	var total int64
	if err := s.db.QueryRow(countQuery, likePattern, likePattern).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count search results: %w", err)
	}

	searchQuery := fmt.Sprintf(`
		SELECT
			m.id,
			COALESCE(m.conversation_id, 0) as conversation_id,
			COALESCE(m.subject, '') as subject,
			COALESCE(p.email_address, '') as from_email,
			COALESCE(m.sent_at, m.received_at, m.internal_date) as sent_at,
			COALESCE(m.snippet, '') as snippet,
			m.has_attachments,
			m.size_estimate
		FROM messages m
		LEFT JOIN message_recipients mr ON mr.message_id = m.id AND mr.recipient_type = 'from'
		LEFT JOIN participants p ON p.id = mr.participant_id
		WHERE %s
		AND (m.subject LIKE ? ESCAPE '\' OR m.snippet LIKE ? ESCAPE '\')
		ORDER BY COALESCE(m.sent_at, m.received_at, m.internal_date) DESC
		LIMIT ? OFFSET ?
	`, LiveMessagesWhere("m", true))

	rows, err := s.db.Query(searchQuery, likePattern, likePattern, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	messages, ids, err := scanMessageRows(rows)
	if err != nil {
		return nil, 0, err
	}

	if len(ids) == 0 {
		return messages, total, nil
	}

	// Batch-load recipients and labels
	if err := s.batchPopulate(messages, ids); err != nil {
		return nil, 0, err
	}

	return messages, total, nil
}

// scanMessageRows scans the standard 8-column message row set.
// Uses string scanning for dates to handle all SQLite datetime formats robustly.
func scanMessageRows(rows *loggedRows) ([]APIMessage, []int64, error) {
	var messages []APIMessage
	var ids []int64
	for rows.Next() {
		var m APIMessage
		var sentAtStr sql.NullString
		err := rows.Scan(&m.ID, &m.ConversationID, &m.Subject, &m.From, &sentAtStr, &m.Snippet, &m.HasAttachments, &m.SizeEstimate)
		if err != nil {
			return nil, nil, err
		}
		if sentAtStr.Valid && sentAtStr.String != "" {
			m.SentAt = parseSQLiteTime(sentAtStr.String)
		}
		messages = append(messages, m)
		ids = append(ids, m.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate messages: %w", err)
	}
	return messages, ids, nil
}

// parseSQLiteTime parses a datetime string from SQLite into time.Time.
// Uses the same comprehensive format list as dbTimeLayouts in sync.go.
func parseSQLiteTime(s string) time.Time {
	// Same formats as dbTimeLayouts - order matters: more specific first
	layouts := []string{
		"2006-01-02 15:04:05.999999999-07:00", // space-separated with fractional seconds and TZ
		"2006-01-02T15:04:05.999999999-07:00", // T-separated with fractional seconds and TZ
		"2006-01-02 15:04:05.999999999",       // space-separated with fractional seconds
		"2006-01-02T15:04:05.999999999",       // T-separated with fractional seconds
		"2006-01-02 15:04:05",                 // SQLite datetime('now') format
		"2006-01-02T15:04:05",                 // T-separated basic
		"2006-01-02 15:04",                    // space-separated without seconds
		"2006-01-02T15:04",                    // T-separated without seconds
		"2006-01-02",                          // date only
		time.RFC3339,                          // e.g., "2006-01-02T15:04:05Z"
		time.RFC3339Nano,                      // e.g., "2006-01-02T15:04:05.999999999Z07:00"
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// batchPopulate batch-loads recipients and labels for a slice of messages.
func (s *Store) batchPopulate(messages []APIMessage, ids []int64) error {
	recipientMap, err := s.batchGetRecipients(ids, "to")
	if err != nil {
		return err
	}
	ccMap, err := s.batchGetRecipients(ids, "cc")
	if err != nil {
		return err
	}
	bccMap, err := s.batchGetRecipients(ids, "bcc")
	if err != nil {
		return err
	}
	labelMap, err := s.batchGetLabels(ids)
	if err != nil {
		return err
	}
	for i := range messages {
		messages[i].To = recipientMap[messages[i].ID]
		messages[i].Cc = ccMap[messages[i].ID]
		messages[i].Bcc = bccMap[messages[i].ID]
		messages[i].Labels = labelMap[messages[i].ID]
	}
	return nil
}

// batchGetRecipients loads recipients for multiple messages in a single query.
func (s *Store) batchGetRecipients(messageIDs []int64, recipientType string) (map[int64][]string, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(messageIDs))
	args := make([]interface{}, 0, len(messageIDs)+1)
	for i, id := range messageIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	args = append(args, recipientType)

	query := fmt.Sprintf(`
		SELECT mr.message_id, COALESCE(p.email_address, '')
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id IN (%s) AND mr.recipient_type = ?
	`, strings.Join(placeholders, ","))

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("batch get recipients: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[int64][]string, len(messageIDs))
	for rows.Next() {
		var msgID int64
		var email string
		if err := rows.Scan(&msgID, &email); err != nil {
			return nil, fmt.Errorf("scan recipient: %w", err)
		}
		if email != "" {
			result[msgID] = append(result[msgID], email)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recipients: %w", err)
	}
	return result, nil
}

// batchGetLabels loads labels for multiple messages in a single query.
func (s *Store) batchGetLabels(messageIDs []int64) (map[int64][]string, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(messageIDs))
	args := make([]interface{}, 0, len(messageIDs))
	for i, id := range messageIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}

	query := fmt.Sprintf(`
		SELECT ml.message_id, l.name
		FROM message_labels ml
		JOIN labels l ON l.id = ml.label_id
		WHERE ml.message_id IN (%s)
	`, strings.Join(placeholders, ","))

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("batch get labels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[int64][]string, len(messageIDs))
	for rows.Next() {
		var msgID int64
		var name string
		if err := rows.Scan(&msgID, &name); err != nil {
			return nil, fmt.Errorf("scan label: %w", err)
		}
		result[msgID] = append(result[msgID], name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate labels: %w", err)
	}
	return result, nil
}

// Single-message helpers (still used by GetMessage for single PK lookups)

func (s *Store) getRecipients(messageID int64, recipientType string) ([]string, error) {
	query := `
		SELECT COALESCE(p.email_address, '')
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id = ? AND mr.recipient_type = ?
	`
	rows, err := s.db.Query(query, messageID, recipientType)
	if err != nil {
		return nil, fmt.Errorf("get recipients: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var recipients []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, fmt.Errorf("scan recipient: %w", err)
		}
		if email != "" {
			recipients = append(recipients, email)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recipients: %w", err)
	}
	return recipients, nil
}

func (s *Store) getLabels(messageID int64) ([]string, error) {
	query := `
		SELECT l.name
		FROM message_labels ml
		JOIN labels l ON l.id = ml.label_id
		WHERE ml.message_id = ?
	`
	rows, err := s.db.Query(query, messageID)
	if err != nil {
		return nil, fmt.Errorf("get labels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var labels []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan label: %w", err)
		}
		labels = append(labels, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate labels: %w", err)
	}
	return labels, nil
}
