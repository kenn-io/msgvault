package embed

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// ErrSourceSnapshotClosed reports an attempted source read after the short
// assembly transaction was released.
var ErrSourceSnapshotClosed = errors.New("embedding source snapshot is closed")

// AssemblyPolicy is every output-affecting assembly bound. SkipMessage is an
// activation-path filter; it is evaluated before an ordinary or meeting
// document is rendered.
type AssemblyPolicy struct {
	MaxChunkRunes        int
	MaxDocumentUTF8Bytes int
	ChatGap              time.Duration
	Preprocess           PreprocessConfig
	SkipMessage          func(AssemblyMessage) bool
}

// AssemblyMessage is the source row used by the deterministic assemblers.
// LastModified remains dialect-native so the later coverage CAS can bind the
// exact value read in this transaction.
type AssemblyMessage struct {
	ID                int64
	ConversationID    int64
	MessageType       string
	Subject           string
	Body              string
	BodyTruncated     bool
	SentAt            time.Time
	SenderID          int64
	SenderDisplay     string
	LastModified      any
	SourceSequence    int64
	ConversationTitle string
}

// AssemblyParticipant is one display snapshot used by contextual headers.
type AssemblyParticipant struct {
	ID          int64
	Role        string
	DisplayName string
	Revision    any
}

// AssemblyConversation is the conversation metadata captured with the source
// rows in the same repeatable-read snapshot.
type AssemblyConversation struct {
	ID              int64
	Title           string
	Participants    []AssemblyParticipant
	MetadataVersion MetadataVersion
}

type sourceSnapshotState struct {
	mu             sync.RWMutex
	tx             *sql.Tx
	rebind         func(string) string
	lastModified   string
	sourceSequence int64
	postgres       bool
	closed         bool
}

// SourceSnapshot owns one short database read transaction. Copying the value
// is safe because all copies share one state and one idempotent Close.
type SourceSnapshot struct {
	state *sourceSnapshotState
}

// BeginSourceSnapshot opens the transaction used for one assembly batch and
// pins the journal clock immediately. PostgreSQL needs REPEATABLE READ;
// SQLite establishes its stable view with the first clock read in the normal
// read transaction.
func BeginSourceSnapshot(ctx context.Context, st *store.Store) (SourceSnapshot, error) {
	if st == nil {
		return SourceSnapshot{}, errors.New("begin embedding source snapshot: nil store")
	}
	opts := &sql.TxOptions{ReadOnly: true}
	lastModified := "CAST(m.last_modified AS TEXT)"
	if st.IsPostgreSQL() {
		opts.Isolation = sql.LevelRepeatableRead
		lastModified = "m.last_modified"
	}
	tx, err := st.DB().BeginTx(ctx, opts)
	if err != nil {
		return SourceSnapshot{}, fmt.Errorf("begin embedding source snapshot: %w", err)
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx,
		`SELECT sequence FROM embedding_change_clock WHERE singleton = 1`).Scan(&sequence); err != nil {
		_ = tx.Rollback()
		return SourceSnapshot{}, fmt.Errorf("pin embedding source sequence: %w", err)
	}
	return SourceSnapshot{state: &sourceSnapshotState{
		tx: tx, rebind: st.Rebind, lastModified: lastModified,
		sourceSequence: sequence, postgres: st.IsPostgreSQL(),
	}}, nil
}

// SourceSequence is the journal clock pinned by this transaction.
func (s SourceSnapshot) SourceSequence() int64 {
	if s.state == nil {
		return 0
	}
	return s.state.sourceSequence
}

// Close releases the source transaction. Assembly callers must call it before
// any embedding HTTP request.
func (s SourceSnapshot) Close() error {
	if s.state == nil {
		return nil
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if s.state.closed {
		return nil
	}
	s.state.closed = true
	if err := s.state.tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("close embedding source snapshot: %w", err)
	}
	return nil
}

// Message reads one live source row from the snapshot.
func (s SourceSnapshot) Message(ctx context.Context, id int64) (AssemblyMessage, bool, error) {
	if s.state == nil {
		return AssemblyMessage{}, false, ErrSourceSnapshotClosed
	}
	s.state.mu.RLock()
	defer s.state.mu.RUnlock()
	if s.state.closed {
		return AssemblyMessage{}, false, ErrSourceSnapshotClosed
	}
	query := s.state.rebind(s.state.messageSelectSQL(`m.id = ?`))
	row, err := scanAssemblyMessage(s.state.tx.QueryRowContext(ctx, query, id), s.state.sourceSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return AssemblyMessage{}, false, nil
	}
	if err != nil {
		return AssemblyMessage{}, false, fmt.Errorf("read embedding source message %d: %w", id, err)
	}
	return row, true, nil
}

