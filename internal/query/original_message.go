package query

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

var (
	// ErrOriginalMIMEUnavailable means the message exists but the archive
	// holds no original MIME for it (chat or calendar sources, imports
	// without raw data).
	ErrOriginalMIMEUnavailable = errors.New("original MIME unavailable")
	// ErrInvalidMessageRef means a reference did not name exactly one of an
	// internal message ID or a provider message ID.
	ErrInvalidMessageRef = errors.New("provide exactly one of id or source_message_id")
	// ErrAmbiguousReference matches every *AmbiguousError, including ones
	// reconstructed from a daemon response.
	ErrAmbiguousReference = errors.New("reference matches several accounts")
	// ErrOriginalExportUnsupported means this engine or daemon cannot serve
	// original messages or thread listings.
	ErrOriginalExportUnsupported = errors.New("original message export is not supported by this archive backend")
)

// AmbiguousError reports that a provider identifier matched archive rows in
// more than one account. Callers retry with MessageRef.Account.
type AmbiguousError struct {
	Kind     string
	Accounts []string
}

func (e *AmbiguousError) Unwrap() error { return ErrAmbiguousReference }

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%s matches several accounts: %s", e.Kind, strings.Join(e.Accounts, ", "))
}

// MessageRef names one archived message. Exactly one of ID (internal
// msgvault ID) or SourceMessageID (provider message ID) is set. Account,
// when set, restricts the lookup to the source with that identifier.
type MessageRef struct {
	ID              int64
	SourceMessageID string
	Account         string
}

// MessageRecord is the provenance of one message or conversation.
type MessageRecord struct {
	MessageID            int64      `json:"message_id,omitzero"`
	SourceMessageID      string     `json:"source_message_id,omitzero"`
	ConversationID       int64      `json:"conversation_id"`
	SourceConversationID string     `json:"source_conversation_id"`
	SourceID             int64      `json:"source_id"`
	Account              string     `json:"account"`
	SourceType           string     `json:"source_type"`
	LastSyncAt           *time.Time `json:"last_sync_at,omitzero"`
}

// OriginalMessage is a message's provenance plus its original MIME bytes
// exactly as the provider delivered them.
type OriginalMessage struct {
	MessageRecord

	MIME []byte
}

// OriginalMessageReader reads original MIME and complete conversation
// listings for export. Engines that cannot serve these omit the interface.
type OriginalMessageReader interface {
	ReadOriginalMessage(ctx context.Context, ref MessageRef) (*OriginalMessage, error)
	ListThread(ctx context.Context, q ThreadQuery) (*ThreadPage, error)
}

var _ OriginalMessageReader = (*SQLiteEngine)(nil)

// ReadOriginalMessage resolves ref among live messages (dedup losers are
// not found; source-deleted messages remain archive data) and returns the
// stored MIME. Non-MIME raw formats report ErrOriginalMIMEUnavailable.
func (e *SQLiteEngine) ReadOriginalMessage(ctx context.Context, ref MessageRef) (*OriginalMessage, error) {
	record, err := e.resolveMessageRecord(ctx, ref)
	if err != nil {
		return nil, err
	}

	var compressed []byte
	var format, compression sql.NullString
	err = e.queryRowContext(ctx, `
		SELECT raw_data, raw_format, compression FROM message_raw WHERE message_id = ?
	`, record.MessageID).Scan(&compressed, &format, &compression)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && format.String != "mime") {
		return nil, fmt.Errorf("message %d: %w", record.MessageID, ErrOriginalMIMEUnavailable)
	}
	if err != nil {
		return nil, fmt.Errorf("read original MIME for message %d: %w", record.MessageID, err)
	}
	mime, err := inflateMessageRaw(compressed, compression)
	if err != nil {
		return nil, fmt.Errorf("decode original MIME for message %d: %w", record.MessageID, err)
	}
	return &OriginalMessage{MessageRecord: *record, MIME: mime}, nil
}

