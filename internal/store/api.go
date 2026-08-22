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
	BodyOmitted          bool
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
	attRows, err := s.db.QueryContext(ctx, "SELECT id, COALESCE(filename, ''), COALESCE(mime_type, ''), COALESCE(size, 0), COALESCE(content_hash, ''), storage_path, COALESCE(source_attachment_id, '') FROM attachments WHERE message_id = ?", id)
	if err != nil {
		return nil, fmt.Errorf("get attachments: %w", err)
	}
	defer func() { _ = attRows.Close() }()
	for attRows.Next() {
		var att APIAttachment
		var storagePath, sourceAttachmentID string
		if err := attRows.Scan(
			&att.ID, &att.Filename, &att.MimeType, &att.Size, &att.ContentHash,
			&storagePath, &sourceAttachmentID,
		); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		if strings.HasPrefix(storagePath, "http://") || strings.HasPrefix(storagePath, "https://") {
			att.ContentHash = ""
			att.URL = storagePath
		} else if att.ContentHash == "" &&
			(strings.HasPrefix(sourceAttachmentID, "discord:") || strings.HasPrefix(sourceAttachmentID, "slack:")) {
			// A hashless Discord/Slack row whose storage path validates as
			// a trusted CAS path is a duplicate-content alias; re-derive
			// its hash so the attachment stays accessible. The provider
			// gate is load-bearing: a Beeper hashless local path means
			// pending/untrusted and must stay hashless (see
			// TestBeeperHashlessLocalPathRemainsPending).
			if pathHash, ok := casPathHash(storagePath); ok {
				att.ContentHash = pathHash
			}
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
// case remains bounded regardless of len(ids). This is the
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

	// The FTS index covers source-deleted messages too: soft deletion only
	// stamps deleted_from_source_at, leaving the FTS5 row (and the PG
	// tsvector on the surviving messages row) intact. Honoring the
	// caller-requested scope here lets the explore lexical resolver search
	// deleted or unrestricted populations; every caller that leaves the
	// scope at its zero value keeps the historical active-only behavior.
	switch q.DeletionScope {
	case search.DeletionScopeDeleted:
		conditions = append(conditions, SourceDeletedMessagesWhere("m"))
	case search.DeletionScopeAny:
		conditions = append(conditions, LiveMessagesWhere("m", false))
	case search.DeletionScopeActive:
		conditions = append(conditions, LiveMessagesWhere("m", true))
	default:
		// Unknown scope values fail closed to the narrowest population.
		conditions = append(conditions, LiveMessagesWhere("m", true))
	}

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

// requiredTimestamp rejects missing or malformed values. Feed watermarks are
// cursor state, so silently treating corruption as a zero timestamp would make
// consumers replay or skip data.
type requiredTimestamp struct {
	Time time.Time
}

func (r *requiredTimestamp) Scan(src any) error {
	var timestamp nullableTimestamp
	if err := timestamp.Scan(src); err != nil {
		return fmt.Errorf("content_changed_at: %w", err)
	}
	if !timestamp.Valid {
		return errors.New("content_changed_at is NULL or is not a valid timestamp")
	}
	r.Time = timestamp.Time
	return nil
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

// batchQueryIDChunk caps how many message ids batchGetRecipients and
// batchGetLabels bind into a single IN-list statement. SQLite refuses a
// statement carrying more than 32766 bound parameters by default, and one id
// is one parameter here; eval's FTS mode over-fetches a ranked page well past
// that at a large -n (the same over-fetch plan documented on rankedKeys).
// Mirrors messageSummaryIDChunk in internal/query — same limit, same cause,
// a different package's copy of the batch-hydration pattern.
const batchQueryIDChunk = 500

// batchGetRecipients loads recipients for multiple messages, chunked to stay
// under the SQLite bound-parameter ceiling.
func (s *Store) batchGetRecipients(ctx context.Context, messageIDs []int64, recipientType string) (map[int64][]string, error) {
	if len(messageIDs) == 0 {
		return map[int64][]string{}, nil
	}
	result := make(map[int64][]string, len(messageIDs))
	for start := 0; start < len(messageIDs); start += batchQueryIDChunk {
		end := min(start+batchQueryIDChunk, len(messageIDs))
		if err := s.fetchRecipientsInto(ctx, messageIDs[start:end], recipientType, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// fetchRecipientsInto runs one chunk's IN-list query and merges its rows
// into result, keyed by message id. Every id belongs to exactly one chunk,
// so no key is ever written by more than one call.
func (s *Store) fetchRecipientsInto(
	ctx context.Context, messageIDs []int64, recipientType string, result map[int64][]string,
) error {
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
		return fmt.Errorf("batch get recipients: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var msgID int64
		var display string
		if err := rows.Scan(&msgID, &display); err != nil {
			return fmt.Errorf("scan recipient: %w", err)
		}
		if display != "" {
			result[msgID] = append(result[msgID], display)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate recipients: %w", err)
	}
	return nil
}

// batchGetLabels loads labels for multiple messages, chunked to stay under
// the SQLite bound-parameter ceiling.
func (s *Store) batchGetLabels(ctx context.Context, messageIDs []int64) (map[int64][]string, error) {
	if len(messageIDs) == 0 {
		return map[int64][]string{}, nil
	}
	result := make(map[int64][]string, len(messageIDs))
	for start := 0; start < len(messageIDs); start += batchQueryIDChunk {
		end := min(start+batchQueryIDChunk, len(messageIDs))
		if err := s.fetchLabelsInto(ctx, messageIDs[start:end], result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// fetchLabelsInto runs one chunk's IN-list query and merges its rows into
// result, keyed by message id.
func (s *Store) fetchLabelsInto(ctx context.Context, messageIDs []int64, result map[int64][]string) error {
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
		return fmt.Errorf("batch get labels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var msgID int64
		var name string
		if err := rows.Scan(&msgID, &name); err != nil {
			return fmt.Errorf("scan label: %w", err)
		}
		result[msgID] = append(result[msgID], name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate labels: %w", err)
	}
	return nil
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

// ChangedMessage is one row of the content-change feed. Every field is a column
// of `messages`, but the feed's fields and the watermark's columns are not the
// same set: sender_id and metadata move the watermark without appearing here,
// and id, source_id and content_changed_at appear here without moving it — the
// first two are immutable identity and the third is the watermark itself. See
// MessagesContentColumns. Labels and recipients live in child tables the
// watermark does not cover and are deliberately absent: a consumer handed them
// here would cache them stale forever.
type ChangedMessage struct {
	ID                  int64
	SourceID            int64
	SourceMessageID     string
	ConversationID      int64
	MessageType         string
	Subject             string
	Snippet             string
	SentAt              *time.Time
	ReceivedAt          *time.Time
	InternalDate        *time.Time
	SizeEstimate        int64
	HasAttachments      bool
	AttachmentCount     int
	DeletedAt           *time.Time
	DeletedFromSourceAt *time.Time
	ContentChangedAt    time.Time
}

// ChangedMessagePage is what the feed returns. ServerTime is the DATABASE's
// clock at query time, for the caller's overlap arithmetic — read from the
// database, not from the Go process: on PostgreSQL the watermarks come from the
// database server, and comparing them against a possibly-skewed application
// clock makes the overlap advice meaningless. It is populated on an empty page
// too, which is why the feed cannot simply return a slice: a caught-up consumer
// gets no rows, and there would be nothing left to derive it from.
//
// CompleteThrough is the instant the page is complete through: every change
// committed strictly before it, at or after the requested cursor, is REACHABLE
// — in this page, or in one of the pages that follow it from this page's
// cursor. It is a bound on the feed, not a cursor, and the difference matters
// whenever the page filled: a consumer that resumes from CompleteThrough
// instead of from the last row's (ContentChangedAt, ID) skips everything
// between the two. It is the page's upper bound
// (Dialect.WatermarkBounds.CommitBound), never after ServerTime, and the gap
// between the two is how stale the bound is — the oldest in-flight write
// transaction's own age on PostgreSQL, and on SQLite the age of the last proof
// that the database was quiescent, which is an upper bound on that age rather
// than a measurement of it (see WatermarkBounds). An empty page carries the
// bound too, and that is the point: without it a feed held back by a long
// transaction is indistinguishable from a caught-up one.
//
// A zero CompleteThrough means no bound has been established at all — the store
// has never yet proved that anything has committed (see
// SQLiteDialect.ReadWatermarkBounds). Such a page is empty by construction. It
// is a state, not an instant: do not subtract it from ServerTime.
type ChangedMessagePage struct {
	Messages        []ChangedMessage
	ServerTime      time.Time
	CompleteThrough time.Time
}

// ChangedMessagesCursor is a position in the content-change feed: the point the
// next page starts from.
//
// It has two shapes, and the difference is not cosmetic. ChangedMessagesAfter
// resumes a walk that stopped part-way through an instant, so it names a row
// and must EXCLUDE it. ChangedMessagesFrom names an instant and must INCLUDE
// every row stamped there, whatever its id.
//
// The second one is a shape rather than a magic id value because no id value
// can express it. The keyset tiebreak is `id > n`, and there is no int64 that
// sorts below every legal id: `id` is SQLite's INTEGER PRIMARY KEY — the rowid
// — and BIGINT on PostgreSQL, and the schema constrains it no further, so 0,
// negatives, and math.MinInt64 itself are all legal (the backfill tests seed
// both ends of the range deliberately). Any sentinel value would silently drop
// the row that happened to carry it, which is the failure this feed exists to
// rule out.
//
// The zero value is the start of the archive: the start of the zero instant,
// which sorts at or below every stored watermark.
type ChangedMessagesCursor struct {
	at time.Time
	// afterID is meaningful only when afterRow is set; the constructors are
	// the only way to set either, so no caller can build the half-state where
	// an id is carried but not honoured.
	afterID  int64
	afterRow bool
}

// ChangedMessagesFrom returns the position at the START of at: every row
// stamped at that instant is still ahead of it.
func ChangedMessagesFrom(at time.Time) ChangedMessagesCursor {
	return ChangedMessagesCursor{at: at}
}

// ChangedMessagesAfter returns the position just after the row (at, id), which
// is what a consumer resumes from when a page ended mid-instant.
func ChangedMessagesAfter(at time.Time, id int64) ChangedMessagesCursor {
	return ChangedMessagesCursor{at: at, afterID: id, afterRow: true}
}

// At is the instant half of the position.
func (c ChangedMessagesCursor) At() time.Time { return c.at }

// AfterID reports the id half of the position, and whether there is one: a
// position at the start of an instant has none, and that is the difference
// between including the rows stamped there and excluding some of them.
func (c ChangedMessagesCursor) AfterID() (int64, bool) { return c.afterID, c.afterRow }

// changedMessagesQuery is the content-change feed's page query, for a cursor
// that resumes after a particular row. changedMessagesFromInstantQuery is the
// same query for a cursor at the start of an instant: identical but for the id
// tiebreak, which it drops rather than binding, because there is no value that
// would let every id at that instant through (see ChangedMessagesCursor).
//
// The lower bound is spelled `>= ? AND (> ? OR id > ?)` rather than the more
// obvious `> ? OR (= ? AND id > ?)`. The two select identical rows. Measured on
// SQLite 3.53.2, the OR form planned as a full index walk ("SCAN messages USING
// INDEX") while the >= form seeked ("SEARCH messages USING INDEX
// idx_messages_content_changed_at (content_changed_at>?)"); on SQLite 3.45.1
// both forms seeked. Plans vary by version and by table statistics — the
// small-table plan is the same either way, which is how the wrong predicate
// nearly shipped — so this is the form that has never been observed to scan,
// not a claim about every SQLite.
//
// The upper bound is not an optimisation, it is the correctness half: the page
// stops strictly below the oldest write that could still commit
// (WatermarkBounds.CommitBound), so the cursor cannot come to rest above a
// change that is stamped but not yet published by any write the bound can see
// (which writes those are: docs/api-server.md). Bounding at the database clock
// instead — which is what this query used to do — leaves the loss window open,
// because the clock says which instants have been REACHED and nothing about
// which are still open for COMMITS. See ListChangedMessages.
//
// Single-table by design: no joins, no hydration. See ChangedMessage.
//
// No visibility filter (no LiveMessagesWhere): dedup-hidden and source-deleted
// rows are returned with their timestamps set, because a consumer mirroring the
// archive must learn about removals — and a row filtered out after the cursor
// passed it is indistinguishable from the end of a page.
const changedMessagesSelect = `
	SELECT id, source_id, COALESCE(source_message_id,''), COALESCE(conversation_id,0),
	       COALESCE(message_type,''), COALESCE(subject,''), COALESCE(snippet,''),
	       sent_at, received_at, internal_date, COALESCE(size_estimate,0),
	       COALESCE(has_attachments,FALSE), COALESCE(attachment_count,0),
	       deleted_at, deleted_from_source_at, content_changed_at
	FROM messages
	WHERE content_changed_at >= ?`

const changedMessagesBounds = `
	  AND content_changed_at < ?
	ORDER BY content_changed_at, id
	LIMIT ?`

const changedMessagesQuery = changedMessagesSelect +
	` AND (content_changed_at > ? OR id > ?)` + changedMessagesBounds

const changedMessagesFromInstantQuery = changedMessagesSelect + changedMessagesBounds

// ListChangedMessages returns messages whose content changed at or after the
// given cursor, in (content_changed_at, id) order, along with the database's
// current clock reading.
//
// The cursor carries an id as well as an instant, not a timestamp alone:
// stamps have millisecond resolution on SQLite and rapid writes share one, so a
// plain `>` drops rows written in the same instant as the cursor and a plain
// `>=` returns that instant on every call. Comparing the pair walks the index
// once. A cursor at the START of an instant (ChangedMessagesFrom) carries no id
// and is compared on the instant alone, which is how every row stamped there —
// including the ones at id 0 and below, which are legal — stays reachable.
//
// The cursor is bound through Dialect.TimestampParam for both timestamp
// placeholders. Binding a time.Time straight through is silently wrong on
// SQLite: the driver serialises it as "2024-03-04 05:06:07+00:00" while the
// column holds "2024-03-04 05:06:07.000", which sorts BELOW it, so every row
// sharing the cursor's instant is skipped — only ever visible at page
// boundaries.
//
// The page also stops strictly below the instant returned as CompleteThrough,
// which is the oldest write that could still commit, NOT the database clock.
// The distinction is the difference between a feed that loses rows and one that
// does not. Both backends stamp the watermark when the statement runs and
// publish the row when its transaction commits, so a change can be stamped in
// an instant the clock has already left and become visible only later; a page
// bounded at the clock parks the consumer's cursor above it, and it then fails
// both arms of the lower bound on every future request. Measured before this
// bound existed: MarkMessagesDeletedFromReader, which deliberately holds one
// transaction across a streamed deletion run, lost all 40 of its tombstones on
// PostgreSQL against a consumer polling the way the handler does; plain
// autocommit writes lost rows in 3 runs out of 8; and on SQLite a
// same-millisecond write on a lower id that committed after a page was read was
// stranded permanently.
//
// Bounding below the oldest write that could still commit cannot lose a row
// whose transaction the bound can see, because every uncommitted stamp is at or
// above the start of the transaction that made it. A PostgreSQL prepared
// transaction is the write it cannot see: it holds its locks with no owning
// session, so no start time is observable for it (see pgWatermarkBoundsQuery in
// dialect_pg.go). Two costs come with it. The newest changes wait for the next
// poll, as before. And the feed stops advancing for as long as any write
// transaction stays open — a connection left idle in a transaction that has
// written to the message table holds the bound still. That is why
// CompleteThrough is on the page and published by the handler: a stalled feed
// must not look like a caught-up one. A writer connected as a different
// PostgreSQL role is inside the guarantee too, at the cost of a stall or a
// refusal rather than a loss (see PostgreSQLDialect.visibilityFloor). What
// remains outside the guarantee is enumerated in exactly one place,
// docs/api-server.md's delivery contract. This comment deliberately does not
// restate that list: it has been corrected in one copy and left false in
// another too many times.
//
// A limit of zero or less asks for no rows; that is not an error.
func (s *Store) ListChangedMessages(
	ctx context.Context, since ChangedMessagesCursor, limit int,
) (ChangedMessagePage, error) {
	if limit <= 0 {
		// No page was read, so there is nothing a server_time reading could
		// honestly say about how far the consumer is caught up.
		return ChangedMessagePage{}, nil
	}

	// Read the bounds BEFORE the page query, in their own statement (constant
	// columns on the page query itself would vanish along with the rows on an
	// empty page). Before, not after: a change committed while the page query
	// runs may be invisible to that query's snapshot yet carry an earlier
	// stamp, and a consumer resuming from a reading taken afterwards would skip
	// it. A reading taken first is never later than the data it accompanies.
	//
	// PostgreSQL's bound read also verifies that `messages` resolves through the
	// connection's search_path. Keep it ahead of the NULL preflight so a missing
	// table produces that actionable diagnostic instead of a bare query error.
	bounds, err := s.dialect.ReadWatermarkBounds(ctx, s.db.DB)
	if err != nil {
		return ChangedMessagePage{}, err
	}

	// NULL watermarks are excluded by the range predicate and would otherwise
	// remain invisible forever. Detect legacy or manually-corrupted rows before
	// returning any page so consumers stop at a visible, repairable error.
	var nullWatermarkID int64
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM messages WHERE content_changed_at IS NULL LIMIT 1`,
	).Scan(&nullWatermarkID)
	if err == nil {
		return ChangedMessagePage{}, fmt.Errorf(
			"message %d has NULL content_changed_at", nullWatermarkID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ChangedMessagePage{}, fmt.Errorf("check message watermarks: %w", err)
	}

	cursor := s.dialect.TimestampParam(since.At())
	openInstant := s.dialect.TimestampParam(bounds.CommitBound)
	query, args := changedMessagesFromInstantQuery, []any{cursor, openInstant, limit}
	if sinceID, ok := since.AfterID(); ok {
		query = changedMessagesQuery
		args = []any{cursor, cursor, sinceID, openInstant, limit}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return ChangedMessagePage{}, fmt.Errorf("list changed messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	page := ChangedMessagePage{ServerTime: bounds.Now, CompleteThrough: bounds.CommitBound}
	for rows.Next() {
		var m ChangedMessage
		// Every timestamp goes through nullableTimestamp: SQLite hands back
		// TEXT for values the driver cannot coerce, and sql.NullTime rejects
		// strings.
		var sentAt, receivedAt, internalDate nullableTimestamp
		var deletedAt, deletedFromSourceAt nullableTimestamp
		var contentChangedAt requiredTimestamp
		if err := rows.Scan(
			&m.ID,
			&m.SourceID,
			&m.SourceMessageID,
			&m.ConversationID,
			&m.MessageType,
			&m.Subject,
			&m.Snippet,
			&sentAt,
			&receivedAt,
			&internalDate,
			&m.SizeEstimate,
			&m.HasAttachments,
			&m.AttachmentCount,
			&deletedAt,
			&deletedFromSourceAt,
			&contentChangedAt,
		); err != nil {
			return ChangedMessagePage{}, fmt.Errorf("scan changed message: %w", err)
		}
		m.SentAt = optionalTimestamp(sentAt)
		m.ReceivedAt = optionalTimestamp(receivedAt)
		m.InternalDate = optionalTimestamp(internalDate)
		m.DeletedAt = optionalTimestamp(deletedAt)
		m.DeletedFromSourceAt = optionalTimestamp(deletedFromSourceAt)
		m.ContentChangedAt = contentChangedAt.Time
		page.Messages = append(page.Messages, m)
	}
	if err := rows.Err(); err != nil {
		return ChangedMessagePage{}, fmt.Errorf("iterate changed messages: %w", err)
	}
	return page, nil
}

// optionalTimestamp converts a scanned nullableTimestamp to the pointer form the
// API structs use, copying the value so the pointer does not alias the scanner
// reused by the next row.
func optionalTimestamp(ts nullableTimestamp) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
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
