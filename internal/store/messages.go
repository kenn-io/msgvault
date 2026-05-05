package store

import (
	"bytes"
	"compress/zlib"
	"database/sql"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"time"

	"github.com/wesm/msgvault/internal/mime"
)

// querier is satisfied by both *sql.DB and *sql.Tx, allowing
// helpers to run inside or outside a transaction.
type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// RecipientSet groups participant IDs and display names for one
// recipient type (from, to, cc, bcc).
type RecipientSet struct {
	Type           string
	ParticipantIDs []int64
	DisplayNames   []string
}

// MessagePersistData bundles everything needed to atomically
// persist a message and its related rows in a single transaction.
type MessagePersistData struct {
	Message    *Message
	BodyText   sql.NullString
	BodyHTML   sql.NullString
	RawMIME    []byte
	Recipients []RecipientSet
	LabelIDs   []int64
}

// Message represents a message in the database.
type Message struct {
	ID              int64
	ConversationID  int64
	SourceID        int64
	SourceMessageID string
	RFC822MessageID sql.NullString // RFC822 Message-ID header for cross-mailbox dedup
	MessageType     string         // "email"
	SentAt          sql.NullTime
	ReceivedAt      sql.NullTime
	InternalDate    sql.NullTime
	SenderID        sql.NullInt64
	IsFromMe        bool
	Subject         sql.NullString
	Snippet         sql.NullString
	SizeEstimate    int64
	HasAttachments  bool
	AttachmentCount int
	DeletedAt       sql.NullTime
	ArchivedAt      time.Time
}

