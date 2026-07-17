package store

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"regexp"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/mime"
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
	Message        *Message
	Conversation   *ConversationPersistData
	Metadata       *sql.NullString
	BodyText       sql.NullString
	BodyHTML       sql.NullString
	RawMIME        []byte
	RawFormat      string
	Recipients     []RecipientSet
	LabelIDs       []int64
	PreserveLabels bool
	FTS            *FTSDoc
}

// ConversationPersistData optionally makes conversation identity, title, and
// membership part of PersistMessage's transaction. When absent, PersistMessage
// uses Message.ConversationID as before.
type ConversationPersistData struct {
	SourceConversationID string
	ConversationType     string
	Title                string
	Participants         []ConversationParticipantRef
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

// MessageMetadataRecord is the archive identity and optional provider metadata
// for one message returned by MessageMetadataBatch.
type MessageMetadataRecord struct {
	ID       int64
	Metadata sql.NullString
}

// MessageExistsBatch checks which message IDs already exist in the database.
// Returns a map of source_message_id -> internal message_id for existing messages.
func (s *Store) MessageExistsBatch(sourceID int64, sourceMessageIDs []string) (map[string]int64, error) {
	if len(sourceMessageIDs) == 0 {
		return make(map[string]int64), nil
	}

	result := make(map[string]int64)
	err := queryInChunks(s.db, sourceMessageIDs, []any{sourceID},
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

// MessageMetadataBatch looks up archive IDs and provider metadata for messages
// from one source. Importers use this instead of issuing one metadata query per
// known item while filtering provider search pages.
func (s *Store) MessageMetadataBatch(
	sourceID int64, sourceMessageIDs []string,
) (map[string]MessageMetadataRecord, error) {
	if len(sourceMessageIDs) == 0 {
		return make(map[string]MessageMetadataRecord), nil
	}

	result := make(map[string]MessageMetadataRecord)
	err := queryInChunks(s.db, sourceMessageIDs, []any{sourceID},
		`SELECT source_message_id, id, metadata
		 FROM messages
		 WHERE source_id = ? AND source_message_id IN (%s)`,
		func(rows *loggedRows) error {
			var sourceMessageID string
			var record MessageMetadataRecord
			if err := rows.Scan(&sourceMessageID, &record.ID, &record.Metadata); err != nil {
				return err
			}
			result[sourceMessageID] = record
			return nil
		})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// SetMessageMetadata writes the messages.metadata JSON/JSONB column for an
// already-persisted message. The column exists in both dialects (schema.sql:
// `metadata JSON`, schema_pg.sql: `metadata JSONB`) but the hot upsertMessageSQL
// path never writes it, so non-email importers that need structured per-message
// metadata (e.g. calendar events: end/all_day/status/recurrence) call this
// immediately after UpsertMessage returns the id. Passing an invalid
// sql.NullString writes SQL NULL, clearing the column. The dialect supplies the
// JSONB cast on PG (?::JSONB) and a bare ? on SQLite, so a JSON string binds in
// both backends.
func (s *Store) SetMessageMetadata(messageID int64, metadata sql.NullString) error {
	return setMessageMetadataWith(s.db, s.dialect, messageID, metadata)
}

func setMessageMetadataWith(q querier, dialect Dialect, messageID int64, metadata sql.NullString) error {
	_, err := q.Exec(fmt.Sprintf(`
		UPDATE messages
		SET metadata = %s
		WHERE id = ?
	`, dialect.JSONBindExpr()), metadata, messageID)
	if err != nil {
		return fmt.Errorf("set message metadata (id=%d): %w", messageID, err)
	}
	return err
}

// GetMessageMetadata reads the messages.metadata column for a message. It is
// the read counterpart to SetMessageMetadata; importers use it to merge a flag
// into existing metadata (e.g. flipping a calendar event to status=cancelled)
// without losing the rest of the stored JSON. Returns an invalid NullString when
// the column is NULL.
func (s *Store) GetMessageMetadata(messageID int64) (sql.NullString, error) {
	var meta sql.NullString
	err := s.db.QueryRow(`SELECT metadata FROM messages WHERE id = ?`, messageID).Scan(&meta)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("get message metadata (id=%d): %w", messageID, err)
	}
	return meta, nil
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

// MigrateSourceMessageID rewrites a legacy source_message_id to a new value
// for one conversation. If the new ID already exists, dependents are repointed
// and the legacy row is removed so future imports converge on the new key.
func (s *Store) MigrateSourceMessageID(sourceID, conversationID int64, legacySourceMessageID, newSourceMessageID string) error {
	if legacySourceMessageID == "" || legacySourceMessageID == newSourceMessageID {
		return nil
	}
	return s.withTx(func(tx *loggedTx) error {
		var newID int64
		err := tx.QueryRow(
			`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`,
			sourceID, newSourceMessageID,
		).Scan(&newID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("find migrated message id: %w", err)
		}
		if err == nil {
			if _, err = tx.Exec(
				`UPDATE messages SET deleted_from_source_at = NULL WHERE id = ?`,
				newID,
			); err != nil {
				return fmt.Errorf("clear migrated message deletion marker: %w", err)
			}

			var legacyID int64
			legacyErr := tx.QueryRow(
				`SELECT id FROM messages
				 WHERE source_id = ? AND conversation_id = ? AND source_message_id = ?`,
				sourceID, conversationID, legacySourceMessageID,
			).Scan(&legacyID)
			if legacyErr != nil && !errors.Is(legacyErr, sql.ErrNoRows) {
				return fmt.Errorf("find legacy message id: %w", legacyErr)
			}
			if legacyErr == nil {
				if _, err = tx.Exec(
					`UPDATE messages SET reply_to_message_id = ?
					 WHERE reply_to_message_id = ?`,
					newID, legacyID,
				); err != nil {
					return fmt.Errorf("repoint legacy replies: %w", err)
				}
			}

			_, err = tx.Exec(
				`DELETE FROM messages
				 WHERE source_id = ? AND conversation_id = ? AND source_message_id = ?`,
				sourceID, conversationID, legacySourceMessageID,
			)
			if err != nil {
				return fmt.Errorf("delete legacy source_message_id: %w", err)
			}
			return nil
		}

		_, err = tx.Exec(
			`UPDATE messages
			 SET source_message_id = ?, deleted_from_source_at = NULL
			 WHERE source_id = ? AND conversation_id = ? AND source_message_id = ?`,
			newSourceMessageID, sourceID, conversationID, legacySourceMessageID,
		)
		if err != nil {
			return fmt.Errorf("migrate source_message_id: %w", err)
		}
		return nil
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
	err := queryInChunks(s.db, sourceMessageIDs, []any{sourceID},
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
// Concurrent first-inserts converge via INSERT ... ON CONFLICT DO UPDATE
// RETURNING id: the no-op SET fires the RETURNING clause for both the
// insert and the conflict path, so the second caller still receives the
// existing row's id instead of a unique-violation error.
func (s *Store) EnsureConversation(sourceID int64, sourceConversationID, title string) (int64, error) {
	now := s.dialect.Now()
	var id int64
	err := s.db.QueryRow(fmt.Sprintf(`
		INSERT INTO conversations (source_id, source_conversation_id, conversation_type, title, created_at, updated_at)
		VALUES (?, ?, 'email_thread', ?, %s, %s)
		ON CONFLICT (source_id, source_conversation_id) DO UPDATE
		SET source_conversation_id = conversations.source_conversation_id
		RETURNING id
	`, now, now), sourceID, sourceConversationID, title).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
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
		embed_gen = CASE
			WHEN COALESCE(messages.subject, '') <> COALESCE(excluded.subject, '') THEN NULL
			ELSE messages.embed_gen
		END,
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
	return upsertMessageBody(s.db, s.dialect, s.fts5Available, messageID, bodyText, bodyHTML)
}

func upsertMessageBody(
	q querier,
	dialect Dialect,
	ftsAvailable bool,
	messageID int64,
	bodyText, bodyHTML sql.NullString,
) error {
	embeddingChanged, textChanged, err := messageBodyChanges(q, messageID, bodyText, bodyHTML)
	if err != nil {
		return err
	}
	if textChanged && ftsAvailable {
		// Invalidate first. UpsertMessageBody is also used outside a wider
		// transaction; if the body write then fails, a missing index entry is
		// recoverable by backfill, while a stale entry could produce a false hit.
		if err := dialect.InvalidateFTSForMessage(q, messageID); err != nil {
			return fmt.Errorf("invalidate message FTS document: %w", err)
		}
	}
	_, err = q.Exec(`
		INSERT INTO message_bodies (message_id, body_text, body_html)
		VALUES (?, ?, ?)
		ON CONFLICT(message_id) DO UPDATE SET
			body_text = excluded.body_text,
			body_html = excluded.body_html
	`, messageID, bodyText, bodyHTML)
	if err != nil {
		return err
	}
	if !embeddingChanged {
		return nil
	}
	_, err = q.Exec(`UPDATE messages SET embed_gen = NULL WHERE id = ? AND embed_gen IS NOT NULL`, messageID)
	return err
}

func messageBodyChanges(
	q querier,
	messageID int64,
	bodyText, bodyHTML sql.NullString,
) (embeddingChanged bool, textChanged bool, err error) {
	var oldText, oldHTML sql.NullString
	err = q.QueryRow(`
		SELECT body_text, body_html FROM message_bodies WHERE message_id = ?
	`, messageID).Scan(&oldText, &oldHTML)
	if errors.Is(err, sql.ErrNoRows) {
		return embeddingBodyValue(bodyText, bodyHTML) != "",
			nullStringValue(bodyText) != "", nil
	}
	if err != nil {
		return false, false, err
	}
	return embeddingBodyValue(oldText, oldHTML) != embeddingBodyValue(bodyText, bodyHTML),
		nullStringValue(oldText) != nullStringValue(bodyText), nil
}

func embeddingBodyValue(bodyText, bodyHTML sql.NullString) string {
	if v := nullStringValue(bodyText); v != "" {
		return v
	}
	return mime.StripHTML(nullStringValue(bodyHTML))
}

func nullStringValue(ns sql.NullString) string {
	if !ns.Valid {
		return ""
	}
	return ns.String
}

// UpsertMessageRaw stores the compressed raw MIME data for a message.
func (s *Store) UpsertMessageRaw(messageID int64, rawData []byte) error {
	return upsertMessageRaw(s.db, messageID, rawData)
}

func upsertMessageRaw(q querier, messageID int64, rawData []byte) error {
	return upsertMessageRawWithFormat(q, messageID, rawData, "mime")
}

func upsertMessageRawWithFormat(q querier, messageID int64, rawData []byte, format string) error {
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
		VALUES (?, ?, ?, 'zlib')
		ON CONFLICT(message_id) DO UPDATE SET
			raw_data = excluded.raw_data,
			raw_format = excluded.raw_format,
			compression = excluded.compression
	`, messageID, compressed.Bytes(), format)
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

// PersistMessage atomically stores a message and its requested related
// snapshots in one transaction. Existing email callers persist body, raw MIME,
// recipients, and labels; non-email callers can additionally include
// conversation state, metadata, provider raw data, and FTS.
func (s *Store) PersistMessage(data *MessagePersistData) (int64, error) {
	if data == nil || data.Message == nil {
		return 0, errors.New("persist message requires a message")
	}
	var messageID int64
	err := s.withTx(func(tx *loggedTx) error {
		message := data.Message
		if data.Conversation != nil {
			conversationID, err := ensureConversationWithType(
				tx, s.dialect, data.Message.SourceID,
				data.Conversation.SourceConversationID,
				data.Conversation.ConversationType,
				data.Conversation.Title,
			)
			if err != nil {
				return fmt.Errorf("ensure conversation: %w", err)
			}
			if err := replaceConversationParticipantsTx(
				tx, s.dialect, conversationID, data.Conversation.Participants,
			); err != nil {
				return fmt.Errorf("replace conversation participants: %w", err)
			}
			messageCopy := *data.Message
			messageCopy.ConversationID = conversationID
			message = &messageCopy
		}

		id, err := upsertMessageWith(tx, s.dialect, message)
		if err != nil {
			return fmt.Errorf("upsert message: %w", err)
		}
		messageID = id
		if data.Metadata != nil {
			if err := setMessageMetadataWith(tx, s.dialect, messageID, *data.Metadata); err != nil {
				return fmt.Errorf("set metadata: %w", err)
			}
		}

		if err := upsertMessageBody(
			tx, s.dialect, s.fts5Available, messageID, data.BodyText, data.BodyHTML,
		); err != nil {
			return fmt.Errorf("upsert body: %w", err)
		}

		if len(data.RawMIME) > 0 {
			rawFormat := data.RawFormat
			if rawFormat == "" {
				rawFormat = "mime"
			}
			if err := upsertMessageRawWithFormat(tx, messageID, data.RawMIME, rawFormat); err != nil {
				return fmt.Errorf("upsert raw: %w", err)
			}
		}

		for _, rs := range data.Recipients {
			if err := replaceMessageRecipientsTx(tx, messageID, rs); err != nil {
				return fmt.Errorf("store %s recipients: %w", rs.Type, err)
			}
		}

		if !data.PreserveLabels {
			if err := replaceMessageLabelsTx(tx, messageID, data.LabelIDs); err != nil {
				return fmt.Errorf("store labels: %w", err)
			}
		}
		if data.FTS != nil && s.fts5Available {
			fts := *data.FTS
			fts.MessageID = messageID
			if err := s.dialect.FTSUpsert(tx, fts); err != nil {
				return fmt.Errorf("upsert fts: %w", err)
			}
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

// EnsureParticipant gets or creates a participant by email. Atomic via
// INSERT … ON CONFLICT … RETURNING id so two goroutines (or two
// processes against PostgreSQL) cannot race between a SELECT-empty and
// the follow-up INSERT and both succeed — one would otherwise lose to
// the unique constraint on (email_address) with a 23505 error. Display
// name and domain are left untouched on conflict to preserve any
// hand-edited values.
func (s *Store) EnsureParticipant(email, displayName, domain string) (int64, error) {
	// ON CONFLICT must mirror the partial unique index on
	// participants(email_address) WHERE email_address IS NOT NULL — both
	// PG and SQLite require the WHERE clause on the conflict target to
	// match the partial index exactly. DO UPDATE (no-op assignment on
	// the same column) makes RETURNING fire for both INSERT and the
	// existing-row case, giving us the id either way.
	var id int64
	err := s.db.QueryRow(fmt.Sprintf(`
		INSERT INTO participants (email_address, display_name, domain, created_at, updated_at)
		VALUES (?, ?, ?, %s, %s)
		ON CONFLICT (email_address) WHERE email_address IS NOT NULL
			DO UPDATE SET email_address = EXCLUDED.email_address
		RETURNING id
	`, s.dialect.Now(), s.dialect.Now()), email, displayName, domain).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
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

	// Collapse duplicate participants within this set. The table holds at most
	// one row per (message_id, participant_id, recipient_type), so a participant
	// repeated in one call — a calendar event listing the same attendee twice, or
	// two address forms that resolve to the same participant — is redundant and
	// would otherwise trip the UNIQUE constraint and abort the entire write. The
	// first occurrence's display name wins.
	seen := make(map[int64]struct{}, len(rs.ParticipantIDs))
	ids := make([]int64, 0, len(rs.ParticipantIDs))
	names := make([]string, 0, len(rs.ParticipantIDs))
	for i, pid := range rs.ParticipantIDs {
		if _, dup := seen[pid]; dup {
			continue
		}
		seen[pid] = struct{}{}
		ids = append(ids, pid)
		name := ""
		if i < len(rs.DisplayNames) {
			name = rs.DisplayNames[i]
		}
		names = append(names, name)
	}

	return insertInChunks(tx, chunkInsert{
		totalRows:    len(ids),
		valuesPerRow: 4,
		prefix:       "INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name) VALUES ",
	}, func(start, end int) ([]string, []any) {
		values := make([]string, end-start)
		args := make([]any, 0, (end-start)*4)
		for i := start; i < end; i++ {
			values[i-start] = "(?, ?, ?, ?)"
			args = append(args, messageID, ids[i], rs.Type, names[i])
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
	q querier,
	sourceID int64,
	sourceLabelID, name, labelType string,
) (int64, error) {
	// Look up by canonical identifier (Gmail label ID).
	var id int64
	var existingName string
	var existingType sql.NullString
	err := q.QueryRow(`
		SELECT id, name, label_type FROM labels
		WHERE source_id = ? AND source_label_id = ?
	`, sourceID, sourceLabelID).Scan(&id, &existingName, &existingType)

	if err == nil {
		if existingName == name {
			if !existingType.Valid || existingType.String != labelType {
				if _, err = q.Exec(`
					UPDATE labels SET label_type = ?
					WHERE id = ?
				`, labelType, id); err != nil {
					return 0, fmt.Errorf("update label type: %w", err)
				}
			}
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
	if !errors.Is(err, sql.ErrNoRows) {
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
	q querier, sourceID int64, name string, keepID int64,
) error {
	var conflictID int64
	err := q.QueryRow(`
		SELECT id FROM labels
		WHERE source_id = ? AND name = ? AND id != ?
	`, sourceID, name, keepID).Scan(&conflictID)
	if errors.Is(err, sql.ErrNoRows) {
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
		// merge the other. Temp names embed the row PK (unique by
		// construction within this source_id) and a SOH (U+0001)
		// prefix that real Gmail label names cannot contain — Gmail's
		// UI rejects control characters, so the temp name cannot
		// collide with any real label name in the same source. The
		// SQLite-only X'00' hex literal that previously played this
		// role is not portable: PostgreSQL doesn't parse X'00' and
		// PG TEXT rejects embedded NUL bytes outright, so we build
		// the sentinel in Go and bind it as a parameter.
		for sourceLabelID, info := range labels {
			var id int64
			var curName string
			err := tx.QueryRow(`
				SELECT id, name FROM labels
				WHERE source_id = ? AND source_label_id = ?
			`, sourceID, sourceLabelID).Scan(&id, &curName)
			if errors.Is(err, sql.ErrNoRows) || curName == info.Name {
				continue
			}
			if err != nil {
				return fmt.Errorf(
					"check label %s: %w", sourceLabelID, err,
				)
			}
			tempName := fmt.Sprintf("\x01__msgvault_pending_rename__%d", id)
			if _, err = tx.Exec(`
				UPDATE labels SET name = ? WHERE id = ?
			`, tempName, id); err != nil {
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
	}, func(start, end int) ([]string, []any) {
		values := make([]string, end-start)
		args := make([]any, 0, (end-start)*2)
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
		}, func(start, end int) ([]string, []any) {
			values := make([]string, end-start)
			args := make([]any, 0, (end-start)*2)
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
	return execInChunks(s.db, labelIDs, []any{messageID},
		`DELETE FROM message_labels WHERE message_id = ? AND label_id IN (%s)`)
}

// SetReplyTo links a channel reply to its parent by resolving the parent's
// source_message_id to its internal messages.id within the same source.
func (s *Store) SetReplyTo(sourceID int64, childSourceMessageID, parentSourceMessageID string) error {
	_, err := s.db.Exec(s.dialect.Rebind(`
		UPDATE messages SET reply_to_message_id =
		  (SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?)
		WHERE source_id = ? AND source_message_id = ?`),
		sourceID, parentSourceMessageID, sourceID, childSourceMessageID)
	return err
}

// SetMessageEdited marks a message as edited at the source. UpsertMessage
// does not write is_edited, so importers that observe an edit flag call this
// after upserting.
func (s *Store) SetMessageEdited(messageID int64) error {
	_, err := s.db.Exec(`UPDATE messages SET is_edited = TRUE WHERE id = ?`, messageID)
	return err
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
	return execInChunks(s.db, sourceMessageIDs, []any{sourceID},
		fmt.Sprintf(`UPDATE messages SET deleted_from_source_at = %s WHERE source_id = ? AND source_message_id IN (%%s)`, s.dialect.Now()))
}

// MarkMessageDeletedByGmailID marks a message as deleted by its Gmail ID.
// This is used by the deletion executor which only has the Gmail message ID.
// When permanent is true, the message row is deleted entirely; otherwise it is
// soft-deleted by setting deleted_from_source_at.
//
// A2 (deferred): the match is NOT scoped by source_id, so a Gmail-ID collision
// across two accounts would delete/soft-delete the wrong account's row (blast
// radius: one row in the colliding account). This is deferred rather than fixed
// because the deletion Manifest carries only a flat []GmailIDs with no per-id
// source_id (internal/deletion/manifest.go), and a single manifest can legitimately
// span multiple accounts (see internal/tui/actions.go resolveGmailIDs /
// internal/mcp/handlers.go, where the account filter is optional), so a single
// Filters.Account cannot scope every id correctly. Properly scoping this needs a
// manifest schema/version change (out of scope). Gmail IDs are random enough that
// a cross-account collision is astronomically unlikely. See docs/internal/PG_STATUS.md.
func (s *Store) MarkMessageDeletedByGmailID(permanent bool, gmailID string) error {
	if permanent {
		// A2 (deferred): unscoped by source_id — see function doc.
		_, err := s.db.Exec(`DELETE FROM messages WHERE source_message_id = ?`, gmailID)
		return err
	}
	// A2 (deferred): unscoped by source_id — see function doc.
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
//
// A2 (deferred): the IN (...) match is NOT scoped by source_id — same unscoped
// collision caveat as MarkMessageDeletedByGmailID; see that function's doc and
// docs/internal/PG_STATUS.md for why it is deferred (manifest lacks per-id source_id and
// can span multiple accounts; collision astronomically unlikely).
func (s *Store) MarkMessagesDeletedByGmailIDBatch(gmailIDs []string) error {
	if len(gmailIDs) == 0 {
		return nil
	}

	const chunkSize = 500
	var firstErr error

	for i := 0; i < len(gmailIDs); i += chunkSize {
		end := min(i+chunkSize, len(gmailIDs))
		chunk := gmailIDs[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for j, id := range chunk {
			placeholders[j] = "?"
			args[j] = id
		}

		// A2 (deferred): unscoped by source_id — see function doc.
		query := fmt.Sprintf(
			`UPDATE messages SET deleted_from_source_at = %s WHERE source_message_id IN (%s)`,
			s.dialect.Now(), strings.Join(placeholders, ","))

		if _, err := s.db.Exec(query, args...); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// Fall back to individual updates for this chunk
			for _, id := range chunk {
				s.MarkMessageDeletedByGmailID(false, id) //nolint:errcheck,gosec // best-effort
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
	// Use explicitly seeded RNG for true randomness across process runs.
	// math/rand is fine — this picks rows for sampling, not authentication.
	rng := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec // sampling RNG, not security

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

	// runMaintenance disables the pool-wide 30s statement_timeout for the
	// clear: FTSClearSQL is a full-table tsvector rewrite that exceeds 30s on
	// a large archive (finding S1). No-op timeout reset on SQLite.
	if err := s.runMaintenance(context.Background(), func(ctx context.Context, tx *loggedTx) error {
		_, err := tx.ExecContext(ctx, s.dialect.FTSClearSQL())
		return err
	}); err != nil {
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
	// runMaintenance disables the pool-wide 30s statement_timeout for the
	// schema teardown/rebuild. On PG, FTSRebuildSchema runs a full-table
	// `UPDATE messages SET search_fts = NULL` (identical cost to the hatched
	// FTSClearSQL) plus a GIN rebuild over a populated table — both can exceed
	// 30s on a large archive and would cancel the rebuild-fts recovery command
	// with SQLSTATE 57014 (finding S1). On SQLite the reset SQL is "" so this
	// is an ordinary transaction around the DROP/CREATE of messages_fts.
	if err := s.runMaintenance(context.Background(), func(ctx context.Context, tx *loggedTx) error {
		return s.dialect.FTSRebuildSchema(tx)
	}); err != nil {
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
			// Only the specific PG tsvector-overflow error (a single
			// pathological row whose body exceeds PostgreSQL's tsvector
			// limit) is recoverable by retrying the batch row by row and
			// skipping the offending row(s). EVERY OTHER error (dead
			// connection, a non-size SQLSTATE, etc.) is systemic — it would
			// hit every row and silently clear-then-skip the whole archive —
			// so it must ABORT and propagate, not be masked as success.
			if !s.dialect.IsFTSValueTooLargeError(err) {
				return indexed, err
			}
			n, err = s.backfillFTSRowByRow(cursor, batchEnd)
			if err != nil {
				return indexed, err
			}
		}
		indexed += n
		cursor = batchEnd

		if progress != nil {
			pos := min(cursor-minID, idRange)
			progress(pos, idRange)
		}
	}
	return indexed, nil
}

// backfillFTSRowByRow re-runs the batch backfill one message id at a time over
// [fromID, toID), called only after a whole-batch failure that was classified
// as the recoverable PG tsvector-overflow error. A row is skipped (with a
// logged warning naming the id) ONLY when its per-row failure is itself the
// tsvector-overflow error; any OTHER per-row error aborts and is returned so a
// systemic failure cannot be swallowed. Returns the number of rows indexed.
//
// A skipped row is NOT left with search_fts NULL or an obsolete
// indexing_version: either state means "needs backfill", so leaving a
// permanently-unindexable row stale would make backfill re-run forever,
// re-hitting the same overflow each time. Instead the row is marked with a
// non-NULL empty tsvector at the current layout version; the row is correctly
// unsearchable (an empty vector matches nothing). This skip write is PG-only —
// the overflow error is PG-specific (IsFTSValueTooLargeError is always false on
// SQLite), so the PG-syntax empty-tsvector literal is safe.
func (s *Store) backfillFTSRowByRow(fromID, toID int64) (int64, error) {
	var indexed int64
	for id := fromID; id < toID; id++ {
		n, err := s.backfillFTSBatch(id, id+1)
		if err != nil {
			if !s.dialect.IsFTSValueTooLargeError(err) {
				return indexed, err
			}
			// Mark the overflow row terminal with a non-NULL empty tsvector so
			// FTSNeedsBackfill stops flagging it and backfill cannot loop on it
			// forever. Keep the warning so the skipped id is still logged.
			slog.Warn("skipping message in FTS backfill",
				slog.Int64("message_id", id),
				slog.Any("error", err))
			if _, uerr := s.db.Exec(
				`UPDATE messages SET search_fts = ''::tsvector, indexing_version = ? WHERE id = ?`,
				CurrentFTSIndexingVersion, id,
			); uerr != nil {
				return indexed, fmt.Errorf("mark FTS-overflow row %d terminal: %w", id, uerr)
			}
			continue
		}
		indexed += n
	}
	return indexed, nil
}

// backfillFTSBatchErrHook is a test-only seam: when non-nil it is consulted
// before each batch's UPDATE, and a non-nil return forces backfillFTSBatch to
// fail for the given id range. It lets tests exercise backfillFTSRowByRow's
// skip-and-continue fallback deterministically without depending on a body that
// happens to overflow PostgreSQL's tsvector limit after the LEFT cap. Nil (and
// thus a no-op) in production; only export_test.go ever sets it.
var backfillFTSBatchErrHook func(fromID, toID int64) error

// backfillFTSBatch inserts FTS rows for messages with id in [fromID, toID).
//
// Each batch runs under runMaintenance so the pool-wide 30s statement_timeout
// is disabled for the batch: a 5000-row tsvector rewrite can exceed 30s on a
// large archive (finding S1). Each batch remains its own committed transaction,
// preserving the existing "partial progress is preserved if interrupted"
// semantics. No-op timeout reset on SQLite.
func (s *Store) backfillFTSBatch(fromID, toID int64) (int64, error) {
	if backfillFTSBatchErrHook != nil {
		if err := backfillFTSBatchErrHook(fromID, toID); err != nil {
			return 0, err
		}
	}
	var affected int64
	err := s.runMaintenance(context.Background(), func(ctx context.Context, tx *loggedTx) error {
		result, err := tx.ExecContext(ctx, s.dialect.FTSBackfillBatchSQL(), fromID, toID)
		if err != nil {
			return err
		}
		affected, err = result.RowsAffected()
		return err
	})
	return affected, err
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

// ForEachTeamsHostedContentBody invokes fn with (messageID, bodyHTML) for every
// message of the given source whose HTML body contains a hostedContents URL, so
// callers can re-fetch inline media.
func (s *Store) ForEachTeamsHostedContentBody(sourceID int64, fn func(messageID int64, bodyHTML string) error) error {
	return s.forEachHostedContentBody(`
		SELECT mb.message_id, mb.body_html
		FROM message_bodies mb
		JOIN messages m ON m.id = mb.message_id
		WHERE m.source_id = ? AND mb.body_html LIKE '%hostedContents%'
	`, sourceID, fn)
}

// ForEachTeamsIncompleteHostedContentBody is like ForEachTeamsHostedContentBody
// but yields only messages whose number of distinct hostedContents references
// in body_html exceeds the count of inline image files already stored for them
// — i.e. messages whose inline media was not fully downloaded (transient fetch
// failures). Used to retry just the gaps instead of re-fetching everything.
func (s *Store) ForEachTeamsIncompleteHostedContentBody(sourceID int64, fn func(messageID int64, bodyHTML string) error) error {
	type bodyRow struct {
		id   int64
		body string
	}
	var buf []bodyRow

	rows, err := s.db.Query(`
		SELECT mb.message_id, mb.body_html,
		       (SELECT COUNT(*) FROM attachments a
		        WHERE a.message_id = mb.message_id
		          AND a.storage_path NOT LIKE 'http%' AND a.storage_path != ''
		          AND a.content_hash != '')
		FROM message_bodies mb
		JOIN messages m ON m.id = mb.message_id
		WHERE m.source_id = ? AND mb.body_html LIKE '%hostedContents%'
	`, sourceID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var messageID int64
		var bodyHTML sql.NullString
		var localAttachmentRows int
		if err := rows.Scan(&messageID, &bodyHTML, &localAttachmentRows); err != nil {
			_ = rows.Close()
			return err
		}
		if !bodyHTML.Valid || bodyHTML.String == "" {
			continue
		}
		if countDistinctHostedContentRefs(bodyHTML.String) > localAttachmentRows {
			buf = append(buf, bodyRow{id: messageID, body: bodyHTML.String})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, r := range buf {
		if err := fn(r.id, r.body); err != nil {
			return err
		}
	}
	return nil
}

var teamsHostedContentURLRe = regexp.MustCompile(`https?://[^"'\s)]+/hostedContents/[^"'\s)]+/\$value`)

func countDistinctHostedContentRefs(bodyHTML string) int {
	refs := teamsHostedContentURLRe.FindAllString(bodyHTML, -1)
	if len(refs) == 0 {
		return 0
	}
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		seen[ref] = struct{}{}
	}
	return len(seen)
}

// forEachHostedContentBody runs query (a single ? = sourceID, selecting
// message_id, body_html) and invokes fn per row. The matching rows are read
// fully and the read cursor is closed BEFORE any callback runs: callers
// typically write (e.g. UpsertAttachment) inside fn, and holding a streaming
// read cursor open across those writes pins a second pooled connection and
// contends for SQLite's single writer ("database is locked"). Returning an
// error from fn stops iteration and is returned.
func (s *Store) forEachHostedContentBody(query string, sourceID int64, fn func(messageID int64, bodyHTML string) error) error {
	type bodyRow struct {
		id   int64
		body string
	}
	var buf []bodyRow

	rows, err := s.db.Query(query, sourceID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var messageID int64
		var bodyHTML sql.NullString
		if err := rows.Scan(&messageID, &bodyHTML); err != nil {
			_ = rows.Close()
			return err
		}
		if !bodyHTML.Valid || bodyHTML.String == "" {
			continue
		}
		buf = append(buf, bodyRow{id: messageID, body: bodyHTML.String})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	// Release the read cursor (and its connection) before the write callbacks.
	if err := rows.Close(); err != nil {
		return err
	}

	for _, r := range buf {
		if err := fn(r.id, r.body); err != nil {
			return err
		}
	}
	return nil
}

// EnsureConversationWithType gets or creates a conversation with an
// explicit conversation_type. Unlike EnsureConversation (which hardcodes
// 'email_thread'), this accepts the type as a parameter, making it
// suitable for WhatsApp and other messaging platforms.
//
// Concurrent first-inserts converge via INSERT ... ON CONFLICT DO UPDATE
// RETURNING id. On conflict, conversation_type is overwritten with the
// caller's value and title is overwritten only when the caller supplies
// a non-empty title — preserves the prior behavior of not blanking out
// stored titles when re-syncs pass an empty value.
func (s *Store) EnsureConversationWithType(sourceID int64, sourceConversationID, conversationType, title string) (int64, error) {
	return ensureConversationWithType(s.db, s.dialect, sourceID, sourceConversationID, conversationType, title)
}

func ensureConversationWithType(q querier, dialect Dialect, sourceID int64, sourceConversationID, conversationType, title string) (int64, error) {
	now := dialect.Now()
	var id int64
	err := q.QueryRow(fmt.Sprintf(`
		INSERT INTO conversations (source_id, source_conversation_id, conversation_type, title, created_at, updated_at)
		VALUES (?, ?, ?, ?, %s, %s)
		ON CONFLICT (source_id, source_conversation_id) DO UPDATE
		SET conversation_type = EXCLUDED.conversation_type,
		    title = CASE WHEN EXCLUDED.title IS NOT NULL AND EXCLUDED.title != ''
		                 THEN EXCLUDED.title ELSE conversations.title END,
		    updated_at = %s
		RETURNING id
	`, now, now, now), sourceID, sourceConversationID, conversationType, title).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// EnsureParticipantByPhone gets or creates a participant by phone number.
// Phone must start with "+" (E.164 format). Returns an error for empty or
// invalid phone numbers to prevent database pollution.
// Also creates a participant_identifiers row with the given identifierType
// (e.g., "whatsapp", "imessage", "google_voice").
func (s *Store) EnsureParticipantByPhone(phone, displayName, identifierType string) (int64, error) {
	if phone == "" {
		return 0, errors.New("phone number is required")
	}
	if !strings.HasPrefix(phone, "+") {
		return 0, fmt.Errorf("phone number must be in E.164 format (starting with +), got %q", phone)
	}

	// Atomic upsert via ON CONFLICT — see EnsureParticipant for the
	// SELECT-then-INSERT race this collapses. The conflict target
	// mirrors the partial unique index on participants(phone_number)
	// WHERE phone_number IS NOT NULL exactly, which is required by
	// both PG and SQLite for partial-index ON CONFLICT to bind. The
	// DO UPDATE backfills display_name when the existing row has none,
	// preserving the prior best-effort behaviour without a second
	// round-trip.
	now := s.dialect.Now()
	var id int64
	err := s.db.QueryRow(fmt.Sprintf(`
		INSERT INTO participants (phone_number, display_name, created_at, updated_at)
		VALUES (?, ?, %s, %s)
		ON CONFLICT (phone_number) WHERE phone_number IS NOT NULL
			DO UPDATE SET display_name = CASE
				WHEN COALESCE(NULLIF(TRIM(participants.display_name), ''), '') = ''
				     AND EXCLUDED.display_name != ''
				THEN EXCLUDED.display_name
				ELSE participants.display_name
			END
		RETURNING id
	`, now, now), phone, displayName).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert participant by phone: %w", err)
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

// MergeParticipants repoints every reference from the old participant to the
// new one — messages, reactions, recipients, conversation membership, and
// identifiers — deduplicating where unique constraints would collide, then
// deletes the old participant row. Used when an importer discovers that two
// participant rows are the same person.
func (s *Store) MergeParticipants(oldID, newID int64) error {
	if oldID == newID || oldID == 0 || newID == 0 {
		return nil
	}
	return s.withTx(func(tx *loggedTx) error {
		// The merge must not lose contact metadata: fill gaps on the survivor
		// from the absorbed row, carrying the email's analytics domain with it.
		// Email and phone are UNIQUE, so the absorbed row must release each
		// value before the survivor can take it.
		var oldEmail, oldDomain, oldPhone sql.NullString
		if err := tx.QueryRow(`SELECT NULLIF(email_address, ''), NULLIF(domain, ''), NULLIF(phone_number, '') FROM participants WHERE id = ?`, oldID).
			Scan(&oldEmail, &oldDomain, &oldPhone); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE participants SET email_address = NULL, phone_number = NULL WHERE id = ?`, oldID); err != nil {
			return err
		}
		if _, err := tx.Exec(`
			UPDATE participants SET
				email_address = COALESCE(NULLIF(email_address, ''), ?),
				domain        = COALESCE(NULLIF(domain, ''), ?),
				phone_number  = COALESCE(NULLIF(phone_number, ''), ?)
			WHERE id = ?`, oldEmail, oldDomain, oldPhone, newID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE messages SET sender_id = ? WHERE sender_id = ?`, newID, oldID); err != nil {
			return err
		}
		// Drop old rows that would collide with an existing row of the new
		// participant, then repoint the remainder.
		if _, err := tx.Exec(`
			DELETE FROM reactions WHERE participant_id = ? AND EXISTS (
				SELECT 1 FROM reactions r2 WHERE r2.message_id = reactions.message_id
				  AND r2.participant_id = ? AND r2.reaction_type = reactions.reaction_type
				  AND r2.reaction_value = reactions.reaction_value)`, oldID, newID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE reactions SET participant_id = ? WHERE participant_id = ?`, newID, oldID); err != nil {
			return err
		}
		if _, err := tx.Exec(`
			DELETE FROM message_recipients WHERE participant_id = ? AND EXISTS (
				SELECT 1 FROM message_recipients m2 WHERE m2.message_id = message_recipients.message_id
				  AND m2.participant_id = ? AND m2.recipient_type = message_recipients.recipient_type)`, oldID, newID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE message_recipients SET participant_id = ? WHERE participant_id = ?`, newID, oldID); err != nil {
			return err
		}
		if _, err := tx.Exec(`
			DELETE FROM conversation_participants WHERE participant_id = ? AND EXISTS (
				SELECT 1 FROM conversation_participants c2 WHERE c2.conversation_id = conversation_participants.conversation_id
				  AND c2.participant_id = ?)`, oldID, newID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE conversation_participants SET participant_id = ? WHERE participant_id = ?`, newID, oldID); err != nil {
			return err
		}
		// Identifier values are globally unique, so a plain repoint suffices.
		if _, err := tx.Exec(`UPDATE participant_identifiers SET participant_id = ? WHERE participant_id = ?`, newID, oldID); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM participants WHERE id = ?`, oldID)
		return err
	})
}

// ParticipantByIdentifier returns the participant an identifier points at,
// with whether that participant carries a phone number (0 if none).
func (s *Store) ParticipantByIdentifier(identifierType, identifierValue string) (id int64, hasPhone bool, err error) {
	err = s.db.QueryRow(`
		SELECT p.id, COALESCE(p.phone_number, '') != ''
		FROM participant_identifiers pi
		JOIN participants p ON p.id = pi.participant_id
		WHERE pi.identifier_type = ? AND pi.identifier_value = ?
	`, identifierType, identifierValue).Scan(&id, &hasPhone)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, hasPhone, err
}

// SetParticipantIdentifier points identifier (type, value) at participantID,
// creating the row or re-pointing an existing one (idempotent). Importers use
// it to persist alternate identifiers on an already-resolved participant so
// later runs unify instead of forking a new participant.
func (s *Store) SetParticipantIdentifier(participantID int64, identifierType, identifierValue string) error {
	identifierType = strings.TrimSpace(identifierType)
	identifierValue = strings.TrimSpace(identifierValue)
	if identifierType == "" || identifierValue == "" {
		return errors.New("identifier type and value are required")
	}
	_, err := s.db.Exec(`
		INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value, is_primary)
		VALUES (?, ?, ?, FALSE)
		ON CONFLICT (identifier_type, identifier_value) DO UPDATE SET participant_id = excluded.participant_id
	`, participantID, identifierType, identifierValue)
	return err
}

func (s *Store) EnsureParticipantByIdentifier(identifierType, identifierValue, displayName string) (int64, error) {
	identifierType = strings.TrimSpace(identifierType)
	identifierValue = strings.TrimSpace(identifierValue)
	if identifierType == "" {
		return 0, errors.New("identifier type is required")
	}
	if identifierValue == "" {
		return 0, errors.New("identifier value is required")
	}

	var participantID int64
	err := s.db.QueryRow(`
		SELECT participant_id FROM participant_identifiers
		WHERE identifier_type = ? AND identifier_value = ?
	`, identifierType, identifierValue).Scan(&participantID)
	if err == nil {
		if displayName != "" {
			_, _ = s.db.Exec(`
				UPDATE participants SET display_name = ?
				WHERE id = ? AND (display_name IS NULL OR display_name = '')
			`, displayName, participantID)
		}
		return participantID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("lookup participant identifier: %w", err)
	}

	now := s.dialect.Now()
	err = s.db.QueryRow(fmt.Sprintf(`
		INSERT INTO participants (display_name, created_at, updated_at)
		VALUES (?, %s, %s)
		RETURNING id
	`, now, now), displayName).Scan(&participantID)
	if err != nil {
		return 0, fmt.Errorf("insert participant: %w", err)
	}
	_, err = s.db.Exec(`
		INSERT INTO participant_identifiers (
			participant_id, identifier_type, identifier_value, display_value, is_primary
		) VALUES (?, ?, ?, ?, TRUE)
	`, participantID, identifierType, identifierValue, identifierValue)
	if err != nil {
		return 0, fmt.Errorf("insert participant identifier: %w", err)
	}
	return participantID, nil
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

// UpdateImessageParticipantDisplayNameByPhone backfills display_name for
// an iMessage-imported participant, treating the legacy "display_name =
// phone_number" placeholder as empty. Earlier versions of import-imessage
// stored the raw phone string as display_name, which the regular
// UpdateParticipantDisplayNameByPhone update guard refuses to overwrite.
//
// Updates only when display_name is NULL/empty or equals phone_number,
// AND the participant has an "imessage" identifier — so contact-driven
// names from other sources (Gmail, WhatsApp, Google Voice) are preserved.
// Returns true if a participant was updated.
func (s *Store) UpdateImessageParticipantDisplayNameByPhone(phone, displayName string) (bool, error) {
	if phone == "" || displayName == "" {
		return false, nil
	}

	result, err := s.db.Exec(fmt.Sprintf(`
		UPDATE participants SET display_name = ?, updated_at = %s
		WHERE phone_number = ?
		  AND (display_name IS NULL OR display_name = '' OR display_name = phone_number)
		  AND EXISTS (
		      SELECT 1 FROM participant_identifiers pi
		      WHERE pi.participant_id = participants.id
		        AND pi.identifier_type = 'imessage'
		  )
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

// RetitleImessageChats refreshes generated titles on apple_messages
// conversations whose stored title is still based on raw phone/email
// handles but whose participants now have real display_names.
//
// Direct-chat titles are updated when the title exactly matches the
// other participant's phone or email. Group-chat titles are updated only
// when the title matches the importer-generated "Alice, Bob +N more"
// shape, preserving named group chats from Messages.app.
//
// Returns the number of conversations whose title was changed.
func (s *Store) RetitleImessageChats() (int64, error) {
	direct, err := s.retitleImessageDirectChats()
	if err != nil {
		return 0, err
	}
	groups, err := s.retitleImessageGroupChats()
	if err != nil {
		return 0, err
	}
	return direct + groups, nil
}

func (s *Store) retitleImessageDirectChats() (int64, error) {
	result, err := s.db.Exec(fmt.Sprintf(`
		UPDATE conversations
		SET title = (
		    SELECT p.display_name
		    FROM conversation_participants cp
		    JOIN participants p ON p.id = cp.participant_id
		    WHERE cp.conversation_id = conversations.id
		      AND p.display_name IS NOT NULL AND p.display_name != ''
		      AND p.display_name != COALESCE(p.phone_number, '')
		      AND LOWER(p.display_name) != LOWER(COALESCE(p.email_address, ''))
		      AND (
		          (p.phone_number IS NOT NULL AND p.phone_number != ''
		              AND conversations.title = p.phone_number)
		          OR (p.email_address IS NOT NULL AND p.email_address != ''
		              AND LOWER(conversations.title) = LOWER(p.email_address))
		      )
		    ORDER BY p.id
		    LIMIT 1
		),
		updated_at = %s
		WHERE conversation_type = 'direct_chat'
		  AND source_id IN (SELECT id FROM sources WHERE source_type = 'apple_messages')
		  AND EXISTS (
		      SELECT 1
		      FROM conversation_participants cp
		      JOIN participants p ON p.id = cp.participant_id
		      WHERE cp.conversation_id = conversations.id
		        AND p.display_name IS NOT NULL AND p.display_name != ''
		        AND p.display_name != COALESCE(p.phone_number, '')
		        AND LOWER(p.display_name) != LOWER(COALESCE(p.email_address, ''))
		        AND (
		            (p.phone_number IS NOT NULL AND p.phone_number != ''
		                AND conversations.title = p.phone_number)
		            OR (p.email_address IS NOT NULL AND p.email_address != ''
		                AND LOWER(conversations.title) = LOWER(p.email_address))
		        )
		  )
	`, s.dialect.Now()))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) retitleImessageGroupChats() (int64, error) {
	rows, err := s.db.Query(`
		SELECT id, title
		FROM conversations
		WHERE conversation_type = 'group_chat'
		  AND source_id IN (SELECT id FROM sources WHERE source_type = 'apple_messages')
		  AND title IS NOT NULL AND title != ''
	`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	type groupConversation struct {
		id    int64
		title string
	}
	var groups []groupConversation
	for rows.Next() {
		var group groupConversation
		if err := rows.Scan(&group.id, &group.title); err != nil {
			return 0, err
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var updated int64
	for _, group := range groups {
		participants, err := s.imessageTitleParticipants(group.id)
		if err != nil {
			return updated, err
		}
		if len(participants) == 0 {
			continue
		}

		newTitle := buildImessageGroupTitle(participants)
		if newTitle == "" || newTitle == group.title {
			continue
		}
		candidates := generatedImessageGroupTitleCandidates(participants)
		if _, ok := candidates[group.title]; !ok {
			continue
		}

		result, err := s.db.Exec(fmt.Sprintf(`
			UPDATE conversations SET title = ?, updated_at = %s
			WHERE id = ? AND title = ?
		`, s.dialect.Now()), newTitle, group.id, group.title)
		if err != nil {
			return updated, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return updated, err
		}
		updated += n
	}
	return updated, nil
}

type imessageTitleParticipant struct {
	displayName string
	phone       string
	email       string
}

func (s *Store) imessageTitleParticipants(conversationID int64) ([]imessageTitleParticipant, error) {
	rows, err := s.db.Query(`
		SELECT
			COALESCE(NULLIF(p.display_name, ''), ''),
			COALESCE(NULLIF(p.phone_number, ''), ''),
			COALESCE(NULLIF(p.email_address, ''), '')
		FROM conversation_participants cp
		JOIN participants p ON p.id = cp.participant_id
		WHERE cp.conversation_id = ?
		  AND COALESCE(p.email_address, '') != 'me@imessage.local'
		  AND COALESCE(p.display_name, '') != 'Me'
		ORDER BY p.id
	`, conversationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var participants []imessageTitleParticipant
	for rows.Next() {
		var p imessageTitleParticipant
		if err := rows.Scan(&p.displayName, &p.phone, &p.email); err != nil {
			return nil, err
		}
		participants = append(participants, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return participants, nil
}

func generatedImessageGroupTitleCandidates(participants []imessageTitleParticipant) map[string]struct{} {
	candidates := make(map[string]struct{})
	shown := min(3, len(participants))
	if shown == 0 {
		return candidates
	}

	var walk func(int, []string)
	walk = func(index int, labels []string) {
		if index == shown {
			candidates[formatImessageGroupTitle(labels, len(participants))] = struct{}{}
			return
		}

		rawLabel := participants[index].rawTitleLabel()
		walk(index+1, append(labels, rawLabel))

		displayLabel := participants[index].displayTitleLabel()
		if displayLabel != rawLabel {
			walk(index+1, append(labels, displayLabel))
		}
	}
	walk(0, nil)
	return candidates
}

func buildImessageGroupTitle(participants []imessageTitleParticipant) string {
	shown := min(3, len(participants))
	if shown == 0 {
		return ""
	}
	labels := make([]string, 0, shown)
	for _, p := range participants[:shown] {
		labels = append(labels, p.displayTitleLabel())
	}
	return formatImessageGroupTitle(labels, len(participants))
}

func formatImessageGroupTitle(labels []string, total int) string {
	if len(labels) == 0 {
		return ""
	}
	title := strings.Join(labels, ", ")
	if total > len(labels) {
		title += fmt.Sprintf(" +%d more", total-len(labels))
	}
	return title
}

func (p imessageTitleParticipant) displayTitleLabel() string {
	if p.hasRealDisplayName() {
		return p.displayName
	}
	return p.rawTitleLabel()
}

func (p imessageTitleParticipant) rawTitleLabel() string {
	if p.phone != "" {
		return p.phone
	}
	if p.email != "" {
		return p.email
	}
	if p.displayName != "" {
		return p.displayName
	}
	return "?"
}

func (p imessageTitleParticipant) hasRealDisplayName() bool {
	if p.displayName == "" {
		return false
	}
	if p.phone != "" && p.displayName == p.phone {
		return false
	}
	return p.email == "" || !strings.EqualFold(p.displayName, p.email)
}

// UpdateParticipantDisplayNameByEmail updates the display_name for an
// existing participant identified by email address. Only updates if
// display_name is currently empty. Returns true if a participant was
// found and updated, false if not found or name was already set. Does
// NOT create new participants. The lookup is case-insensitive.
func (s *Store) UpdateParticipantDisplayNameByEmail(email, displayName string) (bool, error) {
	if email == "" || displayName == "" {
		return false, nil
	}

	result, err := s.db.Exec(fmt.Sprintf(`
		UPDATE participants SET display_name = ?, updated_at = %s
		WHERE LOWER(email_address) = LOWER(?) AND (display_name IS NULL OR display_name = '')
	`, s.dialect.Now()), displayName, email)
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

// ConversationParticipantRef identifies one current member of a conversation.
type ConversationParticipantRef struct {
	ParticipantID int64
	Role          string
}

// ReplaceConversationParticipants atomically replaces a conversation's
// membership with a complete source snapshot.
func (s *Store) ReplaceConversationParticipants(conversationID int64, participants []ConversationParticipantRef) error {
	return s.withTx(func(tx *loggedTx) error {
		return replaceConversationParticipantsTx(tx, s.dialect, conversationID, participants)
	})
}

func replaceConversationParticipantsTx(tx *loggedTx, dialect Dialect, conversationID int64, participants []ConversationParticipantRef) error {
	if _, err := tx.Exec(`DELETE FROM conversation_participants WHERE conversation_id = ?`, conversationID); err != nil {
		return err
	}
	for _, participant := range participants {
		if participant.ParticipantID == 0 {
			continue
		}
		if _, err := tx.Exec(dialect.InsertOrIgnore(fmt.Sprintf(`INSERT OR IGNORE INTO conversation_participants (conversation_id, participant_id, role, joined_at)
			VALUES (?, ?, ?, %s)`, dialect.Now())), conversationID, participant.ParticipantID, participant.Role); err != nil {
			return err
		}
	}
	return nil
}

// UpsertReaction inserts or ignores a reaction.
func (s *Store) UpsertReaction(messageID, participantID int64, reactionType, reactionValue string, createdAt time.Time) error {
	_, err := s.db.Exec(s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO reactions (message_id, participant_id, reaction_type, reaction_value, created_at)
		VALUES (?, ?, ?, ?, ?)`), messageID, participantID, reactionType, reactionValue, createdAt)
	return err
}

type ReactionRef struct {
	ParticipantID int64
	Type          string
	Value         string
	CreatedAt     time.Time
}

// ReplaceReactions replaces all reactions for a message atomically.
func (s *Store) ReplaceReactions(messageID int64, reactions []ReactionRef) error {
	return s.withTx(func(tx *loggedTx) error {
		if _, err := tx.Exec(`DELETE FROM reactions WHERE message_id = ?`, messageID); err != nil {
			return err
		}
		for _, r := range reactions {
			if r.ParticipantID == 0 {
				continue
			}
			if _, err := tx.Exec(s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO reactions (message_id, participant_id, reaction_type, reaction_value, created_at)
				VALUES (?, ?, ?, ?, ?)`), messageID, r.ParticipantID, r.Type, r.Value, r.CreatedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

// UpsertMessageRawWithFormat stores compressed raw data with an explicit format.
// Unlike UpsertMessageRaw (which hardcodes 'mime'), this accepts the format as a parameter.
func (s *Store) UpsertMessageRawWithFormat(messageID int64, rawData []byte, format string) error {
	return upsertMessageRawWithFormat(s.db, messageID, rawData, format)
}

// AttachmentPathsUniqueToSource returns local content and thumbnail paths for
// blobs referenced by sourceID and by no other source. Sharing is checked
// across both hash columns: a content blob used as another source's thumbnail
// (or vice versa) is preserved. Call this before RemoveSource so the cascade
// has not run yet.
func (s *Store) AttachmentPathsUniqueToSource(sourceID int64) ([]string, error) {
	rows, err := s.db.Query(`
		WITH source_blob_paths(blob_hash, blob_path) AS (
		    SELECT a.content_hash, a.storage_path
		    FROM attachments a
		    JOIN messages m ON m.id = a.message_id
		    WHERE m.source_id = ?
		      AND a.content_hash IS NOT NULL AND a.content_hash != ''
		    UNION
		    SELECT a.thumbnail_hash, a.thumbnail_path
		    FROM attachments a
		    JOIN messages m ON m.id = a.message_id
		    WHERE m.source_id = ?
		      AND a.thumbnail_hash IS NOT NULL AND a.thumbnail_hash != ''
		)
		SELECT DISTINCT sb.blob_path
		FROM source_blob_paths sb
		WHERE sb.blob_path IS NOT NULL
		  AND sb.blob_path != ''
		  AND sb.blob_path NOT LIKE 'http://%'
		  AND sb.blob_path NOT LIKE 'https://%'
		  AND NOT EXISTS (
		      SELECT 1 FROM attachments a2
		      JOIN messages m2 ON m2.id = a2.message_id
		      WHERE m2.source_id != ?
		        AND (a2.content_hash = sb.blob_hash OR a2.thumbnail_hash = sb.blob_hash)
		  )
	`, sourceID, sourceID, sourceID)
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
// points to the given content or thumbnail path. Use this immediately before
// deleting a file to guard against a concurrent sync that added a new
// reference after the candidate list was collected.
func (s *Store) IsAttachmentPathReferenced(storagePath string) (bool, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM attachments WHERE storage_path = ? OR thumbnail_path = ?`,
		storagePath, storagePath,
	).Scan(&count)
	if err != nil {
		return true, err // fail safe: treat error as referenced
	}
	return count > 0, nil
}

// UpsertAttachment stores an attachment record. Idempotency for the
// common case is enforced by the partial unique index
// idx_attachments_msg_content_hash on (message_id, content_hash) where
// content_hash is non-empty; concurrent inserts collapse to one row via
// ON CONFLICT DO NOTHING. `size` is widened to int64 at the bind
// boundary so 32-bit builds cannot truncate large attachments before
// the column (BIGINT on PG, INTEGER on SQLite).
//
// When contentHash is empty (the rare untyped-blob path used by some
// importers), the unique index does not cover the row; a best-effort
// (message_id, empty-hash) match is used to avoid trivial duplicates,
// but two concurrent empty-hash inserts on the same message may both
// succeed.
func (s *Store) UpsertAttachment(messageID int64, filename, mimeType, storagePath, contentHash string, size int) error {
	if contentHash != "" {
		_, err := s.db.Exec(fmt.Sprintf(`
			INSERT INTO attachments (message_id, filename, mime_type, storage_path, content_hash, size, created_at)
			VALUES (?, ?, ?, ?, ?, ?, %s)
			ON CONFLICT (message_id, content_hash) WHERE content_hash IS NOT NULL AND content_hash != '' DO NOTHING
		`, s.dialect.Now()), messageID, filename, mimeType, storagePath, contentHash, int64(size))
		return err
	}

	var existingID int64
	err := s.db.QueryRow(`
		SELECT id FROM attachments WHERE message_id = ? AND (content_hash IS NULL OR content_hash = '')
	`, messageID).Scan(&existingID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.db.Exec(fmt.Sprintf(`
		INSERT INTO attachments (message_id, filename, mime_type, storage_path, content_hash, size, created_at)
		VALUES (?, ?, ?, ?, ?, ?, %s)
	`, s.dialect.Now()), messageID, filename, mimeType, storagePath, contentHash, int64(size))
	return err
}

// RecomputeMessageAttachmentStats refreshes the denormalized attachment flags
// on one message from its current attachment rows.
func (s *Store) RecomputeMessageAttachmentStats(messageID int64) error {
	_, err := s.db.Exec(`
		UPDATE messages
		SET has_attachments = (SELECT COUNT(*) FROM attachments WHERE message_id = ?) > 0,
		    attachment_count = (SELECT COUNT(*) FROM attachments WHERE message_id = ?)
		WHERE id = ?
	`, messageID, messageID, messageID)
	return err
}

type AttachmentRef struct {
	Filename           string
	MimeType           string
	StoragePath        string
	ContentHash        string
	Size               int
	SourceAttachmentID string
	// Optional media metadata; zero values are stored as NULL.
	MediaType  string
	Width      int64
	Height     int64
	DurationMS int64
}

// replaceMessageAttachmentsWhere atomically deletes a message's attachment
// rows matching deleteWhere and inserts refs. Refs with an empty StoragePath
// (and, when requireHash is set, an empty ContentHash) are skipped.
func (s *Store) replaceMessageAttachmentsWhere(messageID int64, deleteWhere string, requireHash bool, refs []AttachmentRef) error {
	return s.withTx(func(tx *loggedTx) error {
		if _, err := tx.Exec(`DELETE FROM attachments WHERE message_id = ? AND (`+deleteWhere+`)`, messageID); err != nil {
			return err
		}
		for _, ref := range refs {
			if ref.StoragePath == "" || (requireHash && ref.ContentHash == "") {
				continue
			}
			if _, err := tx.Exec(fmt.Sprintf(`
				INSERT INTO attachments (message_id, filename, mime_type, storage_path, content_hash, size, source_attachment_id,
					media_type, width, height, duration_ms, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, %s)
				ON CONFLICT (message_id, content_hash) WHERE content_hash IS NOT NULL AND content_hash != '' DO NOTHING
			`, s.dialect.Now()), messageID, ref.Filename, ref.MimeType, ref.StoragePath, ref.ContentHash, int64(ref.Size), ref.SourceAttachmentID,
				nullIfEmpty(ref.MediaType), nullIfZero(ref.Width), nullIfZero(ref.Height), nullIfZero(ref.DurationMS)); err != nil {
				return err
			}
		}
		return nil
	})
}

func nullIfEmpty(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func nullIfZero(n int64) sql.NullInt64 {
	return sql.NullInt64{Int64: n, Valid: n != 0}
}

// ReplaceMessageInlineAttachments replaces Teams-managed inline media rows for
// a message. It removes both rows marked by the current source_attachment_id
// scheme and legacy unmarked Teams inline rows produced before that marker was
// added, while leaving URL-backed reference/recording attachments untouched.
func (s *Store) ReplaceMessageInlineAttachments(messageID int64, refs []AttachmentRef) error {
	return s.replaceMessageAttachmentsWhere(messageID, `
		source_attachment_id LIKE 'teams:inline:%'
		OR (
		  (source_attachment_id IS NULL OR source_attachment_id = '')
		  AND storage_path != ''
		  AND storage_path NOT LIKE 'http://%'
		  AND storage_path NOT LIKE 'https://%'
		  AND content_hash IS NOT NULL
		  AND content_hash != ''
		  AND COALESCE(filename, '') = ''
		  AND COALESCE(mime_type, '') = ''
		)`, true, refs)
}

// ReplaceMessageBeeperAttachments replaces Beeper-managed attachment rows for
// a message (rows whose source_attachment_id carries the "beeper:" prefix).
// Rows with a content hash are downloaded media; rows without one are
// pending-download markers whose storage_path holds the source asset URL, so
// a later retry pass can find and repair them.
func (s *Store) ReplaceMessageBeeperAttachments(messageID int64, refs []AttachmentRef) error {
	return s.replaceMessageAttachmentsWhere(messageID, `source_attachment_id LIKE 'beeper:%'`, false, refs)
}

// MessageBeeperAttachments returns the message's existing Beeper-managed
// attachment rows keyed by source_attachment_id, so re-persisting a message
// can keep already-downloaded media without re-fetching it.
func (s *Store) MessageBeeperAttachments(messageID int64) (map[string]AttachmentRef, error) {
	rows, err := s.db.Query(`
		SELECT COALESCE(filename, ''), COALESCE(mime_type, ''), storage_path, COALESCE(content_hash, ''), size, source_attachment_id,
		       COALESCE(media_type, ''), COALESCE(width, 0), COALESCE(height, 0), COALESCE(duration_ms, 0)
		FROM attachments
		WHERE message_id = ? AND source_attachment_id LIKE 'beeper:%'
	`, messageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]AttachmentRef{}
	for rows.Next() {
		var ref AttachmentRef
		var size int64
		if err := rows.Scan(&ref.Filename, &ref.MimeType, &ref.StoragePath, &ref.ContentHash, &size, &ref.SourceAttachmentID,
			&ref.MediaType, &ref.Width, &ref.Height, &ref.DurationMS); err != nil {
			return nil, err
		}
		ref.Size = int(size)
		out[ref.SourceAttachmentID] = ref
	}
	return out, rows.Err()
}

// SourceMessageRef locates an archived message at its source: the source
// message ID, its conversation's source ID, and the archived timestamp.
type SourceMessageRef struct {
	SourceMessageID string
	ChatID          string // conversations.source_conversation_id
	SentAt          time.Time
}

// ListRecentMessagesForSource returns refs of the source's most recently
// archived, non-tombstoned messages, for verifying that stored IDs still
// resolve to the same content at the source.
func (s *Store) ListRecentMessagesForSource(sourceID int64, limit int) ([]SourceMessageRef, error) {
	rows, err := s.db.Query(`
		SELECT m.source_message_id, c.source_conversation_id, m.sent_at
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_id = ? AND m.sent_at IS NOT NULL AND m.deleted_from_source_at IS NULL
		ORDER BY m.sent_at DESC, m.id DESC
		LIMIT ?
	`, sourceID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SourceMessageRef
	for rows.Next() {
		var ref SourceMessageRef
		if err := rows.Scan(&ref.SourceMessageID, &ref.ChatID, &ref.SentAt); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// BeeperPendingAttachmentMessage identifies a message with at least one
// pending (not yet downloaded) Beeper attachment marker.
type BeeperPendingAttachmentMessage struct {
	MessageID       int64
	SourceMessageID string
	ChatID          string // conversations.source_conversation_id
}

// ListBeeperPendingAttachmentMessages returns the messages of a beeper
// source that still have pending attachment markers, so a retry pass can
// re-fetch their media. Buffered (not a cursor callback): callers do slow
// network and write work per item, which must not hold a read cursor open.
func (s *Store) ListBeeperPendingAttachmentMessages(sourceID int64) ([]BeeperPendingAttachmentMessage, error) {
	rows, err := s.db.Query(`
		SELECT m.id, m.source_message_id, c.source_conversation_id
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_id = ?
		  AND EXISTS (
		    SELECT 1 FROM attachments a
		    WHERE a.message_id = m.id
		      AND a.source_attachment_id LIKE 'beeper:%'
		      AND (a.content_hash IS NULL OR a.content_hash = '')
		  )
	`, sourceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var items []BeeperPendingAttachmentMessage
	for rows.Next() {
		var item BeeperPendingAttachmentMessage
		if err := rows.Scan(&item.MessageID, &item.SourceMessageID, &item.ChatID); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ReplaceMessageLinkAttachments replaces URL-backed attachment rows for a message.
// It intentionally leaves content-addressed local attachment paths (for example
// downloaded inline media) untouched.
func (s *Store) ReplaceMessageLinkAttachments(messageID int64, refs []AttachmentRef) error {
	return s.replaceMessageAttachmentsWhere(messageID,
		`storage_path LIKE 'http://%' OR storage_path LIKE 'https://%'`, false, refs)
}