// Messages returns the complete live message set selected by one persisted
// scope. Conversation ranges are bounded by canonical time and ordered by
// canonical time then message ID, including deterministic NULL ordering.
func (s SourceSnapshot) Messages(ctx context.Context, scope AffectedScope) ([]AssemblyMessage, error) {
	if s.state == nil {
		return nil, ErrSourceSnapshotClosed
	}
	s.state.mu.RLock()
	defer s.state.mu.RUnlock()
	if s.state.closed {
		return nil, ErrSourceSnapshotClosed
	}
	if scope.MessageID != 0 {
		query := s.state.rebind(s.state.messageSelectSQL(`m.id = ?`))
		row, err := scanAssemblyMessage(s.state.tx.QueryRowContext(ctx, query, scope.MessageID), s.state.sourceSequence)
		if errors.Is(err, sql.ErrNoRows) {
			return []AssemblyMessage{}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read embedding source scope message %d: %w", scope.MessageID, err)
		}
		return []AssemblyMessage{row}, nil
	}
	if scope.ConversationID == 0 {
		return []AssemblyMessage{}, nil
	}
	where := []string{`m.conversation_id = ?`}
	args := []any{scope.ConversationID}
	where, args, canonicalTime := s.state.scopeRange(scope, where, args)
	query := s.state.rebind(s.state.messageSelectSQL(strings.Join(where, " AND ")) +
		` ORDER BY ` + canonicalTime + `, m.id`)
	return s.state.scanMessages(ctx, query, args, scope.ConversationID, false)
}

// ChatMessages reads one stable, row-bounded chat scope and caps both source
// body columns in SQL before the driver materializes them in Go.
func (s SourceSnapshot) ChatMessages(ctx context.Context, scope AffectedScope) ([]AssemblyMessage, error) {
	if s.state == nil {
		return nil, ErrSourceSnapshotClosed
	}
	s.state.mu.RLock()
	defer s.state.mu.RUnlock()
	if s.state.closed {
		return nil, ErrSourceSnapshotClosed
	}
	if scope.ConversationID == 0 || scope.MessageIDStart <= 0 || scope.MessageIDEnd <= scope.MessageIDStart {
		return nil, errors.New("read embedding chat scope: stable message bounds are required")
	}
	where := []string{`m.conversation_id = ?`}
	args := []any{scope.ConversationID}
	where, args, canonicalTime := s.state.scopeRange(scope, where, args)
	query := s.state.rebind(s.state.chatMessageSelectSQL(strings.Join(where, " AND ")) +
		` ORDER BY ` + canonicalTime + `, m.id`)
	return s.state.scanMessages(ctx, query, args, scope.ConversationID, true)
}

func (s SourceSnapshot) latestChatMessageID(ctx context.Context, conversationID int64) (int64, bool, error) {
	if s.state == nil {
		return 0, false, ErrSourceSnapshotClosed
	}
	s.state.mu.RLock()
	defer s.state.mu.RUnlock()
	if s.state.closed {
		return 0, false, ErrSourceSnapshotClosed
	}
	var id sql.NullInt64
	err := s.state.tx.QueryRowContext(ctx, s.state.rebind(`
		SELECT MAX(m.id)
		  FROM messages m
		  JOIN message_bodies mb ON mb.message_id = m.id
		 WHERE m.conversation_id = ?
		   AND m.message_type = 'beeper'
		   AND m.deleted_at IS NULL
		   AND m.deleted_from_source_at IS NULL`), conversationID).Scan(&id)
	if err != nil {
		return 0, false, fmt.Errorf("read latest embedding chat message %d: %w", conversationID, err)
	}
	return id.Int64, id.Valid, nil
}

