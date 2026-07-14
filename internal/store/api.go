package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/search"
)

// participantDisplaySQL formats a participant joined as `p` (with the
// optional per-recipient `mr.display_name` row) into one display string
// the way the query-backed engines do: "Name <addr>" when both name
// and addr are present, otherwise the bare email/phone, otherwise the
// bare name. The store-backed API used to read only p.email_address,
// which dropped phone-only and identifier-only participants (synctech
// SMS/MMS, etc.). Standard SQL (CASE + ||) — works on SQLite and PG.
const participantDisplaySQL = `COALESCE(
		CASE
			WHEN COALESCE(NULLIF(TRIM(mr.display_name), ''), NULLIF(TRIM(p.display_name), '')) <> ''
			  AND COALESCE(NULLIF(p.email_address, ''), NULLIF(p.phone_number, '')) <> ''
			THEN COALESCE(NULLIF(TRIM(mr.display_name), ''), TRIM(p.display_name))
				|| ' <'
				|| COALESCE(NULLIF(p.email_address, ''), p.phone_number)
				|| '>'
			ELSE COALESCE(
				NULLIF(p.email_address, ''),
				NULLIF(p.phone_number, ''),
				NULLIF(TRIM(mr.display_name), ''),
				NULLIF(TRIM(p.display_name), ''),
				''
			)
		END,
		''
	)`

const participantSenderEmailSQL = `COALESCE(NULLIF(p.email_address, ''), '')`
const participantSenderNameSQL = `COALESCE(NULLIF(TRIM(COALESCE(NULLIF(mr.display_name, ''), p.display_name)), ''), '')`
const participantSenderPhoneSQL = `COALESCE(NULLIF(p.phone_number, ''), '')`
const participantSummarySenderSQL = participantDisplaySQL + ` as from_display,
			` + participantSenderEmailSQL + ` as from_email,
			` + participantSenderNameSQL + ` as from_name,
			` + participantSenderPhoneSQL + ` as from_phone`

// APIMessage represents a message for API responses.
type APIMessage struct {
	ID                   int64
	SourceID             int64
	SourceMessageID      string
	ConversationID       int64
	SourceConversationID string
	Subject              string
	MessageType          string
	From                 string
	FromEmail            string
	FromName             string
	FromPhone            string
	To                   []string
	Cc                   []string
	Bcc                  []string
	SentAt               time.Time
	Snippet              string
	Labels               []string
	HasAttachments       bool
	SizeEstimate         int64
	DeletedAt            *time.Time
	Body                 string
	BodyText             string
	BodyHTML             string
	Headers              map[string]string
	Attachments          []APIAttachment
}

// APIAttachment represents attachment metadata for API responses.
type APIAttachment struct {
	ID          int64
	Filename    string
	MimeType    string
	Size        int64
	ContentHash string
	URL         string
}

// ListMessages returns a paginated list of messages with batch-loaded
// recipients and labels.
func (s *Store) ListMessages(offset, limit int) ([]APIMessage, int64, error) {
	return s.ListMessagesContext(context.Background(), offset, limit)
}