// MessageExistsBatch checks which message IDs already exist in the database.
// Returns a map of source_message_id -> internal message_id for existing messages.
func (s *Store) MessageExistsBatch(sourceID int64, sourceMessageIDs []string) (map[string]int64, error) {
	if len(sourceMessageIDs) == 0 {
		return make(map[string]int64), nil
	}

	result := make(map[string]int64)
	err := queryInChunks(s.db, sourceMessageIDs, []interface{}{sourceID},
		`SELECT source_message_id, id FROM messages WHERE source_id = ? AND source_message_id IN (%s)`,
		func(rows *loggedRows) error {
			var srcID string
			var id int64
			if err := rows.Scan(&srcID, &id); err != nil {
				return err
			}
			result[srcID] = id
			return nil
		})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// GetMessageIDByRFC822ID returns the internal ID of a message
// with the given RFC822 Message-ID for this source, or 0 if
// no match exists.
func (s *Store) GetMessageIDByRFC822ID(
	sourceID int64, rfc822ID string,
) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		`SELECT id FROM messages
		 WHERE source_id = ? AND rfc822_message_id = ?`,
		sourceID, rfc822ID,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// UpdateMessageOnDedup updates an existing message's composite ID
// and labels when a cross-mailbox RFC822 dedup match is found.
// This ensures future syncs recognize the message under its new
// mailbox|uid key and don't re-download it.
func (s *Store) UpdateMessageOnDedup(
	messageID int64, newSourceMessageID string,
	labelIDs []int64,
) error {
	return s.withTx(func(tx *loggedTx) error {
		if _, err := tx.Exec(
			`UPDATE messages SET source_message_id = ?
			 WHERE id = ?`,
			newSourceMessageID, messageID,
		); err != nil {
			return fmt.Errorf("update source_message_id: %w", err)
		}
		return replaceMessageLabelsTx(tx, messageID, labelIDs)
	})
}

// MessageExistsWithRawBatch checks which message IDs already exist in the database
// and have raw MIME data stored.
// Returns a map of source_message_id -> internal message_id.
func (s *Store) MessageExistsWithRawBatch(sourceID int64, sourceMessageIDs []string) (map[string]int64, error) {
	if len(sourceMessageIDs) == 0 {
		return make(map[string]int64), nil
	}

	result := make(map[string]int64)
	err := queryInChunks(s.db, sourceMessageIDs, []interface{}{sourceID},
		`SELECT m.source_message_id, m.id
		 FROM messages m
		 JOIN message_raw mr ON mr.message_id = m.id
		 WHERE m.source_id = ? AND m.source_message_id IN (%s)`,
		func(rows *loggedRows) error {
			var srcID string
			var id int64
			if err := rows.Scan(&srcID, &id); err != nil {
				return err
			}
			result[srcID] = id
			return nil
		})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// EnsureConversation gets or creates a conversation (thread) for a message.
func (s *Store) EnsureConversation(sourceID int64, sourceConversationID, title string) (int64, error) {
	// Try to get existing
	var id int64
	err := s.db.QueryRow(`
		SELECT id FROM conversations
		WHERE source_id = ? AND source_conversation_id = ?
	`, sourceID, sourceConversationID).Scan(&id)

	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}

	// Create new
	result, err := s.db.Exec(fmt.Sprintf(`
		INSERT INTO conversations (source_id, source_conversation_id, conversation_type, title, created_at, updated_at)
		VALUES (?, ?, 'email_thread', ?, %s, %s)
	`, s.dialect.Now(), s.dialect.Now()), sourceID, sourceConversationID, title)
	if err != nil {
		return 0, err
	}

	return result.LastInsertId()
}

// upsertMessageSQL returns the message upsert SQL with dialect-specific timestamp.
func upsertMessageSQL(now string) string {
	return fmt.Sprintf(`
	INSERT INTO messages (
		conversation_id, source_id, source_message_id,
		rfc822_message_id, message_type,
		sent_at, received_at, internal_date, sender_id, is_from_me,
		subject, snippet, size_estimate,
		has_attachments, attachment_count, archived_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, %s)
	ON CONFLICT(source_id, source_message_id) DO UPDATE SET
		conversation_id = excluded.conversation_id,
		rfc822_message_id = excluded.rfc822_message_id,
		sent_at = excluded.sent_at,
		received_at = excluded.received_at,
		internal_date = excluded.internal_date,
		sender_id = excluded.sender_id,
		is_from_me = excluded.is_from_me,
		subject = excluded.subject,
		snippet = excluded.snippet,
		size_estimate = excluded.size_estimate,
		has_attachments = excluded.has_attachments,
		attachment_count = excluded.attachment_count`, now)
}

// UpsertMessage inserts or updates a message.
func (s *Store) UpsertMessage(msg *Message) (int64, error) {
	return upsertMessageWith(s.db, s.dialect, msg)
}

func upsertMessageWith(q querier, d Dialect, msg *Message) (int64, error) {
	sql := upsertMessageSQL(d.Now())
	args := []any{
		msg.ConversationID, msg.SourceID, msg.SourceMessageID,
		msg.RFC822MessageID, msg.MessageType,
		msg.SentAt, msg.ReceivedAt, msg.InternalDate, msg.SenderID, msg.IsFromMe,
		msg.Subject, msg.Snippet, msg.SizeEstimate,
		msg.HasAttachments, msg.AttachmentCount,
	}

	// Use RETURNING to avoid an extra SELECT per message when supported.
	var id int64
	err := q.QueryRow(sql+"\n\t\tRETURNING id\n\t", args...).Scan(&id)

	if err != nil {
		// SQLite < 3.35 does not support RETURNING. Fall back to an Exec + SELECT.
		if !d.IsReturningError(err) {
			return 0, err
		}

		if _, execErr := q.Exec(sql, args...); execErr != nil {
			return 0, execErr
		}

		if err := q.QueryRow(
			`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`,
			msg.SourceID, msg.SourceMessageID,
		).Scan(&id); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// UpsertMessageBody stores the body text and HTML for a message in the separate message_bodies table.
func (s *Store) UpsertMessageBody(messageID int64, bodyText, bodyHTML sql.NullString) error {
	return upsertMessageBody(s.db, messageID, bodyText, bodyHTML)
}

func upsertMessageBody(q querier, messageID int64, bodyText, bodyHTML sql.NullString) error {
	_, err := q.Exec(`
		INSERT INTO message_bodies (message_id, body_text, body_html)
		VALUES (?, ?, ?)
		ON CONFLICT(message_id) DO UPDATE SET
			body_text = excluded.body_text,
			body_html = excluded.body_html
	`, messageID, bodyText, bodyHTML)
	return err
}

// UpsertMessageRaw stores the compressed raw MIME data for a message.
func (s *Store) UpsertMessageRaw(messageID int64, rawData []byte) error {
	return upsertMessageRaw(s.db, messageID, rawData)
}

func upsertMessageRaw(q querier, messageID int64, rawData []byte) error {
	// Compress with zlib
	var compressed bytes.Buffer
	w := zlib.NewWriter(&compressed)
	if _, err := w.Write(rawData); err != nil {
		return fmt.Errorf("compress: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close compressor: %w", err)
	}

	_, err := q.Exec(`
		INSERT INTO message_raw (message_id, raw_data, raw_format, compression)
		VALUES (?, ?, 'mime', 'zlib')
		ON CONFLICT(message_id) DO UPDATE SET
			raw_data = excluded.raw_data,
			raw_format = excluded.raw_format,
			compression = excluded.compression
	`, messageID, compressed.Bytes())
	return err
}

// GetMessageRaw retrieves and decompresses the raw MIME data for a message.
func (s *Store) GetMessageRaw(messageID int64) ([]byte, error) {
	var compressed []byte
	var compression sql.NullString

	err := s.db.QueryRow(`
		SELECT raw_data, compression FROM message_raw WHERE message_id = ?
	`, messageID).Scan(&compressed, &compression)
	if err != nil {
		return nil, err
	}

	if compression.Valid && compression.String == "zlib" {
		r, err := zlib.NewReader(bytes.NewReader(compressed))
		if err != nil {
			return nil, fmt.Errorf("zlib reader: %w", err)
		}
		defer func() { _ = r.Close() }()
		return io.ReadAll(r)
	}

	return compressed, nil
}

// PersistMessage atomically stores a message plus its body, raw MIME,
// recipients, and labels in a single transaction. Returns the message ID.
func (s *Store) PersistMessage(data *MessagePersistData) (int64, error) {
	var messageID int64
	err := s.withTx(func(tx *loggedTx) error {
		id, err := upsertMessageWith(tx, s.dialect, data.Message)
		if err != nil {
			return fmt.Errorf("upsert message: %w", err)
		}
		messageID = id

		if err := upsertMessageBody(tx, messageID, data.BodyText, data.BodyHTML); err != nil {
			return fmt.Errorf("upsert body: %w", err)
		}

		if len(data.RawMIME) > 0 {
			if err := upsertMessageRaw(tx, messageID, data.RawMIME); err != nil {
				return fmt.Errorf("store raw: %w", err)
			}
		}

		for _, rs := range data.Recipients {
			if err := replaceMessageRecipientsTx(tx, messageID, rs); err != nil {
				return fmt.Errorf("store %s recipients: %w", rs.Type, err)
			}
		}

		if err := replaceMessageLabelsTx(tx, messageID, data.LabelIDs); err != nil {
			return fmt.Errorf("store labels: %w", err)
		}

		return nil
	})
	return messageID, err
}

// Participant represents a person in the participants table.
type Participant struct {
	ID           int64
	EmailAddress sql.NullString
	DisplayName  sql.NullString
	Domain       sql.NullString
}

// EnsureParticipant gets or creates a participant by email.
func (s *Store) EnsureParticipant(email, displayName, domain string) (int64, error) {
	// Try to get existing
	var id int64
	err := s.db.QueryRow(`
		SELECT id FROM participants WHERE email_address = ?
	`, email).Scan(&id)

	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}

	// Create new
	result, err := s.db.Exec(fmt.Sprintf(`
		INSERT INTO participants (email_address, display_name, domain, created_at, updated_at)
		VALUES (?, ?, ?, %s, %s)
	`, s.dialect.Now(), s.dialect.Now()), email, displayName, domain)
	if err != nil {
		return 0, err
	}

	return result.LastInsertId()
}

// EnsureParticipantsBatch gets or creates participants in batch.
// Returns a map of email -> participant ID.
func (s *Store) EnsureParticipantsBatch(addresses []mime.Address) (map[string]int64, error) {
	if len(addresses) == 0 {
		return make(map[string]int64), nil
	}

	result := make(map[string]int64)

	// First, try to insert all (ignoring conflicts)
	insertSQL := s.dialect.InsertOrIgnore(fmt.Sprintf(`INSERT OR IGNORE INTO participants (email_address, display_name, domain, created_at, updated_at)
			VALUES (?, ?, ?, %s, %s)`, s.dialect.Now(), s.dialect.Now()))
	for _, addr := range addresses {
		if addr.Email == "" {
			continue
		}
		if _, err := s.db.Exec(insertSQL, addr.Email, addr.Name, addr.Domain); err != nil {
			return nil, err
		}
	}

	// Then fetch all IDs
	emails := make([]string, 0, len(addresses))
	for _, addr := range addresses {
		if addr.Email != "" {
			emails = append(emails, addr.Email)
		}
	}

	if len(emails) == 0 {
		return result, nil
	}

	err := queryInChunks(s.db, emails, nil,
		`SELECT email_address, id FROM participants WHERE email_address IN (%s)`,
		func(rows *loggedRows) error {
			var email string
			var id int64
			if err := rows.Scan(&email, &id); err != nil {
				return err
			}
			result[email] = id
			return nil
		})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ReplaceMessageRecipients replaces all recipients for a message atomically.
func (s *Store) ReplaceMessageRecipients(messageID int64, recipientType string, participantIDs []int64, displayNames []string) error {
	return s.withTx(func(tx *loggedTx) error {
		return replaceMessageRecipientsTx(tx, messageID, RecipientSet{
			Type:           recipientType,
			ParticipantIDs: participantIDs,
			DisplayNames:   displayNames,
		})
	})
}

func replaceMessageRecipientsTx(tx *loggedTx, messageID int64, rs RecipientSet) error {
	_, err := tx.Exec(`
		DELETE FROM message_recipients WHERE message_id = ? AND recipient_type = ?
	`, messageID, rs.Type)
	if err != nil {
		return err
	}

	if len(rs.ParticipantIDs) == 0 {
		return nil
	}

	return insertInChunks(tx, chunkInsert{
		totalRows:    len(rs.ParticipantIDs),
		valuesPerRow: 4,
		prefix:       "INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name) VALUES ",
	}, func(start, end int) ([]string, []interface{}) {
		values := make([]string, end-start)
		args := make([]interface{}, 0, (end-start)*4)
		for i := start; i < end; i++ {
			values[i-start] = "(?, ?, ?, ?)"
			displayName := ""
			if i < len(rs.DisplayNames) {
				displayName = rs.DisplayNames[i]
			}
			args = append(args, messageID, rs.ParticipantIDs[i], rs.Type, displayName)
		}
		return values, args
	})
}

// Label represents a Gmail label.
type Label struct {
	ID            int64
	SourceID      sql.NullInt64
	SourceLabelID sql.NullString
	Name          string
	LabelType     sql.NullString
}

// dbQuerier abstracts *sql.DB and *sql.Tx for functions that need to
// run both standalone and inside a transaction.
type dbQuerier interface {
	QueryRow(query string, args ...interface{}) *sql.Row
	Exec(query string, args ...interface{}) (sql.Result, error)
}

// EnsureLabel gets or creates a label, handling renames and ID changes.
// For batch operations prefer EnsureLabelsBatch which runs in a single
// transaction.
func (s *Store) EnsureLabel(
	sourceID int64,
	sourceLabelID, name, labelType string,
) (int64, error) {
	var id int64
	err := s.withTx(func(tx *loggedTx) error {
		var txErr error
		id, txErr = ensureLabelWith(
			tx, sourceID, sourceLabelID, name, labelType,
		)
		return txErr
	})
	return id, err
}

// ensureLabelWith is the core label-upsert logic, parameterised on the
// database handle so it works both standalone and inside a transaction.
// The handle is expected to be *loggedDB or *loggedTx so placeholder
// rebinding is applied automatically.
//
// Labels are identified by source_label_id (Gmail label ID) but have a
// UNIQUE constraint on (source_id, name). This function handles:
//   - Existing label found by source_label_id: updates name if renamed
//   - Name conflict with different source_label_id: upserts, adopting
//     the new source_label_id (handles deleted+recreated labels, imports)
func ensureLabelWith(
	q dbQuerier,
	sourceID int64,
	sourceLabelID, name, labelType string,
) (int64, error) {
	// Look up by canonical identifier (Gmail label ID).
	var id int64
	var existingName string
	err := q.QueryRow(`
		SELECT id, name FROM labels
		WHERE source_id = ? AND source_label_id = ?
	`, sourceID, sourceLabelID).Scan(&id, &existingName)

	if err == nil {
		if existingName == name {
			return id, nil
		}
		// Label was renamed — update the name. If another row already
		// claims the target name, merge it: move its message-label
		// associations to the canonical row and delete the stale one.
		if err = mergeLabelByName(q, sourceID, name, id); err != nil {
			return 0, err
		}
		if _, err = q.Exec(`
			UPDATE labels SET name = ?, label_type = ?
			WHERE id = ?
		`, name, labelType, id); err != nil {
			return 0, fmt.Errorf("update label name: %w", err)
		}
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}

	// Not found by source_label_id — upsert by name. Handles the case
	// where a label with this name exists from a previous import or
	// with a stale/NULL source_label_id.
	if _, err = q.Exec(`
		INSERT INTO labels (source_id, source_label_id, name, label_type)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(source_id, name) DO UPDATE SET
			source_label_id = excluded.source_label_id,
			label_type = excluded.label_type
	`, sourceID, sourceLabelID, name, labelType); err != nil {
		return 0, err
	}

	err = q.QueryRow(`
		SELECT id FROM labels WHERE source_id = ? AND name = ?
	`, sourceID, name).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// mergeLabelByName finds a label with the given name (excluding keepID)
// and merges it into keepID: message-label associations are reassigned
// and the stale row is deleted. No-op if no conflicting label exists.
func mergeLabelByName(
	q dbQuerier, sourceID int64, name string, keepID int64,
) error {
	var conflictID int64
	err := q.QueryRow(`
		SELECT id FROM labels
		WHERE source_id = ? AND name = ? AND id != ?
	`, sourceID, name, keepID).Scan(&conflictID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find conflicting label: %w", err)
	}
	// Drop associations that would conflict after reassignment (message
	// already linked to keepID). This is the portable equivalent of
	// SQLite's UPDATE OR IGNORE — done explicitly so PostgreSQL works the
	// same way.
	if _, err = q.Exec(`
		DELETE FROM message_labels
		WHERE label_id = ?
		AND message_id IN (
			SELECT message_id FROM message_labels WHERE label_id = ?
		)
	`, conflictID, keepID); err != nil {
		return fmt.Errorf("drop conflicting associations: %w", err)
	}
	// Reassign the remaining associations (no PK violations possible now).
	if _, err = q.Exec(`
		UPDATE message_labels SET label_id = ? WHERE label_id = ?
	`, keepID, conflictID); err != nil {
		return fmt.Errorf("reassign label associations: %w", err)
	}
	if _, err = q.Exec(`
		DELETE FROM labels WHERE id = ?
	`, conflictID); err != nil {
		return fmt.Errorf("delete conflicting label: %w", err)
	}
	return nil
}

// LabelInfo holds the name and type for a label to be ensured.
type LabelInfo struct {
	Name string
	Type string // "system" or "user"
}

// IsSystemLabel returns true if the given Gmail label ID represents a system label.
func IsSystemLabel(sourceLabelID string) bool {
	switch sourceLabelID {
	case "INBOX", "SENT", "TRASH", "SPAM", "DRAFT", "UNREAD", "STARRED", "IMPORTANT":
		return true
	}
	return strings.HasPrefix(sourceLabelID, "CATEGORY_")
}

// EnsureLabelsBatch ensures all labels exist and returns a map of
// source_label_id -> internal ID. Runs in a single transaction with
// a two-phase rename to handle cross-renames safely (e.g. L1:Foo→Bar
// and L2:Bar→Foo in the same batch).
func (s *Store) EnsureLabelsBatch(
	sourceID int64, labels map[string]LabelInfo,
) (map[string]int64, error) {
	result := make(map[string]int64, len(labels))
	err := s.withTx(func(tx *loggedTx) error {
		// Phase 1: Move all renamed labels to temporary names so
		// that cross-renames don't cause one label to incorrectly
		// merge the other. Temp names use the row PK (unique by
		// construction) with a prefix that can't be a real label.
		for sourceLabelID, info := range labels {
			var id int64
			var curName string
			err := tx.QueryRow(`
				SELECT id, name FROM labels
				WHERE source_id = ? AND source_label_id = ?
			`, sourceID, sourceLabelID).Scan(&id, &curName)
			if err == sql.ErrNoRows || curName == info.Name {
				continue
			}
			if err != nil {
				return fmt.Errorf(
					"check label %s: %w", sourceLabelID, err,
				)
			}
			if _, err = tx.Exec(`
				UPDATE labels SET name = CAST(id AS TEXT) || X'00'
				WHERE id = ?
			`, id); err != nil {
				return fmt.Errorf(
					"clear name for label %s: %w", sourceLabelID, err,
				)
			}
		}

		// Phase 2: Apply final names. After phase 1 any remaining
		// name conflict is from a label NOT in this batch, which
		// is safe to merge (dead/imported label).
		for sourceLabelID, info := range labels {
			id, err := ensureLabelWith(
				tx, sourceID, sourceLabelID, info.Name, info.Type,
			)
			if err != nil {
				return err
			}
			result[sourceLabelID] = id
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ReplaceMessageLabels replaces all labels for a message atomically.
func (s *Store) ReplaceMessageLabels(messageID int64, labelIDs []int64) error {
	return s.withTx(func(tx *loggedTx) error {
		return replaceMessageLabelsTx(tx, messageID, labelIDs)
	})
}

func replaceMessageLabelsTx(tx *loggedTx, messageID int64, labelIDs []int64) error {
	_, err := tx.Exec(`
		DELETE FROM message_labels WHERE message_id = ?
	`, messageID)
	if err != nil {
		return err
	}

	if len(labelIDs) == 0 {
		return nil
	}

	return insertInChunks(tx, chunkInsert{
		totalRows:    len(labelIDs),
		valuesPerRow: 2,
		prefix:       "INSERT INTO message_labels (message_id, label_id) VALUES ",
	}, func(start, end int) ([]string, []interface{}) {
		values := make([]string, end-start)
		args := make([]interface{}, 0, (end-start)*2)
		for i := start; i < end; i++ {
			values[i-start] = "(?, ?)"
			args = append(args, messageID, labelIDs[i])
		}
		return values, args
	})
}

// AddMessageLabels adds labels to a message without removing existing ones.
// Uses INSERT OR IGNORE to skip labels that already exist.
func (s *Store) AddMessageLabels(messageID int64, labelIDs []int64) error {
	if len(labelIDs) == 0 {
		return nil
	}
	return s.withTx(func(tx *loggedTx) error {
		return insertInChunks(tx, chunkInsert{
			totalRows:    len(labelIDs),
			valuesPerRow: 2,
			prefix:       s.dialect.InsertOrIgnorePrefix("INSERT OR IGNORE INTO message_labels (message_id, label_id) VALUES "),
			suffix:       s.dialect.InsertOrIgnoreSuffix(),
		}, func(start, end int) ([]string, []interface{}) {
			values := make([]string, end-start)
			args := make([]interface{}, 0, (end-start)*2)
			for i := start; i < end; i++ {
				values[i-start] = "(?, ?)"
				args = append(args, messageID, labelIDs[i])
			}
			return values, args
		})
	})
}

// LinkMessageLabel links a single label to a message.
// Uses INSERT OR IGNORE — safe to call multiple times.
func (s *Store) LinkMessageLabel(messageID, labelID int64) error {
	return s.AddMessageLabels(messageID, []int64{labelID})
}

// RemoveMessageLabels removes specific labels from a message.
func (s *Store) RemoveMessageLabels(messageID int64, labelIDs []int64) error {
	if len(labelIDs) == 0 {
		return nil
	}
	return execInChunks(s.db, labelIDs, []interface{}{messageID},
		`DELETE FROM message_labels WHERE message_id = ? AND label_id IN (%s)`)
}

// MarkMessageDeleted marks a message as deleted from the source.
func (s *Store) MarkMessageDeleted(sourceID int64, sourceMessageID string) error {
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE messages
		SET deleted_from_source_at = %s
		WHERE source_id = ? AND source_message_id = ?
	`, s.dialect.Now()), sourceID, sourceMessageID)
	return err
}

// MarkMessagesDeletedBatch marks multiple messages as deleted from the source in a single transaction.
func (s *Store) MarkMessagesDeletedBatch(sourceID int64, sourceMessageIDs []string) error {
	if len(sourceMessageIDs) == 0 {
		return nil
	}
	return execInChunks(s.db, sourceMessageIDs, []interface{}{sourceID},
		fmt.Sprintf(`UPDATE messages SET deleted_from_source_at = %s WHERE source_id = ? AND source_message_id IN (%%s)`, s.dialect.Now()))
}

// MarkMessageDeletedByGmailID marks a message as deleted by its Gmail ID.
// This is used by the deletion executor which only has the Gmail message ID.
// When permanent is true, the message row is deleted entirely; otherwise it is
// soft-deleted by setting deleted_from_source_at.
func (s *Store) MarkMessageDeletedByGmailID(permanent bool, gmailID string) error {
	if permanent {
		_, err := s.db.Exec(`DELETE FROM messages WHERE source_message_id = ?`, gmailID)
		return err
	}
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE messages
		SET deleted_from_source_at = %s
		WHERE source_message_id = ?
	`, s.dialect.Now()), gmailID)
	return err
}

// MarkMessagesDeletedByGmailIDBatch marks multiple messages as deleted by their Gmail IDs
// in batched UPDATE statements. Much faster than individual MarkMessageDeletedByGmailID calls
// because it issues one UPDATE per chunk instead of one per message.
//
// Uses best-effort semantics: if a chunk fails, it falls back to individual updates
// for that chunk and continues with remaining chunks. Returns the first error encountered
// (if any) after processing all IDs.
func (s *Store) MarkMessagesDeletedByGmailIDBatch(gmailIDs []string) error {
	if len(gmailIDs) == 0 {
		return nil
	}

	const chunkSize = 500
	var firstErr error

	for i := 0; i < len(gmailIDs); i += chunkSize {
		end := i + chunkSize
		if end > len(gmailIDs) {
			end = len(gmailIDs)
		}
		chunk := gmailIDs[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]interface{}, len(chunk))
		for j, id := range chunk {
			placeholders[j] = "?"
			args[j] = id
		}

		query := fmt.Sprintf(
			`UPDATE messages SET deleted_from_source_at = %s WHERE source_message_id IN (%s)`,
			s.dialect.Now(), strings.Join(placeholders, ","))

		if _, err := s.db.Exec(query, args...); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// Fall back to individual updates for this chunk
			for _, id := range chunk {
				s.MarkMessageDeletedByGmailID(false, id) //nolint:errcheck // best-effort
			}
		}
	}

	return firstErr
}

// CountMessagesForSource returns the count of messages for a specific source (account).
func (s *Store) CountMessagesForSource(sourceID int64) (int64, error) {
	var count int64
	err := s.db.QueryRow(fmt.Sprintf(`
		SELECT COUNT(*) FROM messages WHERE source_id = ? AND %s
	`, LiveMessagesWhere("", true)), sourceID).Scan(&count)
	return count, err
}

// CountMessagesWithRaw returns the count of messages that have raw MIME stored.
func (s *Store) CountMessagesWithRaw(sourceID int64) (int64, error) {
	var count int64
	err := s.db.QueryRow(fmt.Sprintf(`
		SELECT COUNT(*) FROM messages m
		JOIN message_raw mr ON m.id = mr.message_id
		WHERE m.source_id = ? AND %s
	`, LiveMessagesWhere("m", true)), sourceID).Scan(&count)
	return count, err
}

// GetRandomMessageIDs returns a random sample of message IDs for a source.
// Uses reservoir sampling with random offsets for O(limit) performance on large tables,
// falling back to ORDER BY RANDOM() for small tables where the overhead isn't significant.
func (s *Store) GetRandomMessageIDs(sourceID int64, limit int) ([]int64, error) {
	live := LiveMessagesWhere("", true)
	// Get total count first
	var total int64
	err := s.db.QueryRow(fmt.Sprintf(`
		SELECT COUNT(*) FROM messages
		WHERE source_id = ? AND %s
	`, live), sourceID).Scan(&total)
	if err != nil {
		return nil, err
	}

	if total == 0 {
		return nil, nil
	}

	// For small tables or when limit >= total, use simple ORDER BY RANDOM()
	// The threshold of 10000 balances query overhead vs. scan cost
	if total < 10000 || int64(limit) >= total {
		rows, err := s.db.Query(fmt.Sprintf(`
			SELECT id FROM messages
			WHERE source_id = ? AND %s
			ORDER BY RANDOM()
			LIMIT ?
		`, live), sourceID, limit)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()

		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, rows.Err()
	}

	// For large tables, use random offset sampling
	// This is O(limit) instead of O(n) for ORDER BY RANDOM()
	// Generate random offsets in Go for dialect portability (SQLite vs Postgres)
	// Use explicitly seeded RNG for true randomness across process runs
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	ids := make([]int64, 0, limit)
	seen := make(map[int64]bool)

	for len(ids) < limit {
		// Generate random offset in Go (portable across SQLite/Postgres)
		offset := rng.Int63n(total)

		var id int64
		err := s.db.QueryRow(fmt.Sprintf(`
			SELECT id FROM messages
			WHERE source_id = ? AND %s
			ORDER BY id
			LIMIT 1 OFFSET ?
		`, live), sourceID, offset).Scan(&id)
		if err != nil {
			if err == sql.ErrNoRows {
				continue // Race condition with deletions, retry
			}
			return nil, err
		}

		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}

	return ids, nil
}

// UpsertFTS inserts or updates the FTS index for a message.
// No-op if FTS is not available.
func (s *Store) UpsertFTS(messageID int64, subject, bodyText, fromAddr, toAddrs, ccAddrs string) error {
	if !s.fts5Available {
		return nil
	}
	return s.dialect.FTSUpsert(s.db, FTSDoc{
		MessageID: messageID,
		Subject:   subject,
		Body:      bodyText,
		FromAddr:  fromAddr,
		ToAddrs:   toAddrs,
		CcAddrs:   ccAddrs,
	})
}

// BackfillFTS populates the FTS table from existing message data.
// Processes in batches to avoid blocking for minutes on large archives.
// The progress callback (if non-nil) is called after each batch with
// (position in ID range, total ID range). Each batch is committed
// independently so partial progress is preserved if interrupted.
// Returns the number of rows inserted. No-op if FTS5 is not available.
//
// BackfillFTS clears FTS rows with DELETE before inserting. If the FTS5
// shadow tables are themselves malformed, that DELETE will either fail or
// leave corruption in place — callers recovering from shadow-table
// corruption should use RebuildFTS instead.
func (s *Store) BackfillFTS(progress func(done, total int64)) (int64, error) {
	if !s.fts5Available {
		return 0, nil
	}

	minID, maxID, err := s.messageIDRange()
	if err != nil {
		return 0, err
	}
	if maxID == 0 {
		return 0, nil
	}

	if _, err := s.db.Exec(s.dialect.FTSClearSQL()); err != nil {
		return 0, fmt.Errorf("clear FTS: %w", err)
	}

	return s.backfillFTSRange(minID, maxID, progress)
}

// RebuildFTS fully recreates the FTS index from the underlying message
// tables. Unlike BackfillFTS (DELETE + INSERT), this drops and recreates
// the FTS table itself so malformed FTS5 shadow tables are fully replaced.
//
// Ignores the cached fts5Available flag: a corrupt shadow table causes the
// availability probe to fail, which is precisely the symptom this method
// exists to recover from. On successful completion, fts5Available is set to
// true. Returns an error if the binary was built without FTS5 support.
func (s *Store) RebuildFTS(progress func(done, total int64)) (int64, error) {
	if err := s.dialect.FTSRebuildSchema(s.db.DB); err != nil {
		return 0, err
	}

	minID, maxID, err := s.messageIDRange()
	if err != nil {
		return 0, err
	}
	if maxID == 0 {
		s.fts5Available = true
		return 0, nil
	}

	indexed, err := s.backfillFTSRange(minID, maxID, progress)
	if err != nil {
		return indexed, err
	}
	s.fts5Available = true
	return indexed, nil
}

// messageIDRange returns (minID, maxID) using MIN/MAX B-tree lookups
// rather than COUNT(*), which would scan the whole table.
func (s *Store) messageIDRange() (int64, int64, error) {
	var minID, maxID int64
	err := s.db.QueryRow(
		"SELECT COALESCE(MIN(id),0), COALESCE(MAX(id),0) FROM messages",
	).Scan(&minID, &maxID)
	if err != nil {
		return 0, 0, fmt.Errorf("get message ID range: %w", err)
	}
	return minID, maxID, nil
}

// backfillFTSRange inserts FTS rows for all messages with id in [minID, maxID],
// in batches. Shared between BackfillFTS (DELETE+fill) and RebuildFTS
// (DROP+CREATE+fill). Each batch is committed independently so partial
// progress is preserved if interrupted.
func (s *Store) backfillFTSRange(minID, maxID int64, progress func(done, total int64)) (int64, error) {
	const batchSize = 5000
	idRange := maxID - minID + 1
	var indexed int64
	cursor := minID

	for cursor <= maxID {
		batchEnd := cursor + batchSize
		n, err := s.backfillFTSBatch(cursor, batchEnd)
		if err != nil {
			return indexed, fmt.Errorf("backfill batch [%d,%d): %w", cursor, batchEnd, err)
		}
		indexed += n
		cursor = batchEnd

		if progress != nil {
			pos := cursor - minID
			if pos > idRange {
				pos = idRange
			}
			progress(pos, idRange)
		}
	}
	return indexed, nil
}

// backfillFTSBatch inserts FTS rows for messages with id in [fromID, toID).
func (s *Store) backfillFTSBatch(fromID, toID int64) (int64, error) {
	result, err := s.db.Exec(s.dialect.FTSBackfillBatchSQL(), fromID, toID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// RecomputeConversationStats updates the denormalized stats columns on all conversations
// belonging to the given source. It recomputes message_count, participant_count,
// last_message_at, and last_message_preview from the current table state.
// Safe to call multiple times — always produces the same result (idempotent).
func (s *Store) RecomputeConversationStats(sourceID int64) error {
	_, err := s.db.Exec(`
		UPDATE conversations SET
			message_count = (
				SELECT COUNT(*) FROM messages
				WHERE conversation_id = conversations.id
			),
			participant_count = (
				SELECT COUNT(*) FROM conversation_participants
				WHERE conversation_id = conversations.id
			),
			last_message_at = (
				SELECT MAX(COALESCE(sent_at, received_at, internal_date))
				FROM messages
				WHERE conversation_id = conversations.id
			),
			last_message_preview = (
				SELECT snippet FROM messages
				WHERE conversation_id = conversations.id
				ORDER BY COALESCE(sent_at, received_at, internal_date) DESC, id DESC
				LIMIT 1
			)
		WHERE source_id = ?
	`, sourceID)
	if err != nil {
		return fmt.Errorf("recompute conversation stats: %w", err)
	}
	return nil
}

// EnsureConversationWithType gets or creates a conversation with an explicit conversation_type.
// Unlike EnsureConversation (which hardcodes 'email_thread'), this accepts the type as a parameter,
// making it suitable for WhatsApp and other messaging platforms.
func (s *Store) EnsureConversationWithType(sourceID int64, sourceConversationID, conversationType, title string) (int64, error) {
	// Try to get existing
	var id int64
	err := s.db.QueryRow(`
		SELECT id FROM conversations
		WHERE source_id = ? AND source_conversation_id = ?
	`, sourceID, sourceConversationID).Scan(&id)

	if err == nil {
		// Update conversation_type and title if they've changed.
		// Only update title when the new value is non-empty (don't blank out existing titles).
		now := s.dialect.Now()
		if title != "" {
			_, _ = s.db.Exec(fmt.Sprintf(`
				UPDATE conversations SET conversation_type = ?, title = ?, updated_at = %s
				WHERE id = ? AND (conversation_type != ? OR title != ? OR title IS NULL)
			`, now), conversationType, title, id, conversationType, title)
		} else {
			_, _ = s.db.Exec(fmt.Sprintf(`
				UPDATE conversations SET conversation_type = ?, updated_at = %s
				WHERE id = ? AND conversation_type != ?
			`, now), conversationType, id, conversationType)
		}
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}

	// Create new
	now := s.dialect.Now()
	result, err := s.db.Exec(fmt.Sprintf(`
		INSERT INTO conversations (source_id, source_conversation_id, conversation_type, title, created_at, updated_at)
		VALUES (?, ?, ?, ?, %s, %s)
	`, now, now), sourceID, sourceConversationID, conversationType, title)
	if err != nil {
		return 0, err
	}

	return result.LastInsertId()
}

// EnsureParticipantByPhone gets or creates a participant by phone number.
// Phone must start with "+" (E.164 format). Returns an error for empty or
// invalid phone numbers to prevent database pollution.
// Also creates a participant_identifiers row with the given identifierType
// (e.g., "whatsapp", "imessage", "google_voice").
func (s *Store) EnsureParticipantByPhone(phone, displayName, identifierType string) (int64, error) {
	if phone == "" {
		return 0, fmt.Errorf("phone number is required")
	}
	if !strings.HasPrefix(phone, "+") {
		return 0, fmt.Errorf("phone number must be in E.164 format (starting with +), got %q", phone)
	}

	// Try to get existing by phone
	var id int64
	err := s.db.QueryRow(`
		SELECT id FROM participants WHERE phone_number = ?
	`, phone).Scan(&id)

	if err == nil {
		// Update display name if provided and currently empty
		if displayName != "" {
			_, _ = s.db.Exec(`
				UPDATE participants SET display_name = ?
				WHERE id = ? AND (display_name IS NULL OR display_name = '')
			`, displayName, id) // best-effort display name update, ignore error
		}
	} else if err != sql.ErrNoRows {
		return 0, err
	} else {
		// Create new participant
		now := s.dialect.Now()
		result, err := s.db.Exec(fmt.Sprintf(`
			INSERT INTO participants (phone_number, display_name, created_at, updated_at)
			VALUES (?, ?, %s, %s)
		`, now, now), phone, displayName)
		if err != nil {
			return 0, fmt.Errorf("insert participant: %w", err)
		}

		id, err = result.LastInsertId()
		if err != nil {
			return 0, err
		}
	}

	// Ensure a participant_identifiers row exists for this identifierType.
	// INSERT OR IGNORE is idempotent: a second call with the same type is a no-op.
	_, err = s.db.Exec(s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO participant_identifiers (participant_id, identifier_type, identifier_value, is_primary)
		VALUES (?, ?, ?, TRUE)`), id, identifierType, phone)
	if err != nil {
		return 0, fmt.Errorf("insert participant identifier: %w", err)
	}

	return id, nil
}

// UpdateParticipantDisplayNameByPhone updates the display_name for an existing
// participant identified by phone number. Only updates if display_name is currently
// empty. Returns true if a participant was found and updated, false if not found
// or name was already set. Does NOT create new participants.
func (s *Store) UpdateParticipantDisplayNameByPhone(phone, displayName string) (bool, error) {
	if phone == "" || displayName == "" {
		return false, nil
	}

	result, err := s.db.Exec(fmt.Sprintf(`
		UPDATE participants SET display_name = ?, updated_at = %s
		WHERE phone_number = ? AND (display_name IS NULL OR display_name = '')
	`, s.dialect.Now()), displayName, phone)
	if err != nil {
		return false, err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// EnsureConversationParticipant adds a participant to a conversation.
// Uses INSERT OR IGNORE to be idempotent.
func (s *Store) EnsureConversationParticipant(conversationID, participantID int64, role string) error {
	_, err := s.db.Exec(s.dialect.InsertOrIgnore(fmt.Sprintf(`INSERT OR IGNORE INTO conversation_participants (conversation_id, participant_id, role, joined_at)
		VALUES (?, ?, ?, %s)`, s.dialect.Now())), conversationID, participantID, role)
	return err
}

// UpsertReaction inserts or ignores a reaction.
func (s *Store) UpsertReaction(messageID, participantID int64, reactionType, reactionValue string, createdAt time.Time) error {
	_, err := s.db.Exec(s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO reactions (message_id, participant_id, reaction_type, reaction_value, created_at)
		VALUES (?, ?, ?, ?, ?)`), messageID, participantID, reactionType, reactionValue, createdAt)
	return err
}

// UpsertMessageRawWithFormat stores compressed raw data with an explicit format.
// Unlike UpsertMessageRaw (which hardcodes 'mime'), this accepts the format as a parameter.
func (s *Store) UpsertMessageRawWithFormat(messageID int64, rawData []byte, format string) error {
	// Compress with zlib
	var compressed bytes.Buffer
	w := zlib.NewWriter(&compressed)
	if _, err := w.Write(rawData); err != nil {
		return fmt.Errorf("compress: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close compressor: %w", err)
	}

	_, err := s.db.Exec(`
		INSERT INTO message_raw (message_id, raw_data, raw_format, compression)
		VALUES (?, ?, ?, 'zlib')
		ON CONFLICT(message_id) DO UPDATE SET
			raw_data = excluded.raw_data,
			raw_format = excluded.raw_format,
			compression = excluded.compression
	`, messageID, compressed.Bytes(), format)
	return err
}

// AttachmentPathsUniqueToSource returns storage_path values for attachments
// belonging to sourceID whose content_hash is not shared with any other source.
// Call this before RemoveSource so the cascade hasn't run yet.
//
// Note: thumbnail_path values are not included. No sync/import code currently
// writes thumbnail files to disk, so there are no thumbnail files to clean up.
// If thumbnail storage is added in the future, this function and the delete
// loop in remove_account.go must be extended to cover thumbnail_path as well.
func (s *Store) AttachmentPathsUniqueToSource(sourceID int64) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT a.storage_path
		FROM attachments a
		WHERE EXISTS (
		    SELECT 1 FROM messages m
		    WHERE m.id = a.message_id AND m.source_id = ?
		  )
		  AND a.content_hash IS NOT NULL
		  AND a.storage_path IS NOT NULL
		  AND a.storage_path != ''
		  AND NOT EXISTS (
		      SELECT 1 FROM attachments a2
		      WHERE a2.content_hash = a.content_hash
		        AND EXISTS (
		            SELECT 1 FROM messages m2
		            WHERE m2.id = a2.message_id AND m2.source_id != ?
		        )
		  )
	`, sourceID, sourceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// IsAttachmentPathReferenced returns true if any attachment record still
// points to the given storage_path. Use this immediately before deleting a
// file to guard against a concurrent sync that added a new reference after
// the candidate list was collected.
func (s *Store) IsAttachmentPathReferenced(storagePath string) (bool, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM attachments WHERE storage_path = ?`,
		storagePath,
	).Scan(&count)
	if err != nil {
		return true, err // fail safe: treat error as referenced
	}
	return count > 0, nil
}

// UpsertAttachment stores an attachment record.
func (s *Store) UpsertAttachment(messageID int64, filename, mimeType, storagePath, contentHash string, size int) error {
	// Check if attachment already exists (by message_id and content_hash)
	var existingID int64
	err := s.db.QueryRow(`
		SELECT id FROM attachments WHERE message_id = ? AND content_hash = ?
	`, messageID, contentHash).Scan(&existingID)

	if err == nil {
		// Already exists, nothing to do
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}

	// Insert new attachment
	_, err = s.db.Exec(fmt.Sprintf(`
		INSERT INTO attachments (message_id, filename, mime_type, storage_path, content_hash, size, created_at)
		VALUES (?, ?, ?, ?, ?, ?, %s)
	`, s.dialect.Now()), messageID, filename, mimeType, storagePath, contentHash, size)
	return err
}