// MessageVersions reads only CAS identity fields for a complete scope. It
// avoids loading source bodies a second time before contextual assembly.
func (s SourceSnapshot) MessageVersions(ctx context.Context, scope AffectedScope) ([]SourceVersion, error) {
	if s.state == nil {
		return nil, ErrSourceSnapshotClosed
	}
	s.state.mu.RLock()
	defer s.state.mu.RUnlock()
	if s.state.closed {
		return nil, ErrSourceSnapshotClosed
	}
	where := []string{`m.deleted_at IS NULL`, `m.deleted_from_source_at IS NULL`}
	args := make([]any, 0, 4)
	if scope.MessageID != 0 {
		where = append(where, `m.id = ?`)
		args = append(args, scope.MessageID)
		if scope.Kind != "" {
			where = append(where, `m.message_type = ?`)
			args = append(args, scope.Kind)
		}
	} else {
		if scope.ConversationID == 0 {
			return []SourceVersion{}, nil
		}
		where = append(where, `m.conversation_id = ?`)
		args = append(args, scope.ConversationID)
		where, args, _ = s.state.scopeRange(scope, where, args)
	}
	query := s.state.rebind(fmt.Sprintf(`SELECT m.id, %s FROM messages m WHERE %s ORDER BY m.id`,
		s.state.lastModified, strings.Join(where, " AND ")))
	rows, err := s.state.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read embedding source versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	versions := make([]SourceVersion, 0)
	for rows.Next() {
		var version SourceVersion
		if err := rows.Scan(&version.MessageID, &version.LastModified); err != nil {
			return nil, fmt.Errorf("scan embedding source version: %w", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate embedding source versions: %w", err)
	}
	return versions, nil
}

func (s *sourceSnapshotState) scopeRange(
	scope AffectedScope, where []string, args []any,
) ([]string, []any, string) {
	if scope.Kind != "" {
		where = append(where, `m.message_type = ?`)
		args = append(args, scope.Kind)
	}
	if scope.MessageIDStart > 0 {
		where = append(where, `m.id >= ?`)
		args = append(args, scope.MessageIDStart)
	}
	if scope.MessageIDEnd > 0 {
		where = append(where, `m.id < ?`)
		args = append(args, scope.MessageIDEnd)
	}
	rawCanonicalTime := `COALESCE(m.sent_at, m.received_at, m.internal_date)`
	canonicalTime := rawCanonicalTime
	timeParameter := `?`
	if !s.postgres {
		// SQLite stores source timestamps as text. julianday normalizes explicit
		// offsets before UTC range comparisons and chronological ordering.
		canonicalTime = `julianday(` + rawCanonicalTime + `)`
		timeParameter = `julianday(?)`
	}
	if scope.Undated {
		where = append(where, rawCanonicalTime+` IS NULL`)
	} else if !scope.UTCStart.IsZero() {
		where = append(where, canonicalTime+` >= `+timeParameter)
		args = append(args, s.timestampParam(scope.UTCStart))
	}
	if !scope.Undated && !scope.UTCEnd.IsZero() {
		where = append(where, canonicalTime+` < `+timeParameter)
		args = append(args, s.timestampParam(scope.UTCEnd))
	}
	return where, args, canonicalTime
}

func (s *sourceSnapshotState) scanMessages(
	ctx context.Context, query string, args []any, conversationID int64, chat bool,
) ([]AssemblyMessage, error) {
	rows, err := s.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read embedding source conversation %d: %w", conversationID, err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]AssemblyMessage, 0)
	for rows.Next() {
		var row AssemblyMessage
		if chat {
			row, err = scanChatAssemblyMessage(rows, s.sourceSequence)
		} else {
			row, err = scanAssemblyMessage(rows, s.sourceSequence)
		}
		if err != nil {
			return nil, fmt.Errorf("scan embedding source conversation %d: %w", conversationID, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate embedding source conversation %d: %w", conversationID, err)
	}
	return out, nil
}

// Conversation returns the rendered metadata and canonical metadata digest
// used by chat documents and their later all-or-nothing coverage CAS.
func (s SourceSnapshot) Conversation(ctx context.Context, id int64) (AssemblyConversation, bool, error) {
	if s.state == nil {
		return AssemblyConversation{}, false, ErrSourceSnapshotClosed
	}
	s.state.mu.RLock()
	defer s.state.mu.RUnlock()
	if s.state.closed {
		return AssemblyConversation{}, false, ErrSourceSnapshotClosed
	}
	var conversation AssemblyConversation
	err := s.state.tx.QueryRowContext(ctx, s.state.rebind(
		`SELECT id, COALESCE(title, '') FROM conversations WHERE id = ?`), id).
		Scan(&conversation.ID, &conversation.Title)
	if errors.Is(err, sql.ErrNoRows) {
		return AssemblyConversation{}, false, nil
	}
	if err != nil {
		return AssemblyConversation{}, false, fmt.Errorf("read embedding conversation %d: %w", id, err)
	}
	participantRevision := store.ParticipantRevisionSQLite
	if s.state.postgres {
		participantRevision = store.ParticipantRevisionPostgres
	}
	query := s.state.rebind(fmt.Sprintf(`
		SELECT cp.participant_id, COALESCE(cp.role, ''),
		       COALESCE(NULLIF(TRIM(p.display_name), ''),
		                NULLIF(p.email_address, ''), NULLIF(p.phone_number, ''), ''),
		       %s
		FROM conversation_participants cp
		JOIN participants p ON p.id = cp.participant_id
		WHERE cp.conversation_id = ?
		ORDER BY cp.participant_id`, participantRevision))
	rows, err := s.state.tx.QueryContext(ctx, query, id)
	if err != nil {
		return AssemblyConversation{}, false, fmt.Errorf("read embedding conversation participants %d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var participant AssemblyParticipant
		if err := rows.Scan(&participant.ID, &participant.Role, &participant.DisplayName, &participant.Revision); err != nil {
			return AssemblyConversation{}, false, fmt.Errorf("scan embedding conversation participant %d: %w", id, err)
		}
		conversation.Participants = append(conversation.Participants, participant)
	}
	if err := rows.Err(); err != nil {
		return AssemblyConversation{}, false, fmt.Errorf("iterate embedding conversation participants %d: %w", id, err)
	}
	conversation.MetadataVersion = MetadataVersion{
		ConversationID: id,
		Digest:         conversationMetadataDigest(conversation),
	}
	return conversation, true, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *sourceSnapshotState) messageSelectSQL(predicate string) string {
	return fmt.Sprintf(`
		SELECT m.id, m.conversation_id, COALESCE(m.message_type, ''),
		       COALESCE(m.subject, ''), COALESCE(mb.body_text, ''),
		       COALESCE(mb.body_html, ''),
		       m.sent_at, m.received_at, m.internal_date,
		       COALESCE(NULLIF(TRIM(sender.display_name), ''),
		                NULLIF(sender.email_address, ''), NULLIF(sender.phone_number, ''), ''),
		       %s, COALESCE(c.title, '')
		FROM messages m
		LEFT JOIN message_bodies mb ON mb.message_id = m.id
		LEFT JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN participants sender ON sender.id = m.sender_id
		WHERE %s AND m.deleted_at IS NULL
		  AND m.deleted_from_source_at IS NULL`, s.lastModified, predicate)
}

func (s *sourceSnapshotState) chatMessageSelectSQL(predicate string) string {
	// body_text is truncated in SQL: plain-text truncation is prefix-stable,
	// so stored offsets still reconstruct identical excerpts against the full
	// text. body_html ships whole — HTML stripping is NOT prefix-stable, so a
	// raw-HTML cut could leak markup into embeddings and desynchronize stored
	// offsets from search-time canonicalization. The canonical-text cap for
	// the HTML path is applied after Preprocess in chatMembers.
	bodyText := fmt.Sprintf(`SUBSTR(COALESCE(mb.body_text, ''), 1, %d)`, chatMessageBodyMaxChars)
	// Plain TRIM strips only spaces; trim the full ASCII whitespace set so a
	// tab/newline-padded body_text still falls back to body_html. The Go-side
	// predicate in ContextualBodyText must match this set exactly.
	blankText := `NULLIF(TRIM(COALESCE(mb.body_text, ''), char(32,9,10,13)), '') IS NULL`
	if s.postgres {
		bodyText = fmt.Sprintf(`LEFT(COALESCE(mb.body_text, ''), %d)`, chatMessageBodyMaxChars)
		blankText = `NULLIF(BTRIM(COALESCE(mb.body_text, ''), E' \t\n\r'), '') IS NULL`
	}
	bodyHTML := `CASE WHEN ` + blankText + ` THEN COALESCE(mb.body_html, '') ELSE '' END`
	bodyTruncated := fmt.Sprintf(`CASE
		WHEN %s THEN 1 = 0
		ELSE LENGTH(COALESCE(mb.body_text, '')) > %d
	END`, blankText, chatMessageBodyMaxChars)
	return fmt.Sprintf(`
		SELECT m.id, m.conversation_id, COALESCE(m.message_type, ''),
		       COALESCE(m.subject, ''), %s, %s,
		       m.sent_at, m.received_at, m.internal_date,
		       m.sender_id,
		       COALESCE(NULLIF(TRIM(sender.display_name), ''),
		                NULLIF(sender.email_address, ''), NULLIF(sender.phone_number, ''), ''),
		       %s, COALESCE(c.title, ''), %s
		FROM messages m
		LEFT JOIN message_bodies mb ON mb.message_id = m.id
		LEFT JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN participants sender ON sender.id = m.sender_id
		WHERE %s AND m.deleted_at IS NULL
		  AND m.deleted_from_source_at IS NULL`, bodyText, bodyHTML, s.lastModified, bodyTruncated, predicate)
}

func scanAssemblyMessage(scanner rowScanner, sequence int64) (AssemblyMessage, error) {
	var row AssemblyMessage
	var bodyText, bodyHTML string
	var sentAt, receivedAt, internalDate sql.NullTime
	err := scanner.Scan(
		&row.ID, &row.ConversationID, &row.MessageType, &row.Subject,
		&bodyText, &bodyHTML, &sentAt, &receivedAt, &internalDate,
		&row.SenderDisplay, &row.LastModified, &row.ConversationTitle,
	)
	if err != nil {
		return AssemblyMessage{}, err
	}
	row.Body = BodyTextForEmbedding(bodyText, bodyHTML)
	for _, candidate := range []sql.NullTime{sentAt, receivedAt, internalDate} {
		if candidate.Valid {
			row.SentAt = candidate.Time.UTC()
			break
		}
	}
	row.SourceSequence = sequence
	return row, nil
}

func scanChatAssemblyMessage(scanner rowScanner, sequence int64) (AssemblyMessage, error) {
	var row AssemblyMessage
	var bodyText, bodyHTML string
	var sentAt, receivedAt, internalDate sql.NullTime
	var senderID sql.NullInt64
	err := scanner.Scan(
		&row.ID, &row.ConversationID, &row.MessageType, &row.Subject,
		&bodyText, &bodyHTML, &sentAt, &receivedAt, &internalDate,
		&senderID, &row.SenderDisplay, &row.LastModified, &row.ConversationTitle, &row.BodyTruncated,
	)
	if err != nil {
		return AssemblyMessage{}, err
	}
	// chatMessageSelectSQL ships body_html only when body_text is blank after
	// trimming ASCII whitespace; ContextualBodyText applies the identical
	// predicate so a whitespace-only body_text row uses the HTML it was
	// shipped instead of silently owning no document.
	row.Body = ContextualBodyText(bodyText, bodyHTML)
	if senderID.Valid {
		row.SenderID = senderID.Int64
	}
	for _, candidate := range []sql.NullTime{sentAt, receivedAt, internalDate} {
		if candidate.Valid {
			row.SentAt = candidate.Time.UTC()
			break
		}
	}
	row.SourceSequence = sequence
	return row, nil
}

func (s *sourceSnapshotState) timestampParam(value time.Time) any {
	value = value.UTC()
	if s.postgres {
		return value
	}
	return value.Format("2006-01-02 15:04:05.999999999")
}

func conversationMetadataDigest(conversation AssemblyConversation) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%d\x00%s\x00", conversation.ID, conversation.Title)
	for _, participant := range conversation.Participants {
		_, _ = fmt.Fprintf(h, "%d\x00%s\x00%s\x00%v\x00", participant.ID,
			participant.Role, participant.DisplayName, participant.Revision)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Assembler resolves complete desired documents for affected source scopes.
type Assembler interface {
	AssembleScopes(ctx context.Context, snapshot SourceSnapshot, scopes []AffectedScope) ([]Document, error)
}

// CompositeAssembler routes only Beeper scopes to the injected chat
// specialization. Meeting documents use the strict meeting renderer. Every
// other live row, including Beeper before Task 6 injects Chat, uses the
// ordinary singleton path.
type CompositeAssembler struct {
	Policy AssemblyPolicy
	Chat   Assembler
}

// AssembleScopes deduplicates selectors independently of stale type labels,
// resolves the current type in the supplied snapshot, and returns documents
// in key order.
func (a CompositeAssembler) AssembleScopes(ctx context.Context, snapshot SourceSnapshot, scopes []AffectedScope) ([]Document, error) {
	unique := deduplicateScopes(scopes)
	chatScopes := make([]AffectedScope, 0)
	docs := make([]Document, 0, len(unique))
	for _, scope := range unique {
		var row AssemblyMessage
		var found bool
		var err error
		if scope.MessageID != 0 {
			row, found, err = snapshot.Message(ctx, scope.MessageID)
			if err != nil {
				return nil, err
			}
		}
		kind := scope.Kind
		if found {
			kind = row.MessageType
			scope.Kind = kind
			if scope.ConversationID == 0 {
				scope.ConversationID = row.ConversationID
			}
		}
		switch kind {
		case contextualChatMessageType:
			if a.Chat != nil {
				// A stale ordinary or meeting selector retains MessageID so its
				// keyed scope can be tombstoned. If the live row is now Beeper,
				// route a separate stable block selector to the chat assembler.
				// Passing the keyed selector through would discard its old scope
				// identity and make ChatMessages reject the missing bounds.
				if scope.MessageID != 0 && !found {
					continue
				}
				if scope.MessageID != 0 {
					scope = ChatMessageScope(row.ConversationID, row.SentAt, row.ID)
				}
				chatScopes = append(chatScopes, scope)
				continue
			}
			if !found {
				continue
			}
		case "meeting_transcript":
			if !found || a.shouldSkip(row) {
				continue
			}
			doc, err := AssembleMeetingDocument(row, a.Policy)
			if err != nil {
				return nil, err
			}
			if len(doc.Chunks) != 0 {
				docs = append(docs, doc)
			}
			continue
		default:
			if !found {
				continue
			}
		}
		if a.shouldSkip(row) {
			continue
		}
		if doc, ok := assembleOrdinaryDocument(row, a.Policy); ok {
			docs = append(docs, doc)
		}
	}
	if len(chatScopes) != 0 {
		chatDocs, err := a.Chat.AssembleScopes(ctx, snapshot, chatScopes)
		if err != nil {
			return nil, fmt.Errorf("assemble beeper scopes: %w", err)
		}
		docs = append(docs, chatDocs...)
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Key < docs[j].Key })
	return docs, nil
}

func (a CompositeAssembler) shouldSkip(row AssemblyMessage) bool {
	return a.Policy.SkipMessage != nil && a.Policy.SkipMessage(row)
}

func deduplicateScopes(scopes []AffectedScope) []AffectedScope {
	ordered := slices.Clone(scopes)
	sort.Slice(ordered, func(i, j int) bool {
		left, right := scopeSelectorKey(ordered[i]), scopeSelectorKey(ordered[j])
		if left == right {
			return ordered[i].Kind < ordered[j].Kind
		}
		return left < right
	})
	out := make([]AffectedScope, 0, len(ordered))
	last := ""
	for _, scope := range ordered {
		key := scopeSelectorKey(scope)
		if len(out) != 0 && key == last {
			continue
		}
		out = append(out, scope)
		last = key
	}
	return out
}

func scopeSelectorKey(scope AffectedScope) string {
	if scope.MessageID != 0 {
		return "message:" + strconv.FormatInt(scope.MessageID, 10)
	}
	return "conversation:" + strconv.FormatInt(scope.ConversationID, 10) + ":" +
		scope.Kind + ":" +
		strconv.FormatBool(scope.Undated) + ":" +
		scope.UTCStart.UTC().Format(time.RFC3339Nano) + ":" +
		scope.UTCEnd.UTC().Format(time.RFC3339Nano) + ":" +
		strconv.FormatInt(scope.MessageIDStart, 10) + ":" +
		strconv.FormatInt(scope.MessageIDEnd, 10)
}

func assembleOrdinaryDocument(row AssemblyMessage, policy AssemblyPolicy) (Document, bool) {
	text, bodyTruncated := Preprocess(row.Subject, row.Body, 0, policy.Preprocess)
	if strings.TrimSpace(text) == "" {
		return Document{}, false
	}
	spans, tailDropped := ChunkText(text, policy.MaxChunkRunes,
		chunkOverlapFor(policy.MaxChunkRunes), maxSpansPerMessage)
	chunks := make([]OwnedChunk, 0, len(spans))
	for i, span := range spans {
		chunks = append(chunks, OwnedChunk{
			MessageID: row.ID, ChunkIndex: i, Text: span.Text,
			SourceCharStart: span.CharStart, SourceCharEnd: span.CharEnd,
			SourceBasis: vector.SourceBasisSubjectBody,
			Truncated:   bodyTruncated || tailDropped,
		})
	}
	chunks = limitOwnedChunksToRequest(chunks, policy.MaxDocumentUTF8Bytes, defaultVoyageRequestLimits.MaxChunks)
	if len(chunks) == 0 {
		return Document{}, false
	}
	key := "message:" + strconv.FormatInt(row.ID, 10)
	return Document{
		Key: key, Kind: "ordinary-message", ScopeKey: key,
		Revision:       documentRevision("ordinary", row, policy, chunks),
		SourceSequence: row.SourceSequence,
		Versions:       []SourceVersion{{MessageID: row.ID, LastModified: row.LastModified}},
		Chunks:         chunks,
	}, true
}

// limitOwnedChunksToRequest makes the assembler's output obey the exact byte
// accounting used by PackDocuments. It preserves a prefix of the source,
// marks the final retained chunk truncated, and never splits one document
// across provider requests.
func limitOwnedChunksToRequest(chunks []OwnedChunk, maxBytes, maxChunks int) []OwnedChunk {
	if len(chunks) == 0 || (maxBytes <= 0 && maxChunks <= 0) {
		return chunks
	}
	limit := len(chunks)
	if maxChunks > 0 {
		limit = min(limit, maxChunks)
	}
	remaining := maxBytes
	out := make([]OwnedChunk, 0, limit)
	dropped := limit < len(chunks)
	for i := range limit {
		chunk := chunks[i]
		if maxBytes <= 0 {
			out = append(out, chunk)
			continue
		}
		textBudget := remaining - voyagePromptReserveUTF8BytesPerChunk
		if textBudget <= 0 {
			dropped = true
			break
		}
		if len(chunk.Text) > textBudget {
			var ok bool
			chunk, ok = trimOwnedChunkText(chunk, textBudget)
			if !ok {
				dropped = true
				break
			}
			chunk.Truncated = true
			out = append(out, chunk)
			dropped = true
			break
		}
		out = append(out, chunk)
		remaining -= len(chunk.Text) + voyagePromptReserveUTF8BytesPerChunk
	}
	if dropped && len(out) > 0 {
		out[len(out)-1].Truncated = true
	}
	return out
}

func trimOwnedChunkText(chunk OwnedChunk, maxBytes int) (OwnedChunk, bool) {
	if maxBytes <= 0 {
		return OwnedChunk{}, false
	}
	runes := []rune(chunk.Text)
	sourceRunes := max(0, chunk.SourceCharEnd-chunk.SourceCharStart)
	prefixRunes := max(0, len(runes)-sourceRunes)
	prefix := string(runes[:prefixRunes])
	source := string(runes[prefixRunes:])

	// Context is useful, but source text must remain present. Bound the prefix
	// first and give all remaining bytes to the source suffix.
	prefixBudget := min(len(prefix), maxBytes/4)
	prefix = utf8Prefix(prefix, prefixBudget)
	source = utf8Prefix(source, maxBytes-len(prefix))
	if sourceRunes > 0 && source == "" {
		prefix = ""
		source = utf8Prefix(string(runes[prefixRunes:]), maxBytes)
	}
	if prefix == "" && source == "" {
		return OwnedChunk{}, false
	}
	chunk.Text = prefix + source
	chunk.SourceCharEnd = chunk.SourceCharStart + utf8.RuneCountInString(source)
	return chunk, true
}

func utf8Prefix(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func documentRevision(kind string, row AssemblyMessage, policy AssemblyPolicy, chunks []OwnedChunk) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%d\x00%d\x00%s\x00%d\x00%d\x00", kind, row.ID,
		row.ConversationID, row.MessageType, policy.MaxChunkRunes, policy.MaxDocumentUTF8Bytes)
	for _, chunk := range chunks {
		_, _ = fmt.Fprintf(h, "%d\x00%d\x00%d\x00%d\x00%d\x00%t\x00%s\x00",
			chunk.MessageID, chunk.ChunkIndex, chunk.SourceCharStart, chunk.SourceCharEnd,
			chunk.SourceBasis, chunk.Truncated, chunk.Text)
	}
	return hex.EncodeToString(h.Sum(nil))
}