func (e *SQLiteEngine) resolveMessageRecord(ctx context.Context, ref MessageRef) (*MessageRecord, error) {
	if (ref.ID == 0) == (ref.SourceMessageID == "") || ref.ID < 0 {
		return nil, ErrInvalidMessageRef
	}
	conditions := []string{store.LiveMessagesWhere("m", false)}
	var args []any
	if ref.ID != 0 {
		conditions = append(conditions, "m.id = ?")
		args = append(args, ref.ID)
	} else {
		conditions = append(conditions, "m.source_message_id = ?")
		args = append(args, ref.SourceMessageID)
	}
	if ref.Account != "" {
		conditions = append(conditions, "s.identifier = ?")
		args = append(args, ref.Account)
	}
	rows, err := e.queryContext(ctx, `
		SELECT m.id, COALESCE(m.source_message_id, ''), COALESCE(m.conversation_id, 0),
		       COALESCE(conv.source_conversation_id, ''),
		       s.id, s.identifier, s.source_type, s.last_sync_at
		FROM messages m
		JOIN sources s ON s.id = m.source_id
		LEFT JOIN conversations conv ON conv.id = m.conversation_id
		WHERE `+strings.Join(conditions, " AND ")+`
		ORDER BY m.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("resolve message: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var records []MessageRecord
	for rows.Next() {
		var record MessageRecord
		var lastSyncAt sql.NullTime
		if err := rows.Scan(&record.MessageID, &record.SourceMessageID, &record.ConversationID,
			&record.SourceConversationID, &record.SourceID, &record.Account, &record.SourceType,
			&lastSyncAt); err != nil {
			return nil, fmt.Errorf("scan message reference: %w", err)
		}
		record.LastSyncAt = utcTimePtr(lastSyncAt)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve message: %w", err)
	}
	switch len(records) {
	case 0:
		return nil, fmt.Errorf("message %s: %w", ref.describe(), store.ErrMessageNotFound)
	case 1:
		return &records[0], nil
	}
	accounts := make([]string, 0, len(records))
	for _, record := range records {
		accounts = append(accounts, record.Account)
	}
	return nil, &AmbiguousError{Kind: "source_message_id " + ref.SourceMessageID, Accounts: sortedUnique(accounts)}
}

func (ref MessageRef) describe() string {
	if ref.ID != 0 {
		return strconv.FormatInt(ref.ID, 10)
	}
	return ref.SourceMessageID
}

func utcTimePtr(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	t := value.Time.UTC()
	return &t
}

func sortedUnique(values []string) []string {
	slices.Sort(values)
	return slices.Compact(values)
}

// inflateMessageRaw decodes a message_raw payload by its compression tag.
func inflateMessageRaw(stored []byte, compression sql.NullString) ([]byte, error) {
	if !compression.Valid || compression.String != "zlib" {
		return stored, nil
	}
	r, err := zlib.NewReader(bytes.NewReader(stored))
	if err != nil {
		return nil, fmt.Errorf("zlib reader: %w", err)
	}
	defer func() { _ = r.Close() }()
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("zlib decompress: %w", err)
	}
	return raw, nil
}

// ErrThreadNotFound means no archived conversation matched a thread lookup.
var ErrThreadNotFound = errors.New("thread not found")

// Thread listing page bounds.
const (
	ThreadDefaultLimit = 100
	ThreadMaxLimit     = 500
)

// ThreadQuery selects one conversation by exactly one of an anchor message
// (MessageRef.ID or MessageRef.SourceMessageID) or a provider conversation
// ID (ThreadID). MessageRef.Account narrows either lookup to one source.
type ThreadQuery struct {
	MessageRef

	ThreadID string
	Limit    int
	Offset   int
}

// ThreadMessage is one archived message in a conversation listing.
type ThreadMessage struct {
	ID                  int64      `json:"id"`
	SourceMessageID     string     `json:"source_message_id"`
	Subject             string     `json:"subject"`
	SentAt              *time.Time `json:"sent_at,omitzero"`
	From                []Address  `json:"from"`
	To                  []Address  `json:"to"`
	Cc                  []Address  `json:"cc"`
	HasRaw              bool       `json:"has_raw"`
	AttachmentCount     int        `json:"attachment_count"`
	DeletedFromSourceAt *time.Time `json:"deleted_from_source_at,omitzero"`
}

// ThreadPage is one page of a conversation in chronological order. The
// embedded record describes the conversation; MessageID and
// SourceMessageID name the anchor message when the lookup used one.
type ThreadPage struct {
	MessageRecord

	Total    int64           `json:"total"`
	Offset   int             `json:"offset"`
	HasMore  bool            `json:"has_more"`
	Messages []ThreadMessage `json:"messages"`
}

// ListThread lists the live messages of one conversation ordered by
// sent_at (undated last), then ID. Source-deleted messages are included
// because they remain archive data. HasRaw reports stored original MIME.
func (e *SQLiteEngine) ListThread(ctx context.Context, q ThreadQuery) (*ThreadPage, error) {
	anchors := 0
	for _, set := range []bool{q.ID != 0, q.SourceMessageID != "", q.ThreadID != ""} {
		if set {
			anchors++
		}
	}
	if anchors != 1 {
		return nil, ErrInvalidMessageRef
	}
	limit := q.Limit
	if limit <= 0 {
		limit = ThreadDefaultLimit
	}
	limit = min(limit, ThreadMaxLimit)
	offset := max(q.Offset, 0)

	var header *MessageRecord
	var err error
	if q.ThreadID != "" {
		header, err = e.resolveThreadRecord(ctx, q.ThreadID, q.Account)
	} else {
		header, err = e.resolveMessageRecord(ctx, q.MessageRef)
	}
	if err != nil {
		return nil, err
	}

	live := store.LiveMessagesWhere("m", false)
	page := &ThreadPage{MessageRecord: *header, Offset: offset, Messages: []ThreadMessage{}}
	if err := e.queryRowContext(ctx, `
		SELECT COUNT(*) FROM messages m WHERE m.conversation_id = ? AND `+live,
		header.ConversationID).Scan(&page.Total); err != nil {
		return nil, fmt.Errorf("count thread messages: %w", err)
	}

	rows, err := e.queryContext(ctx, `
		SELECT m.id, COALESCE(m.source_message_id, ''), COALESCE(m.subject, ''), m.sent_at,
		       COALESCE(m.attachment_count, 0), m.deleted_from_source_at,
		       CASE WHEN EXISTS (
		           SELECT 1 FROM message_raw mr
		           WHERE mr.message_id = m.id AND mr.raw_format = 'mime'
		       ) THEN 1 ELSE 0 END
		FROM messages m
		WHERE m.conversation_id = ? AND `+live+`
		ORDER BY CASE WHEN m.sent_at IS NULL THEN 1 ELSE 0 END, m.sent_at, m.id
		LIMIT ? OFFSET ?`, header.ConversationID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list thread messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	index := make(map[int64]int)
	for rows.Next() {
		var msg ThreadMessage
		var sentAt, deletedAt sql.NullTime
		var hasRaw int
		if err := rows.Scan(&msg.ID, &msg.SourceMessageID, &msg.Subject, &sentAt,
			&msg.AttachmentCount, &deletedAt, &hasRaw); err != nil {
			return nil, fmt.Errorf("scan thread message: %w", err)
		}
		msg.SentAt = utcTimePtr(sentAt)
		msg.DeletedFromSourceAt = utcTimePtr(deletedAt)
		msg.HasRaw = hasRaw == 1
		msg.From, msg.To, msg.Cc = []Address{}, []Address{}, []Address{}
		index[msg.ID] = len(page.Messages)
		page.Messages = append(page.Messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list thread messages: %w", err)
	}
	page.HasMore = int64(offset+len(page.Messages)) < page.Total
	if err := e.fillThreadParticipants(ctx, page.Messages, index); err != nil {
		return nil, err
	}
	return page, nil
}

func (e *SQLiteEngine) resolveThreadRecord(ctx context.Context, threadID, account string) (*MessageRecord, error) {
	conditions := "conv.source_conversation_id = ?"
	args := []any{threadID}
	if account != "" {
		conditions += " AND s.identifier = ?"
		args = append(args, account)
	}
	rows, err := e.queryContext(ctx, `
		SELECT conv.id, conv.source_conversation_id, s.id, s.identifier, s.source_type, s.last_sync_at
		FROM conversations conv
		JOIN sources s ON s.id = conv.source_id
		WHERE `+conditions+`
		ORDER BY conv.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("resolve thread: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var records []MessageRecord
	for rows.Next() {
		var record MessageRecord
		var lastSyncAt sql.NullTime
		if err := rows.Scan(&record.ConversationID, &record.SourceConversationID, &record.SourceID,
			&record.Account, &record.SourceType, &lastSyncAt); err != nil {
			return nil, fmt.Errorf("scan thread reference: %w", err)
		}
		record.LastSyncAt = utcTimePtr(lastSyncAt)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve thread: %w", err)
	}
	switch len(records) {
	case 0:
		return nil, fmt.Errorf("thread %s: %w", threadID, ErrThreadNotFound)
	case 1:
		return &records[0], nil
	}
	accounts := make([]string, 0, len(records))
	for _, record := range records {
		accounts = append(accounts, record.Account)
	}
	return nil, &AmbiguousError{Kind: "thread_id " + threadID, Accounts: sortedUnique(accounts)}
}

func (e *SQLiteEngine) fillThreadParticipants(ctx context.Context, messages []ThreadMessage, index map[int64]int) error {
	if len(messages) == 0 {
		return nil
	}
	ids := make([]any, len(messages))
	placeholders := make([]string, len(messages))
	for i, msg := range messages {
		ids[i] = msg.ID
		placeholders[i] = "?"
	}
	rows, err := e.queryContext(ctx, fmt.Sprintf(`
		SELECT mr.message_id, mr.recipient_type,
		       COALESCE(NULLIF(p.email_address, ''), NULLIF(p.phone_number, ''), ''),
		       %s
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id IN (%s)
		  AND mr.recipient_type IN ('from', 'to', 'cc')
		ORDER BY mr.message_id, mr.id
	`, recipientNameExpr("mr", "p"), strings.Join(placeholders, ",")), ids...)
	if err != nil {
		return fmt.Errorf("fetch thread participants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var messageID int64
		var recipientType string
		var addr Address
		if err := rows.Scan(&messageID, &recipientType, &addr.Email, &addr.Name); err != nil {
			return fmt.Errorf("scan thread participant: %w", err)
		}
		msg := &messages[index[messageID]]
		switch recipientType {
		case "from":
			msg.From = append(msg.From, addr)
		case "to":
			msg.To = append(msg.To, addr)
		case "cc":
			msg.Cc = append(msg.Cc, addr)
		}
	}
	return rows.Err()
}