// ListMessagesContext is the context-aware form of ListMessages. Request
// paths pass the request context so the count, list, and hydration queries
// carry the request_id for SQL logging and are cancelled together when the
// request is abandoned or times out.
func (s *Store) ListMessagesContext(ctx context.Context, offset, limit int) ([]APIMessage, int64, error) {
	// Get total count. Use the canonical live-messages predicate so
	// dedup-hidden rows (deleted_at) are excluded alongside source-
	// deleted rows.
	var total int64
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM messages WHERE "+LiveMessagesWhere("", true),
	).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	// Query messages with sender info
	query := fmt.Sprintf(`
		SELECT
			m.id,
			m.source_id,
			COALESCE(m.source_message_id, '') as source_message_id,
			COALESCE(m.conversation_id, 0) as conversation_id,
			COALESCE(c.source_conversation_id, '') as source_conversation_id,
			COALESCE(m.subject, '') as subject,
			COALESCE(m.message_type, '') as message_type,
			%s,
			COALESCE(m.sent_at, m.received_at, m.internal_date) as sent_at,
			COALESCE(m.snippet, '') as snippet,
			m.has_attachments,
			m.size_estimate
		FROM messages m
		LEFT JOIN message_recipients mr ON mr.id = (
			SELECT mr2.id FROM message_recipients mr2
			WHERE mr2.message_id = m.id AND mr2.recipient_type = 'from'
			ORDER BY mr2.id LIMIT 1
		)
		LEFT JOIN participants p ON p.id = COALESCE(m.sender_id, mr.participant_id)
		LEFT JOIN conversations c ON c.id = m.conversation_id
		WHERE %s
		ORDER BY COALESCE(m.sent_at, m.received_at, m.internal_date) DESC, m.id DESC
		LIMIT ? OFFSET ?
	`, participantSummarySenderSQL, LiveMessagesWhere("m", true))

	rows, err := s.db.QueryContext(ctx, query, limit, offset)
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
	if err := s.batchPopulateContext(ctx, messages, ids); err != nil {
		return nil, 0, err
	}

	return messages, total, nil
}

// ErrMessageNotFound is returned by GetMessage when no message row
// matches the given ID. Wrapped via fmt.Errorf("...: %w", ...) so
// callers can use errors.Is to distinguish absence from real DB errors.
var ErrMessageNotFound = errors.New("message not found")

// GetMessage returns a single message with full details.
// Only this method accesses message_bodies (single PK lookup).
func (s *Store) GetMessage(id int64) (*APIMessage, error) {
	return s.GetMessageContext(context.Background(), id)
}

// GetMessageContext is the context-aware form of GetMessage. Request paths
// pass the request context so the base row, recipient, label, body, and
// attachment queries carry the request_id for SQL logging and cancel together.
func (s *Store) GetMessageContext(ctx context.Context, id int64) (*APIMessage, error) {
	query := fmt.Sprintf(`
		SELECT
			m.id,
			m.source_id,
			COALESCE(m.source_message_id, '') as source_message_id,
			COALESCE(m.conversation_id, 0) as conversation_id,
			COALESCE(c.source_conversation_id, '') as source_conversation_id,
			COALESCE(m.subject, '') as subject,
			COALESCE(m.message_type, '') as message_type,
			%s,
			COALESCE(m.sent_at, m.received_at, m.internal_date) as sent_at,
			COALESCE(m.snippet, '') as snippet,
			m.has_attachments,
			m.size_estimate,
			m.deleted_from_source_at
		FROM messages m
		LEFT JOIN message_recipients mr ON mr.id = (
			SELECT mr2.id FROM message_recipients mr2
			WHERE mr2.message_id = m.id AND mr2.recipient_type = 'from'
			ORDER BY mr2.id LIMIT 1
		)
		LEFT JOIN participants p ON p.id = COALESCE(m.sender_id, mr.participant_id)
		LEFT JOIN conversations c ON c.id = m.conversation_id
		WHERE m.id = ?
	`, participantSummarySenderSQL)

	var m APIMessage
	// sentAt is a COALESCE expression; use nullableTimestamp so
	// SQLite TEXT results parse correctly. deletedAt is a real
	// TIMESTAMP column but routing it through the same scanner
	// keeps the API consistent and tolerant of either driver.
	var sentAt, deletedAt nullableTimestamp
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&m.ID,
		&m.SourceID,
		&m.SourceMessageID,
		&m.ConversationID,
		&m.SourceConversationID,
		&m.Subject,
		&m.MessageType,
		&m.From,
		&m.FromEmail,
		&m.FromName,
		&m.FromPhone,
		&sentAt,
		&m.Snippet,
		&m.HasAttachments,
		&m.SizeEstimate,
		&deletedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("message %d: %w", id, ErrMessageNotFound)
	}
	if err != nil {
		return nil, err
	}
	if sentAt.Valid {
		m.SentAt = sentAt.Time
	}
	if deletedAt.Valid {
		t := deletedAt.Time
		m.DeletedAt = &t
	}

	// Get recipients (single message, per-row is fine)
	m.To, err = s.getRecipients(ctx, m.ID, "to")
	if err != nil {
		return nil, err
	}
	m.Cc, err = s.getRecipients(ctx, m.ID, "cc")
	if err != nil {
		return nil, err
	}
	m.Bcc, err = s.getRecipients(ctx, m.ID, "bcc")
	if err != nil {
		return nil, err
	}

	// Get labels (single message, per-row is fine)
	m.Labels, err = s.getLabels(ctx, m.ID)
	if err != nil {
		return nil, err
	}

	// Get body (single PK lookup — only place we touch message_bodies)
	var bodyText, bodyHTML sql.NullString
	err = s.db.QueryRowContext(ctx, "SELECT body_text, body_html FROM message_bodies WHERE message_id = ?", id).Scan(&bodyText, &bodyHTML)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get message body: %w", err)
	}
	m.BodyText = nullStringValue(bodyText)
	m.BodyHTML = nullStringValue(bodyHTML)
	if m.BodyText != "" {
		m.Body = m.BodyText
	} else {
		m.Body = m.BodyHTML
	}

	// Get attachments
	attRows, err := s.db.QueryContext(ctx, "SELECT id, COALESCE(filename, ''), COALESCE(mime_type, ''), COALESCE(size, 0), COALESCE(content_hash, ''), storage_path FROM attachments WHERE message_id = ?", id)
	if err != nil {
		return nil, fmt.Errorf("get attachments: %w", err)
	}
	defer func() { _ = attRows.Close() }()
	for attRows.Next() {
		var att APIAttachment
		var storagePath string
		if err := attRows.Scan(&att.ID, &att.Filename, &att.MimeType, &att.Size, &att.ContentHash, &storagePath); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		if strings.HasPrefix(storagePath, "http://") || strings.HasPrefix(storagePath, "https://") {
			att.ContentHash = ""
			att.URL = storagePath
		}
		m.Attachments = append(m.Attachments, att)
	}
	if err := attRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attachments: %w", err)
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
	return s.GetMessagesSummariesByIDsContext(context.Background(), ids)
}

// GetMessagesSummariesByIDsContext is the context-aware form of
// GetMessagesSummariesByIDs. Request paths pass the request context so the
// summary and hydration queries carry the request_id for SQL logging and are
// cancelled together with the request.
func (s *Store) GetMessagesSummariesByIDsContext(ctx context.Context, ids []int64) ([]APIMessage, error) {
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
			m.source_id,
			COALESCE(m.source_message_id, '') as source_message_id,
			COALESCE(m.conversation_id, 0) as conversation_id,
			COALESCE(c.source_conversation_id, '') as source_conversation_id,
			COALESCE(m.subject, '') as subject,
			COALESCE(m.message_type, '') as message_type,
			%s,
			COALESCE(m.sent_at, m.received_at, m.internal_date) as sent_at,
			COALESCE(m.snippet, '') as snippet,
			m.has_attachments,
			m.size_estimate
		FROM messages m
		LEFT JOIN message_recipients mr ON mr.id = (
			SELECT mr2.id FROM message_recipients mr2
			WHERE mr2.message_id = m.id AND mr2.recipient_type = 'from'
			ORDER BY mr2.id LIMIT 1
		)
		LEFT JOIN participants p ON p.id = COALESCE(m.sender_id, mr.participant_id)
		LEFT JOIN conversations c ON c.id = m.conversation_id
		WHERE m.id IN (%s) AND %s
	`, participantSummarySenderSQL, strings.Join(placeholders, ","), LiveMessagesWhere("m", true))
	rows, err := s.db.QueryContext(ctx, q, args...)
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
	if err := s.batchPopulateContext(ctx, messages, foundIDs); err != nil {
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

// SearchMessages searches messages using full-text search, with
// batch-loaded recipients and labels. The raw query string is split on
// whitespace into TextTerms and the work is delegated to
// SearchMessagesQuery so both call sites share one FTS-argument
// pipeline. Previously this function bound the raw user string straight
// into FTSSearchClause's placeholder, which on PostgreSQL fed
// to_tsquery un-escaped input (whitespace and metacharacters in user
// queries broke the parser) and on SQLite let FTS5 metacharacters
// reach the MATCH parser. Routing through BuildFTSArg sanitizes per
// dialect and reuses the FALSE fallback for tokenless inputs.
func (s *Store) SearchMessages(query string, offset, limit int) ([]APIMessage, int64, error) {
	return s.SearchMessagesContext(context.Background(), query, offset, limit)
}

// SearchMessagesContext is the context-aware form of SearchMessages. The
// context is threaded to the underlying SQLite driver so an abandoned or
// timed-out request aborts the query instead of running to completion.
func (s *Store) SearchMessagesContext(
	ctx context.Context, query string, offset, limit int,
) ([]APIMessage, int64, error) {
	terms := strings.Fields(query)
	if len(terms) == 0 {
		// Whitespace-only / empty input: no search performed. Returning
		// every row (the "no FTS filter applied" interpretation) would
		// be a startling UX change vs. the prior behavior, where empty
		// queries errored at the FTS parser. Treat as "no matches".
		return []APIMessage{}, 0, nil
	}
	return s.SearchMessagesQueryContext(
		ctx, &search.Query{TextTerms: terms}, offset, limit,
	)
}

// SearchMessagesQuery searches messages using a parsed query with
// support for structured operators (from:, to:, label:, etc.).
func (s *Store) SearchMessagesQuery(
	q *search.Query, offset, limit int,
) ([]APIMessage, int64, error) {
	return s.SearchMessagesQueryContext(context.Background(), q, offset, limit)
}

// SearchMessagesQueryContext is the context-aware form of
// SearchMessagesQuery. Request paths pass the request context so query
// cancellation (client disconnect or server-side timeout) stops the scan.
func (s *Store) SearchMessagesQueryContext(
	ctx context.Context, q *search.Query, offset, limit int,
) ([]APIMessage, int64, error) {
	return s.searchMessagesQueryImpl(ctx, q, offset, limit, s.fts5Available)
}

// searchMessagesQueryImpl runs the actual query. The ftsAvailable flag is
// taken as an explicit parameter so the runtime FTS-error fallback
// (searchMessagesQueryNoFTS) can force the LIKE path even when
// s.fts5Available was true at startup.
func (s *Store) searchMessagesQueryImpl(
	ctx context.Context, q *search.Query, offset, limit int, ftsAvailable bool,
) ([]APIMessage, int64, error) {
	var conditions []string
	var args []any

	conditions = append(conditions, LiveMessagesWhere("m", true))

	// FTS text terms. ftsEnabled is the authoritative signal that FTS is
	// active — ftsJoin may be empty on dialects (e.g. PostgreSQL) whose
	// tsvector lives on the main table and needs no extra join.
	ftsEnabled := len(q.TextTerms) > 0 && ftsAvailable
	var ftsJoin, ftsOrder, ftsExpr string
	var ftsOrderArgCount int
	if ftsEnabled {
		ftsExpr = s.dialect.BuildFTSArg(q.TextTerms)
		if ftsExpr == "" {
			// Every text term reduced to nothing usable (punctuation-
			// only input like "!!!" or "---"). Dispatching the dialect's
			// FTS WHERE here would feed PG's to_tsquery an empty string
			// ("text-search query doesn't contain lexemes") and SQLite's
			// FTS5 MATCH a syntax error. Substitute FALSE so the query
			// returns zero rows without ever touching the FTS function,
			// matching the (expr="FALSE", arg="") fallback that the
			// query package's BuildFTSTerm uses for the same input.
			conditions = append(conditions, "FALSE")
			ftsEnabled = false
		} else {
			join, where, orderBy, orderArgCount := s.dialect.FTSSearchClause()
			ftsJoin = join
			ftsOrder = orderBy
			ftsOrderArgCount = orderArgCount
			conditions = append(conditions, where)
			args = append(args, ftsExpr)
		}
	} else if len(q.TextTerms) > 0 {
		// FTS unavailable but the caller still has free-text terms.
		// Match each term against subject OR snippet so the no-FTS
		// path catches snippet hits, not just subjects. Per CLAUDE.md,
		// search queries never scan message_bodies.
		added := 0
		for _, term := range q.TextTerms {
			// Skip terms with no searchable token (empty string,
			// punctuation-only). hasFTSToken is the same predicate the
			// FTS path uses via BuildFTSArg, so both paths agree on what
			// is "tokenless". Without this, term=="" becomes LIKE '%%'
			// and matches every message instead of nothing.
			if !hasFTSToken(term) {
				continue
			}
			like := "%" + escapeLike(strings.ToLower(term)) + "%"
			conditions = append(conditions,
				`(LOWER(m.subject) LIKE ? ESCAPE '\' OR LOWER(m.snippet) LIKE ? ESCAPE '\')`)
			args = append(args, like, like)
			added++
		}
		if added == 0 {
			// All terms were tokenless: substitute FALSE so the LIKE
			// fallback returns zero rows, matching the FTS path.
			conditions = append(conditions, "FALSE")
		}
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

	// subject: filter — LOWER on both sides for PG portability.
	// SQLite's default LIKE is ASCII-case-insensitive; PG's is strict-
	// case, so a bare `m.subject LIKE '%invoice%'` returned zero hits
	// against "Invoice from acme" on PG. Every other LIKE in this
	// function already wraps with LOWER.
	for _, term := range q.SubjectTerms {
		// An empty subject term would build LIKE '%%' and match every
		// message; skip it. (The parser already drops empties; this guards
		// directly-constructed queries.) Punctuation-only terms are kept —
		// the subject filter is a literal substring match, not FTS.
		if strings.TrimSpace(term) == "" {
			continue
		}
		conditions = append(conditions,
			`LOWER(m.subject) LIKE LOWER(?) ESCAPE '\'`)
		args = append(args, "%"+escapeLike(strings.ToLower(term))+"%")
	}

	// message_type: / message_type= filter.
	if len(q.MessageTypes) > 0 {
		placeholders := make([]string, len(q.MessageTypes))
		for i, typ := range q.MessageTypes {
			placeholders[i] = "?"
			args = append(args, typ)
		}
		conditions = append(conditions,
			"m.message_type IN ("+strings.Join(placeholders, ",")+")")
	}

	// Account scoping (in: / API account/collection filter). The HTTP
	// search endpoints resolve an account or collection to its source IDs
	// and put them here; without this condition the scope is validated at
	// the front door and then silently dropped, returning every account's
	// messages. Uses the same IN-placeholder style as message_type so it
	// works on both SQLite and PostgreSQL after Rebind.
	if len(q.AccountIDs) > 0 {
		placeholders := make([]string, len(q.AccountIDs))
		for i, id := range q.AccountIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		conditions = append(conditions,
			"m.source_id IN ("+strings.Join(placeholders, ",")+")")
	}

	// has:attachment
	if q.HasAttachment != nil && *q.HasAttachment {
		conditions = append(conditions,
			s.dialect.BoolTrueExpr("m.has_attachments"))
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
	// PostgreSQL compares typed TIMESTAMPTZ values directly. SQLite archives can
	// contain both UTC and offset-bearing timestamp strings, so compare Julian
	// day values instead of their lexical encodings. Normalizing the bound to UTC
	// keeps the argument stable on both backends. [cr2-9]
	timestampExpr := "COALESCE(m.sent_at, m.received_at, m.internal_date)"
	boundExpr := "?"
	if !s.IsPostgreSQL() {
		timestampExpr = "julianday(" + timestampExpr + ")"
		boundExpr = "julianday(?)"
	}
	if q.AfterDate != nil {
		conditions = append(conditions, timestampExpr+" >= "+boundExpr)
		args = append(args, q.AfterDate.UTC())
	}
	if q.BeforeDate != nil {
		conditions = append(conditions, timestampExpr+" < "+boundExpr)
		args = append(args, q.BeforeDate.UTC())
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
	if err := s.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		if ftsEnabled && ctx.Err() == nil {
			return s.searchMessagesQueryNoFTS(ctx, q, offset, limit)
		}
		return nil, 0, fmt.Errorf("count search results: %w", err)
	}

	// Results query.
	orderBy := "COALESCE(m.sent_at, m.received_at, m.internal_date) DESC, m.id DESC"
	if ftsEnabled {
		orderBy = ftsOrder + ", " + orderBy
	}
	searchSQL := fmt.Sprintf(`
		SELECT
			m.id,
			m.source_id,
			COALESCE(m.source_message_id, '') as source_message_id,
			COALESCE(m.conversation_id, 0) as conversation_id,
			COALESCE(c.source_conversation_id, '') as source_conversation_id,
			COALESCE(m.subject, '') as subject,
			COALESCE(m.message_type, '') as message_type,
			%s,
			COALESCE(m.sent_at, m.received_at, m.internal_date) as sent_at,
			COALESCE(m.snippet, '') as snippet,
			m.has_attachments,
			m.size_estimate
		FROM messages m
		%s
		LEFT JOIN message_recipients mr ON mr.id = (
			SELECT mr2.id FROM message_recipients mr2
			WHERE mr2.message_id = m.id AND mr2.recipient_type = 'from'
			ORDER BY mr2.id LIMIT 1
		)
		LEFT JOIN participants p ON p.id = COALESCE(m.sender_id, mr.participant_id)
		LEFT JOIN conversations c ON c.id = m.conversation_id
		WHERE %s
		ORDER BY %s
		LIMIT ? OFFSET ?
	`, participantSummarySenderSQL, ftsJoin, whereClause, orderBy)

	// If the dialect's order-by fragment has ? placeholders, bind the FTS
	// expression that many extra times — right after the WHERE args and
	// before LIMIT/OFFSET so Rebind assigns them the correct positions.
	resultArgs := make([]any, 0, len(args)+ftsOrderArgCount+2)
	resultArgs = append(resultArgs, args...)
	for range ftsOrderArgCount {
		resultArgs = append(resultArgs, ftsExpr)
	}
	resultArgs = append(resultArgs, limit, offset)
	rows, err := s.db.QueryContext(ctx, searchSQL, resultArgs...)
	if err != nil {
		// FTS5 not available -- fall back if we used it. Skip the fallback
		// when the context was cancelled: the error is the abort we asked
		// for, not an FTS capability problem, and re-running would ignore it.
		if ftsEnabled && ctx.Err() == nil {
			return s.searchMessagesQueryNoFTS(ctx, q, offset, limit)
		}
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	messages, ids, err := scanMessageRows(rows)
	if err != nil {
		return nil, 0, err
	}

	if len(ids) > 0 {
		if err := s.batchPopulateContext(ctx, messages, ids); err != nil {
			return nil, 0, err
		}
	}

	return messages, total, nil
}

// searchMessagesQueryNoFTS retries the query with the LIKE-based text
// branch. Used when the FTS path errored at runtime even though the
// startup probe said FTS5 was available; passing ftsAvailable=false
// forces the subject+snippet LIKE branch in searchMessagesQueryImpl.
func (s *Store) searchMessagesQueryNoFTS(
	ctx context.Context, q *search.Query, offset, limit int,
) ([]APIMessage, int64, error) {
	return s.searchMessagesQueryImpl(ctx, q, offset, limit, false)
}

// escapeLike escapes SQL LIKE special characters (%, _) so they are
// matched literally. The escaped string should be used with ESCAPE '\'.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// searchMessagesLike is a fallback search using LIKE with batch-loaded
// recipients and labels. Wraps both sides in LOWER for PG portability —
// SQLite's ASCII LIKE is case-insensitive by default but PG's is strict.
func (s *Store) searchMessagesLike(query string, offset, limit int) ([]APIMessage, int64, error) {
	likePattern := "%" + escapeLike(strings.ToLower(query)) + "%"

	countQuery := fmt.Sprintf(`
		SELECT COUNT(*) FROM messages
		WHERE %s
		AND (LOWER(subject) LIKE ? ESCAPE '\' OR LOWER(snippet) LIKE ? ESCAPE '\')
	`, LiveMessagesWhere("", true))
	var total int64
	if err := s.db.QueryRow(countQuery, likePattern, likePattern).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count search results: %w", err)
	}

	searchQuery := fmt.Sprintf(`
		SELECT
			m.id,
			m.source_id,
			COALESCE(m.source_message_id, '') as source_message_id,
			COALESCE(m.conversation_id, 0) as conversation_id,
			COALESCE(c.source_conversation_id, '') as source_conversation_id,
			COALESCE(m.subject, '') as subject,
			COALESCE(m.message_type, '') as message_type,
			%s,
			COALESCE(m.sent_at, m.received_at, m.internal_date) as sent_at,
			COALESCE(m.snippet, '') as snippet,
			m.has_attachments,
			m.size_estimate
		FROM messages m
		LEFT JOIN message_recipients mr ON mr.id = (
			SELECT mr2.id FROM message_recipients mr2
			WHERE mr2.message_id = m.id AND mr2.recipient_type = 'from'
			ORDER BY mr2.id LIMIT 1
		)
		LEFT JOIN participants p ON p.id = COALESCE(m.sender_id, mr.participant_id)
		LEFT JOIN conversations c ON c.id = m.conversation_id
		WHERE %s
		AND (LOWER(m.subject) LIKE ? ESCAPE '\' OR LOWER(m.snippet) LIKE ? ESCAPE '\')
		ORDER BY COALESCE(m.sent_at, m.received_at, m.internal_date) DESC, m.id DESC
		LIMIT ? OFFSET ?
	`, participantSummarySenderSQL, LiveMessagesWhere("m", true))

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

// nullableTimestamp is a sql.Scanner that accepts time.Time (pgx/v5
// stdlib for TIMESTAMP/TIMESTAMPTZ), string, []byte (SQLite for
// computed COALESCE expressions whose declared datetime affinity is
// lost), and nil. The sql.NullTime path that previously covered both
// drivers is not sufficient for SQLite: when SELECT COALESCE(...) is
// used over datetime columns, go-sqlite3 may surface the value as
// TEXT because the COALESCE result has no column type info, and
// NullTime's Scan rejects strings.
type nullableTimestamp struct {
	Time  time.Time
	Valid bool
}

// Scan implements sql.Scanner. Strings and []byte are parsed via
// parseSQLiteTime which already enumerates every layout SQLite emits;
// unparseable values are treated as "not valid" rather than a hard
// error so a single malformed row does not abort an entire listing.
func (n *nullableTimestamp) Scan(src any) error {
	if src == nil {
		n.Time, n.Valid = time.Time{}, false
		return nil
	}
	switch v := src.(type) {
	case time.Time:
		n.Time, n.Valid = v, !v.IsZero()
		return nil
	case string:
		t := parseSQLiteTime(v)
		n.Time, n.Valid = t, !t.IsZero()
		return nil
	case []byte:
		t := parseSQLiteTime(string(v))
		n.Time, n.Valid = t, !t.IsZero()
		return nil
	default:
		return fmt.Errorf("nullableTimestamp: unsupported scan type %T", src)
	}
}

// scanMessageRows scans the standard message row set
// (id, source_id, source_message_id, conversation_id, source_conversation_id, subject,
// message_type, from_display, from_email, from_name, from_phone, sent_at,
// snippet, has_attachments, size_estimate). All SELECT statements that feed
// this scanner must produce the same column order.
// Timestamps go through nullableTimestamp because the sent_at column
// is a COALESCE(m.sent_at, m.received_at, m.internal_date) computed
// expression with no declared datetime type, which on SQLite can come
// back as TEXT and trip sql.NullTime.Scan. pgx/v5 still delivers
// time.Time, which nullableTimestamp also handles.
func scanMessageRows(rows *loggedRows) ([]APIMessage, []int64, error) {
	var messages []APIMessage
	var ids []int64
	for rows.Next() {
		var m APIMessage
		var sentAt nullableTimestamp
		err := rows.Scan(
			&m.ID,
			&m.SourceID,
			&m.SourceMessageID,
			&m.ConversationID,
			&m.SourceConversationID,
			&m.Subject,
			&m.MessageType,
			&m.From,
			&m.FromEmail,
			&m.FromName,
			&m.FromPhone,
			&sentAt,
			&m.Snippet,
			&m.HasAttachments,
			&m.SizeEstimate,
		)
		if err != nil {
			return nil, nil, err
		}
		if sentAt.Valid {
			m.SentAt = sentAt.Time
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
	return s.batchPopulateContext(context.Background(), messages, ids)
}

// batchPopulateContext is the context-aware form of batchPopulate. Request
// paths pass the request context so recipient/label hydration is cancelled
// (and the request_id carried on the context reaches the SQL logger) alongside
// the count/result queries, instead of running to completion on a background
// context after the request budget is exceeded.
func (s *Store) batchPopulateContext(ctx context.Context, messages []APIMessage, ids []int64) error {
	recipientMap, err := s.batchGetRecipients(ctx, ids, "to")
	if err != nil {
		return err
	}
	ccMap, err := s.batchGetRecipients(ctx, ids, "cc")
	if err != nil {
		return err
	}
	bccMap, err := s.batchGetRecipients(ctx, ids, "bcc")
	if err != nil {
		return err
	}
	labelMap, err := s.batchGetLabels(ctx, ids)
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
func (s *Store) batchGetRecipients(ctx context.Context, messageIDs []int64, recipientType string) (map[int64][]string, error) {
	if len(messageIDs) == 0 {
		return map[int64][]string{}, nil
	}

	placeholders := make([]string, len(messageIDs))
	args := make([]any, 0, len(messageIDs)+1)
	for i, id := range messageIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	args = append(args, recipientType)

	query := fmt.Sprintf(`
		SELECT mr.message_id, %s
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id IN (%s) AND mr.recipient_type = ?
	`, participantDisplaySQL, strings.Join(placeholders, ","))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("batch get recipients: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[int64][]string, len(messageIDs))
	for rows.Next() {
		var msgID int64
		var display string
		if err := rows.Scan(&msgID, &display); err != nil {
			return nil, fmt.Errorf("scan recipient: %w", err)
		}
		if display != "" {
			result[msgID] = append(result[msgID], display)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recipients: %w", err)
	}
	return result, nil
}

// batchGetLabels loads labels for multiple messages in a single query.
func (s *Store) batchGetLabels(ctx context.Context, messageIDs []int64) (map[int64][]string, error) {
	if len(messageIDs) == 0 {
		return map[int64][]string{}, nil
	}

	placeholders := make([]string, len(messageIDs))
	args := make([]any, 0, len(messageIDs))
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

	rows, err := s.db.QueryContext(ctx, query, args...)
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

func (s *Store) getRecipients(ctx context.Context, messageID int64, recipientType string) ([]string, error) {
	query := fmt.Sprintf(`
		SELECT %s
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id = ? AND mr.recipient_type = ?
	`, participantDisplaySQL)
	rows, err := s.db.QueryContext(ctx, query, messageID, recipientType)
	if err != nil {
		return nil, fmt.Errorf("get recipients: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var recipients []string
	for rows.Next() {
		var display string
		if err := rows.Scan(&display); err != nil {
			return nil, fmt.Errorf("scan recipient: %w", err)
		}
		if display != "" {
			recipients = append(recipients, display)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recipients: %w", err)
	}
	return recipients, nil
}

func (s *Store) getLabels(ctx context.Context, messageID int64) ([]string, error) {
	query := `
		SELECT l.name
		FROM message_labels ml
		JOIN labels l ON l.id = ml.label_id
		WHERE ml.message_id = ?
	`
	rows, err := s.db.QueryContext(ctx, query, messageID)
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
