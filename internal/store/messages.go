package store

import (
	"bufio"
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
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

// querier is satisfied by both *sql.DB and *sql.Tx, allowing
// helpers to run inside or outside a transaction.
type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

type contextStatementQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type boundQuerier struct {
	ctx context.Context
	q   contextStatementQuerier
}

func (q boundQuerier) Exec(query string, args ...any) (sql.Result, error) {
	return q.q.ExecContext(q.ctx, query, args...)
}

func (q boundQuerier) QueryRow(query string, args ...any) *sql.Row {
	return q.q.QueryRowContext(q.ctx, query, args...)
}

type contextQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// RecipientSet groups participant IDs, display names, and envelope
// addresses for one recipient type (from, to, cc, bcc). EmailAddresses
// carries the address as it appeared in the message envelope; unlike the
// participant row it resolves to, it never changes under participant
// merges. Writers without envelope addresses may leave it empty.
type RecipientSet struct {
	Type           string
	ParticipantIDs []int64
	DisplayNames   []string
	EmailAddresses []string
}

// ParticipantPersistData describes one email participant to resolve inside a
// message persistence transaction.
type ParticipantPersistData struct {
	EmailAddress string
	DisplayName  string
	Domain       string
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
	// IdentityDerivedIsFromMe reports that IsFromMe came from a confirmed
	// account identity rather than a source-native sent-by-me signal.
	IdentityDerivedIsFromMe bool
	Subject                 sql.NullString
	Snippet                 sql.NullString
	SizeEstimate            int64
	HasAttachments          bool
	AttachmentCount         int
	DeletedAt               sql.NullTime
	ArchivedAt              time.Time
}

// MessageMetadataRecord is the archive identity and optional provider metadata
// for one message returned by MessageMetadataBatch.
type MessageMetadataRecord struct {
	ID       int64
	Metadata sql.NullString
}

// MessageWithRawMetadata identifies an archived message and the RFC822
// identity stored with its raw MIME.
type MessageWithRawMetadata struct {
	ID              int64
	RFC822MessageID sql.NullString
}

// UnresolvedMessageReply is a message whose provider metadata may contain a
// durable source reply reference but whose generic reply link is still NULL.
// Importers decode their own metadata shape and call SetReplyTo once the
// referenced message has arrived.
type UnresolvedMessageReply struct {
	MessageID       int64
	SourceMessageID string
	Metadata        string
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

// MessageSourceIDsInSnowflakeInterval returns canonical decimal source IDs in
// the exact numeric interval (lower, upper] for one source and conversation.
// Snowflakes are compared as decimal strings so values above signed int64 are
// ordered correctly on both SQLite and PostgreSQL.
func (s *Store) MessageSourceIDsInSnowflakeInterval(
	sourceID, conversationID int64, lower, upper string,
) ([]string, error) {
	canonicalLower, err := canonicalDecimal(lower)
	if err != nil {
		return nil, fmt.Errorf("invalid lower snowflake: %w", err)
	}
	canonicalUpper, err := canonicalDecimal(upper)
	if err != nil {
		return nil, fmt.Errorf("invalid upper snowflake: %w", err)
	}
	if compareCanonicalDecimals(canonicalLower, canonicalUpper) > 0 {
		return nil, errors.New("invalid snowflake interval: lower exceeds upper")
	}

	lowerLength := len(canonicalLower)
	upperLength := len(canonicalUpper)
	rows, err := s.db.Query(`
		SELECT source_message_id
		FROM messages
		WHERE source_id = ?
		  AND conversation_id = ?
		  AND (
		    LENGTH(source_message_id) > ?
		    OR (LENGTH(source_message_id) = ? AND source_message_id > ?)
		  )
		  AND (
		    LENGTH(source_message_id) < ?
		    OR (LENGTH(source_message_id) = ? AND source_message_id <= ?)
		  )
		ORDER BY LENGTH(source_message_id), source_message_id
	`, sourceID, conversationID,
		lowerLength, lowerLength, canonicalLower,
		upperLength, upperLength, canonicalUpper,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var sourceMessageIDs []string
	for rows.Next() {
		var sourceMessageID string
		if err := rows.Scan(&sourceMessageID); err != nil {
			return nil, err
		}
		canonical, err := canonicalDecimal(sourceMessageID)
		if err != nil || canonical != sourceMessageID {
			continue
		}
		sourceMessageIDs = append(sourceMessageIDs, sourceMessageID)
	}
	return sourceMessageIDs, rows.Err()
}

// MaxMessageSourceIDInSnowflakeInterval returns the numerically greatest
// canonical decimal source ID in (lower, upper] without materializing the
// interval. An empty interval returns an empty string.
func (s *Store) MaxMessageSourceIDInSnowflakeInterval(
	sourceID, conversationID int64, lower, upper string,
) (string, error) {
	page, err := s.MessageSourceIDsInSnowflakeIntervalPage(
		sourceID, conversationID, lower, upper, "", 1,
	)
	if err != nil || len(page) == 0 {
		return "", err
	}
	return page[0], nil
}

// MessageSourceIDsInSnowflakeIntervalPage returns one descending numeric
// keyset page from (lower, upper]. before is an optional exclusive cursor.
func (s *Store) MessageSourceIDsInSnowflakeIntervalPage(
	sourceID, conversationID int64, lower, upper, before string, limit int,
) ([]string, error) {
	canonicalLower, err := canonicalDecimal(lower)
	if err != nil {
		return nil, fmt.Errorf("invalid lower snowflake: %w", err)
	}
	canonicalUpper, err := canonicalDecimal(upper)
	if err != nil {
		return nil, fmt.Errorf("invalid upper snowflake: %w", err)
	}
	if compareCanonicalDecimals(canonicalLower, canonicalUpper) > 0 {
		return nil, errors.New("invalid snowflake interval: lower exceeds upper")
	}
	if limit <= 0 {
		return nil, errors.New("invalid snowflake page limit")
	}

	query := `
		SELECT source_message_id
		FROM messages
		WHERE source_id = ?
		  AND conversation_id = ?
		  AND (
		    LENGTH(source_message_id) > ?
		    OR (LENGTH(source_message_id) = ? AND source_message_id > ?)
		  )
		  AND (
		    LENGTH(source_message_id) < ?
		    OR (LENGTH(source_message_id) = ? AND source_message_id <= ?)
		  )`
	args := []any{
		sourceID, conversationID,
		len(canonicalLower), len(canonicalLower), canonicalLower,
		len(canonicalUpper), len(canonicalUpper), canonicalUpper,
	}
	if before != "" {
		canonicalBefore, beforeErr := canonicalDecimal(before)
		if beforeErr != nil {
			return nil, fmt.Errorf("invalid before snowflake: %w", beforeErr)
		}
		query += ` AND (
			LENGTH(source_message_id) < ?
			OR (LENGTH(source_message_id) = ? AND source_message_id < ?)
		)`
		args = append(args, len(canonicalBefore), len(canonicalBefore), canonicalBefore)
	}
	query += ` ORDER BY LENGTH(source_message_id) DESC, source_message_id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make([]string, 0, limit)
	for rows.Next() {
		var sourceMessageID string
		if err := rows.Scan(&sourceMessageID); err != nil {
			return nil, err
		}
		canonical, err := canonicalDecimal(sourceMessageID)
		if err == nil && canonical == sourceMessageID {
			result = append(result, sourceMessageID)
		}
	}
	return result, rows.Err()
}

func canonicalDecimal(value string) (string, error) {
	if value == "" {
		return "", errors.New("value is empty")
	}
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return "", errors.New("value must contain only decimal digits")
		}
	}
	canonical := strings.TrimLeft(value, "0")
	if canonical == "" {
		return "0", nil
	}
	return canonical, nil
}

func compareCanonicalDecimals(left, right string) int {
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return strings.Compare(left, right)
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

// ListUnresolvedMessageReplies returns provider messages that still need a
// reply-linking pass. A portable textual key check bounds the candidate set;
// the provider remains authoritative for JSON decoding and validation.
func (s *Store) ListUnresolvedMessageReplies(sourceID int64, messageType string) ([]UnresolvedMessageReply, error) {
	return s.listUnresolvedMessageReplies(sourceID, messageType, 0, 0)
}

// ListUnresolvedMessageRepliesAfter returns one bounded keyset page. Callers
// can resolve large provider archives without retaining every candidate's JSON
// metadata in memory at once.
func (s *Store) ListUnresolvedMessageRepliesAfter(
	sourceID int64, messageType string, afterID int64, limit int,
) ([]UnresolvedMessageReply, error) {
	if limit <= 0 {
		return nil, errors.New("list unresolved message replies: limit must be positive")
	}
	return s.listUnresolvedMessageReplies(sourceID, messageType, afterID, limit)
}

func (s *Store) listUnresolvedMessageReplies(
	sourceID int64, messageType string, afterID int64, limit int,
) ([]UnresolvedMessageReply, error) {
	limitClause := ""
	args := []any{sourceID, messageType, afterID}
	if limit > 0 {
		limitClause = " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.Query(`
		SELECT id, source_message_id, metadata
		FROM messages
		WHERE source_id = ?
		  AND message_type = ?
		  AND id > ?
		  AND reply_to_message_id IS NULL
		  AND metadata IS NOT NULL
		  AND CAST(metadata AS TEXT) LIKE '%"referenced_message_id"%'
		ORDER BY id`+limitClause,
		args...)
	if err != nil {
		return nil, fmt.Errorf("list unresolved message replies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var unresolved []UnresolvedMessageReply
	for rows.Next() {
		var reply UnresolvedMessageReply
		if err := rows.Scan(&reply.MessageID, &reply.SourceMessageID, &reply.Metadata); err != nil {
			return nil, fmt.Errorf("scan unresolved message reply: %w", err)
		}
		unresolved = append(unresolved, reply)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unresolved message replies: %w", err)
	}
	return unresolved, nil
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
	if s.syncGeneration != nil {
		return s.withTx(func(tx *loggedTx) error {
			if err := s.requireSyncMessageSourceTx(tx, messageID); err != nil {
				return err
			}
			return setMessageMetadataWith(tx, s.dialect, messageID, metadata)
		})
	}
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

// GetMessageSourceID returns the provider-specific identifier for a message.
func (s *Store) GetMessageSourceID(messageID int64) (string, error) {
	var sourceMessageID string
	err := s.db.QueryRow(
		`SELECT source_message_id FROM messages WHERE id = ?`,
		messageID,
	).Scan(&sourceMessageID)
	return sourceMessageID, err
}

// RekeyMessageSourceID changes a provider-specific identifier only while it
// still has the value observed by the caller.
func (s *Store) RekeyMessageSourceID(
	messageID int64,
	expectedSourceMessageID, newSourceMessageID string,
) (bool, error) {
	var rekeyed bool
	write := func(q querier) error {
		if err := s.requireSyncMessageSourceTx(q, messageID); err != nil {
			return err
		}
		result, err := q.Exec(
			`UPDATE messages SET source_message_id = ?
			 WHERE id = ? AND source_message_id = ?`,
			newSourceMessageID, messageID, expectedSourceMessageID)
		if err != nil {
			return fmt.Errorf("rekey message source ID: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read rekeyed row count: %w", err)
		}
		rekeyed = affected == 1
		return nil
	}
	if s.syncGeneration == nil {
		err := write(s.db)
		return rekeyed, err
	}
	err := s.withTx(func(tx *loggedTx) error { return write(tx) })
	return rekeyed, err
}

// UpdateMessageOnDedup updates an existing message's composite ID
// and labels when a cross-mailbox RFC822 dedup match is found.
// This ensures future syncs recognize the message under its new
// mailbox|uid key and don't re-download it.
func (s *Store) UpdateMessageOnDedup(
	messageID int64, newSourceMessageID string,
	labelIDs []int64,
) (bool, error) {
	return s.updateMessageOnDedup(
		messageID, newSourceMessageID, labelIDs, true)
}

// UpdateMessageOnPartialDedup updates an existing message's composite ID and
// merges newly observed labels. It is used when the new ID is authoritative
// but the scan did not observe every mailbox membership.
func (s *Store) UpdateMessageOnPartialDedup(
	messageID int64, newSourceMessageID string,
	labelIDs []int64,
) (bool, error) {
	return s.updateMessageOnDedup(
		messageID, newSourceMessageID, labelIDs, false)
}

func (s *Store) updateMessageOnDedup(
	messageID int64, newSourceMessageID string,
	labelIDs []int64, replaceLabels bool,
) (bool, error) {
	var changed bool
	err := s.withTx(func(tx *loggedTx) error {
		if err := s.requireSyncMessageSourceTx(tx, messageID); err != nil {
			return err
		}
		var currentSourceMessageID string
		if err := tx.QueryRow(
			`SELECT source_message_id FROM messages WHERE id = ?`,
			messageID,
		).Scan(&currentSourceMessageID); err != nil {
			return fmt.Errorf("get source_message_id: %w", err)
		}

		sourceIDChanged := currentSourceMessageID != newSourceMessageID
		if sourceIDChanged {
			if _, err := tx.Exec(
				`UPDATE messages SET source_message_id = ?
				 WHERE id = ?`,
				newSourceMessageID, messageID,
			); err != nil {
				return fmt.Errorf("update source_message_id: %w", err)
			}
		}

		labelsChanged, err := s.reconcileMessageLabelsTx(
			tx, messageID, labelIDs, replaceLabels)
		if err != nil {
			return err
		}
		changed = sourceIDChanged || labelsChanged
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// MigrateSourceMessageID rewrites a legacy source_message_id to a new value
// for one conversation. If the new ID already exists, dependents are repointed
// and the legacy row is removed so future imports converge on the new key.
func (s *Store) MigrateSourceMessageID(sourceID, conversationID int64, legacySourceMessageID, newSourceMessageID string) error {
	if legacySourceMessageID == "" || legacySourceMessageID == newSourceMessageID {
		return nil
	}
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
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

// MessageMetadataWithRawBatch returns identity metadata for messages that
// already have raw MIME stored.
func (s *Store) MessageMetadataWithRawBatch(
	sourceID int64,
	sourceMessageIDs []string,
) (map[string]MessageWithRawMetadata, error) {
	result := make(map[string]MessageWithRawMetadata)
	if len(sourceMessageIDs) == 0 {
		return result, nil
	}

	err := queryInChunks(
		s.db,
		sourceMessageIDs,
		[]any{sourceID},
		`SELECT m.source_message_id, m.id, m.rfc822_message_id
		 FROM messages m
		 JOIN message_raw mr ON mr.message_id = m.id
		 WHERE m.source_id = ? AND m.source_message_id IN (%s)`,
		func(rows *loggedRows) error {
			var sourceMessageID string
			var metadata MessageWithRawMetadata
			if err := rows.Scan(
				&sourceMessageID,
				&metadata.ID,
				&metadata.RFC822MessageID,
			); err != nil {
				return err
			}
			result[sourceMessageID] = metadata
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// EnsureConversation gets or creates a conversation (thread) for a message.
// Concurrent first-inserts converge via INSERT ... ON CONFLICT DO NOTHING
// RETURNING id followed by a lookup. The conflict path deliberately issues no
// UPDATE, not even a no-op SET: the SQLite activity conversation trigger fires
// on every conversations UPDATE, so a per-message no-op upsert would requeue
// the whole thread once per synced message — quadratic queue churn on a large
// thread.
func (s *Store) EnsureConversation(sourceID int64, sourceConversationID, title string) (int64, error) {
	now := s.dialect.Now()
	var id int64
	err := s.db.QueryRow(fmt.Sprintf(`
		INSERT INTO conversations (source_id, source_conversation_id, conversation_type, title, created_at, updated_at)
		VALUES (?, ?, 'email_thread', ?, %s, %s)
		ON CONFLICT (source_id, source_conversation_id) DO NOTHING
		RETURNING id
	`, now, now), sourceID, sourceConversationID, title).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if err := s.db.QueryRow(
		`SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?`,
		sourceID, sourceConversationID,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("ensure conversation %d/%q: %w",
			sourceID, sourceConversationID, err)
	}
	return id, nil
}

// upsertMessageSQL returns the message upsert SQL with dialect-specific timestamp.
// The attribution CTE runs before this transaction writes any 'from' envelope
// snapshot, so it must mirror the no-envelope fallback of
// messageIdentityAttributionMatch exactly; refreshMessageAttributionWith later
// settles rows whose envelope disagrees.
func upsertMessageSQL(now string) string {
	return fmt.Sprintf(`
	WITH attribution AS (
		SELECT
			CAST(? AS BOOLEAN) AS source_is_from_me,
			(
				CAST(? AS BOOLEAN)
				OR EXISTS (
					SELECT 1
					FROM account_identities ai
					JOIN participants p ON p.id = ?
					WHERE ai.source_id = ?
					  AND p.email_address IS NOT NULL
					  AND TRIM(p.email_address) <> ''
					  AND LOWER(p.email_address) = LOWER(ai.address)
				)
				OR EXISTS (
					SELECT 1
					FROM account_identities ai
					JOIN participant_identifiers pi ON pi.participant_id = ?
					WHERE ai.source_id = ?
					  AND (
						(pi.identifier_type = 'email'
						 AND NOT EXISTS (
							SELECT 1
							FROM participants p
							WHERE p.id = pi.participant_id
							  AND p.email_address IS NOT NULL
							  AND TRIM(p.email_address) <> ''
						 )
						 AND LOWER(pi.identifier_value) = LOWER(ai.address))
						OR (pi.identifier_type <> 'email'
							AND pi.identifier_value = ai.address)
					  )
				)
			) AS identity_is_from_me
	)
	INSERT INTO messages (
		conversation_id, source_id, source_message_id,
		rfc822_message_id, message_type,
		sent_at, received_at, internal_date, sender_id,
		is_from_me, source_is_from_me, identity_is_from_me,
		subject, snippet, size_estimate,
		has_attachments, attachment_count, archived_at
	)
	SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?,
	       (source_is_from_me OR identity_is_from_me),
	       source_is_from_me, identity_is_from_me,
	       ?, ?, ?, ?, ?, %s
	FROM attribution
	WHERE TRUE
	ON CONFLICT(source_id, source_message_id) DO UPDATE SET
		embed_gen = CASE
			WHEN COALESCE(messages.subject, '') <> COALESCE(excluded.subject, '')
				OR COALESCE(messages.message_type, '') <> COALESCE(excluded.message_type, '') THEN NULL
			ELSE messages.embed_gen
		END,
		message_type = excluded.message_type,
		conversation_id = excluded.conversation_id,
		rfc822_message_id = excluded.rfc822_message_id,
		sent_at = excluded.sent_at,
		received_at = excluded.received_at,
		internal_date = excluded.internal_date,
		sender_id = excluded.sender_id,
		is_from_me = excluded.is_from_me,
		source_is_from_me = excluded.source_is_from_me,
		identity_is_from_me = excluded.identity_is_from_me,
		subject = excluded.subject,
		snippet = excluded.snippet,
		size_estimate = excluded.size_estimate,
		has_attachments = excluded.has_attachments,
		attachment_count = excluded.attachment_count`, now)
}

// UpsertMessage inserts or updates a message.
func (s *Store) UpsertMessage(msg *Message) (int64, error) {
	if msg == nil {
		return 0, errors.New("upsert message requires a message")
	}
	if err := s.requireSyncSource(msg.SourceID); err != nil {
		return 0, err
	}
	if !isBodylessMessageJournalCandidate(msg) {
		if s.syncGeneration != nil {
			var id int64
			err := s.withTx(func(tx *loggedTx) error {
				var err error
				id, err = upsertMessageWith(tx, s.dialect, msg)
				return err
			})
			return id, err
		}
		return upsertMessageWith(s.db, s.dialect, msg)
	}
	var id int64
	err := s.withTx(func(tx *loggedTx) error {
		if s.dialect.DriverName() != postgresDriverName {
			// Acquire SQLite's writer slot before the prior-state read. This
			// prevents a deferred read transaction from failing to upgrade when
			// another writer commits between the read and the message upsert.
			// The no-op UPDATE dirties the clock page even when the journal is
			// disabled — an accepted per-persist cost so the transaction can
			// never fail its writer upgrade mid-persist.
			if _, err := tx.Exec(`UPDATE embedding_change_clock SET sequence = sequence WHERE singleton = 1`); err != nil {
				return fmt.Errorf("lock bodyless message journal: %w", err)
			}
		}
		var err error
		id, err = upsertMessageWith(tx, s.dialect, msg)
		return err
	})
	return id, err
}

func isBodylessMessageJournalCandidate(msg *Message) bool {
	return !msg.DeletedAt.Valid
}

type bodylessMessageJournalState struct {
	found          bool
	id             int64
	messageType    sql.NullString
	conversationID sql.NullInt64
	sentAt         sql.NullTime
	receivedAt     sql.NullTime
	internalDate   sql.NullTime
	senderID       sql.NullInt64
	subject        sql.NullString
	journaled      bool
	hasBody        bool
	deleted        bool
}

func upsertMessageWith(q querier, d Dialect, msg *Message) (int64, error) {
	journalCandidate := isBodylessMessageJournalCandidate(msg)
	var prior bodylessMessageJournalState
	if journalCandidate {
		err := q.QueryRow(`
			SELECT m.id, m.message_type, m.conversation_id, m.sent_at, m.received_at, m.internal_date,
			       m.sender_id, m.subject,
			       EXISTS (SELECT 1 FROM embedding_changes ec WHERE ec.message_id = m.id),
			       EXISTS (SELECT 1 FROM message_bodies mb WHERE mb.message_id = m.id),
			       (m.deleted_at IS NOT NULL OR m.deleted_from_source_at IS NOT NULL)
			FROM messages m WHERE m.source_id = ? AND m.source_message_id = ?`,
			msg.SourceID, msg.SourceMessageID).Scan(
			&prior.id, &prior.messageType, &prior.conversationID, &prior.sentAt, &prior.receivedAt,
			&prior.internalDate, &prior.senderID, &prior.subject, &prior.journaled, &prior.hasBody, &prior.deleted)
		switch {
		case err == nil:
			prior.found = true
		case errors.Is(err, sql.ErrNoRows):
		default:
			return 0, fmt.Errorf("read bodyless message journal state: %w", err)
		}
	}
	sql := upsertMessageSQL(d.Now())
	sourceIsFromMe := msg.IsFromMe && !msg.IdentityDerivedIsFromMe
	identityIsFromMe := msg.IsFromMe && msg.IdentityDerivedIsFromMe
	args := []any{
		sourceIsFromMe, identityIsFromMe,
		msg.SenderID, msg.SourceID,
		msg.SenderID, msg.SourceID,
		msg.ConversationID, msg.SourceID, msg.SourceMessageID,
		msg.RFC822MessageID, msg.MessageType,
		msg.SentAt, msg.ReceivedAt, msg.InternalDate, msg.SenderID,
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
	if journalCandidate && needsBodylessMessageJournal(prior, msg) {
		if err := appendBodylessMessageChange(q, d, id, prior, msg); err != nil {
			return 0, err
		}
		if err := coalesceLatestMessageChanges(q, d, id); err != nil {
			return 0, err
		}
	}
	// Fresh SQLite message inserts intentionally have no activity INSERT
	// trigger: even an inert trigger compiles a subprogram into every INSERT and
	// opens a statement journal. Enqueue through the production write path
	// instead. INSERT OR IGNORE makes this compatible with the UPDATE trigger
	// (which has already advanced an existing row) and with archives that still
	// carry the old INSERT trigger during migration.
	if err := enqueueActivityProjectionMessage(q, d, id); err != nil {
		return 0, err
	}
	if journalCandidate && !prior.found && d.DriverName() != postgresDriverName {
		if err := appendPersonSweepMessageInsert(q, d, id); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func needsBodylessMessageJournal(prior bodylessMessageJournalState, msg *Message) bool {
	newSubject := ""
	if msg.Subject.Valid {
		newSubject = strings.TrimSpace(msg.Subject.String)
	}
	if !prior.found {
		return true
	}
	if prior.hasBody || prior.deleted {
		return false
	}
	priorSubject := ""
	if prior.subject.Valid {
		priorSubject = strings.TrimSpace(prior.subject.String)
	}
	if newSubject == "" {
		return priorSubject != ""
	}
	return !prior.journaled ||
		prior.conversationID != nullInt64(msg.ConversationID) ||
		!nullTimeEqual(prior.sentAt, msg.SentAt) ||
		!nullTimeEqual(prior.receivedAt, msg.ReceivedAt) ||
		!nullTimeEqual(prior.internalDate, msg.InternalDate) ||
		prior.senderID != msg.SenderID || prior.subject != msg.Subject
}

func appendBodylessMessageChange(
	q querier, dialect Dialect, messageID int64, prior bodylessMessageJournalState, msg *Message,
) error {
	kind := EmbeddingChangeMessageInsert
	oldType := sql.NullString{}
	oldConversation := sql.NullInt64{}
	oldSentAt := sql.NullTime{}
	if prior.found {
		kind = EmbeddingChangeMessageUpdate
		oldType = prior.messageType
		oldConversation = prior.conversationID
		oldSentAt = canonicalMessageTime(prior.sentAt, prior.receivedAt, prior.internalDate)
	}
	newType := sql.NullString{String: msg.MessageType, Valid: msg.MessageType != ""}
	newConversation := nullInt64(msg.ConversationID)
	newSentAt := canonicalMessageTime(msg.SentAt, msg.ReceivedAt, msg.InternalDate)
	if dialect.DriverName() == postgresDriverName {
		if _, err := q.Exec(`
			SELECT pg_advisory_xact_lock_shared(hashtextextended('msgvault.embedding_change_clock', 0)),
			       append_embedding_change(?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
			string(kind), messageID, oldType, newType, oldConversation, newConversation, oldSentAt, newSentAt); err != nil {
			return fmt.Errorf("append PostgreSQL bodyless message journal: %w", err)
		}
		return nil
	}
	if _, err := q.Exec(`UPDATE embedding_change_clock SET sequence = sequence + 1 WHERE singleton = 1 AND enabled = TRUE`); err != nil {
		return fmt.Errorf("advance bodyless message journal: %w", err)
	}
	if _, err := q.Exec(`
		INSERT INTO embedding_changes (
			sequence, kind, message_id, old_message_type, new_message_type, old_conversation_id,
			new_conversation_id, old_sent_at, new_sent_at, participant_id
		)
		SELECT sequence, ?, ?, ?, ?, ?, ?, ?, ?, NULL
		FROM embedding_change_clock WHERE singleton = 1 AND enabled = TRUE`,
		string(kind), messageID, oldType, newType, oldConversation, newConversation, oldSentAt, newSentAt); err != nil {
		return fmt.Errorf("append SQLite bodyless message journal: %w", err)
	}
	return nil
}

func nullInt64(value int64) sql.NullInt64 {
	return sql.NullInt64{Int64: value, Valid: value != 0}
}

func canonicalMessageTime(values ...sql.NullTime) sql.NullTime {
	for _, value := range values {
		if value.Valid {
			return value
		}
	}
	return sql.NullTime{}
}

func nullTimeEqual(left, right sql.NullTime) bool {
	return left.Valid == right.Valid && (!left.Valid || left.Time.Equal(right.Time))
}

func enqueueActivityProjectionMessage(q querier, d Dialect, messageID int64) error {
	statement := d.InsertOrIgnore(`
		INSERT OR IGNORE INTO activity_projection_queue
			(message_id, revision, processed_revision)
		VALUES (?, 1, 0)`)
	if _, err := q.Exec(statement, messageID); err != nil {
		return fmt.Errorf("enqueue activity projection message %d: %w", messageID, err)
	}
	return nil
}

// UpsertMessageBody stores the body text and HTML for a message in the separate message_bodies table.
func (s *Store) UpsertMessageBody(messageID int64, bodyText, bodyHTML sql.NullString) error {
	if s.syncGeneration != nil {
		return s.withTx(func(tx *loggedTx) error {
			if err := s.requireSyncMessageSourceTx(tx, messageID); err != nil {
				return err
			}
			return upsertMessageBody(tx, s.dialect, s.fts5Available,
				messageID, bodyText, bodyHTML)
		})
	}
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
	if _, err = q.Exec(`UPDATE messages SET embed_gen = NULL WHERE id = ? AND embed_gen IS NOT NULL`, messageID); err != nil {
		return err
	}
	return coalesceLatestMessageChanges(q, dialect, messageID)
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

	return decodeMessageRaw(compressed, compression)
}

func decodeMessageRaw(compressed []byte, compression sql.NullString) ([]byte, error) {
	if !compression.Valid || compression.String != "zlib" {
		return compressed, nil
	}
	r, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("zlib reader: %w", err)
	}
	data, readErr := io.ReadAll(r)
	closeErr := r.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close zlib reader: %w", closeErr)
	}
	return data, nil
}

// GetMessageIsFromMe returns the baked account-attribution flag for a message.
func (s *Store) GetMessageIsFromMe(messageID int64) (bool, error) {
	var isFromMe bool
	err := s.db.QueryRow(`
		SELECT COALESCE(is_from_me, FALSE)
		FROM messages
		WHERE id = ?
	`, messageID).Scan(&isFromMe)
	return isFromMe, err
}

// messageIdentityAttributionMatch derives identity_is_from_me for one
// messages row. A non-empty 'from' envelope snapshot is authoritative: the
// sender's current participant aliases cannot reclassify that message after a
// participant merge. Legacy rows without an envelope use the sender's primary
// email and identifier rows with the same per-type case rules as identity
// matching. Envelope snapshots only contain email addresses, so non-email
// identities use the legacy fallback.
const messageIdentityAttributionMatch = `(
	EXISTS (
	  SELECT 1
	  FROM account_identities ai
	  JOIN message_recipients mr ON mr.message_id = messages.id
	  WHERE ai.source_id = messages.source_id
	    AND mr.recipient_type = 'from'
	    AND mr.email_address IS NOT NULL
	    AND TRIM(mr.email_address) <> ''
	    AND LOWER(mr.email_address) = LOWER(ai.address)
	)
	OR (
	  NOT EXISTS (
	    SELECT 1
	    FROM message_recipients mr
	    WHERE mr.message_id = messages.id
	      AND mr.recipient_type = 'from'
	      AND mr.email_address IS NOT NULL
	      AND TRIM(mr.email_address) <> ''
	  )
	  AND (
	    EXISTS (
	      SELECT 1
	      FROM account_identities ai
	      JOIN participants p ON p.id = messages.sender_id
	      WHERE ai.source_id = messages.source_id
	        AND p.email_address IS NOT NULL
	        AND TRIM(p.email_address) <> ''
	        AND LOWER(p.email_address) = LOWER(ai.address)
	    )
	    OR EXISTS (
	      SELECT 1
	      FROM account_identities ai
	      JOIN participant_identifiers pi ON pi.participant_id = messages.sender_id
	      WHERE ai.source_id = messages.source_id
	        AND (
	          (pi.identifier_type = 'email'
	           AND NOT EXISTS (
	             SELECT 1
	             FROM participants p
	             WHERE p.id = messages.sender_id
	               AND p.email_address IS NOT NULL
	               AND TRIM(p.email_address) <> ''
	           )
	           AND LOWER(pi.identifier_value) = LOWER(ai.address))
	          OR (pi.identifier_type <> 'email' AND pi.identifier_value = ai.address)
	        )
	    )
	  )
	)
)`

const messageSourceAttribution = `COALESCE(source_is_from_me, FALSE)`

func refreshSourceMessageAttributionContext(
	ctx context.Context,
	q contextQuerier,
	sourceID int64,
	excludeSourceMessageID string,
) error {
	// InitSchema assigns source provenance to every legacy row once. Runtime
	// identity changes therefore only need to update the derived and effective
	// values, and the change predicate avoids firing last_modified triggers for
	// messages whose attribution already agrees with the current identity set.
	_, err := q.ExecContext(ctx, fmt.Sprintf(`
		UPDATE messages
		SET identity_is_from_me = %[2]s,
		    is_from_me = (%[1]s OR %[2]s)
		WHERE source_id = ?
		  AND (? = '' OR source_message_id <> ?)
		  AND (
		    identity_is_from_me <> %[2]s
		    OR is_from_me IS NULL
		    OR is_from_me <> (%[1]s OR %[2]s)
		  )
	`, messageSourceAttribution, messageIdentityAttributionMatch),
		sourceID, excludeSourceMessageID, excludeSourceMessageID)
	if err != nil {
		return fmt.Errorf("refresh source message attribution: %w", err)
	}
	return nil
}

func refreshParticipantMessageAttributionContext(
	ctx context.Context,
	q contextQuerier,
	participantIDs ...int64,
) error {
	ids := make([]int64, 0, len(participantIDs))
	for _, participantID := range participantIDs {
		if participantID != 0 {
			ids = append(ids, participantID)
		}
	}
	if len(ids) == 0 {
		return nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, participantID := range ids {
		args[i] = participantID
	}
	_, err := q.ExecContext(ctx, fmt.Sprintf(`
		UPDATE messages
		SET identity_is_from_me = %[2]s,
		    is_from_me = (%[1]s OR %[2]s)
		WHERE sender_id IN (`+placeholders+`)
		  AND (
		    identity_is_from_me <> %[2]s
		    OR is_from_me IS NULL
		    OR is_from_me <> (%[1]s OR %[2]s)
		  )
	`, messageSourceAttribution, messageIdentityAttributionMatch), args...)
	if err != nil {
		return fmt.Errorf("refresh participant message attribution: %w", err)
	}
	return nil
}

// refreshMessageAttributionWith recomputes one message's identity attribution
// against its final recipient rows. persistMessageWith calls it after
// replacing recipients because the attribution CTE inside the message upsert
// runs before this transaction writes the 'from' envelope snapshot: a
// confirmed identity represented only there — the shape a participant merge
// leaves behind — would otherwise persist as unattributed, and re-persisting
// an already-repaired message would clear the flag its envelope had earned.
// The change guard keeps the common agreeing case write-free, so it fires no
// last_modified triggers.
func refreshMessageAttributionWith(q querier, messageID int64) error {
	_, err := q.Exec(fmt.Sprintf(`
		UPDATE messages
		SET identity_is_from_me = %[2]s,
		    is_from_me = (%[1]s OR %[2]s)
		WHERE id = ?
		  AND (
		    identity_is_from_me <> %[2]s
		    OR is_from_me IS NULL
		    OR is_from_me <> (%[1]s OR %[2]s)
		  )
	`, messageSourceAttribution, messageIdentityAttributionMatch), messageID)
	if err != nil {
		return fmt.Errorf("refresh message attribution: %w", err)
	}
	return nil
}

// PersistMessage atomically stores a message and its requested related
// snapshots in one transaction. Existing email callers persist body, raw MIME,
// recipients, and labels; non-email callers can additionally include
// conversation state, metadata, provider raw data, and FTS.
func (s *Store) PersistMessage(data *MessagePersistData) (int64, error) {
	return s.PersistMessageContext(context.Background(), data)
}

// PersistMessageContext is the request-aware form of PersistMessage. Every
// statement in the transaction observes ctx, and cancellation rolls the full
// message snapshot back.
func (s *Store) PersistMessageContext(ctx context.Context, data *MessagePersistData) (int64, error) {
	if data == nil || data.Message == nil {
		return 0, errors.New("persist message requires a message")
	}
	return s.persistMessageWithParticipantsContext(ctx, nil, func([]int64) *MessagePersistData {
		return data
	})
}

// PersistMessageWithParticipantsContext resolves participants and persists the
// message snapshot in one request-aware transaction. build receives IDs in the
// same order as participants and must return the snapshot that references them.
func (s *Store) PersistMessageWithParticipantsContext(
	ctx context.Context,
	participants []ParticipantPersistData,
	build func(participantIDs []int64) *MessagePersistData,
) (int64, error) {
	if build == nil {
		return 0, errors.New("persist message requires a participant builder")
	}
	return s.persistMessageWithParticipantsContext(ctx, participants, build)
}

func (s *Store) persistMessageWithParticipantsContext(
	ctx context.Context,
	participants []ParticipantPersistData,
	build func(participantIDs []int64) *MessagePersistData,
) (int64, error) {
	var messageID int64
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if s.dialect.DriverName() != postgresDriverName {
			// Reserve SQLite's writer slot before any prior-state or related
			// snapshot reads. Otherwise a concurrent commit can leave this
			// deferred WAL transaction unable to upgrade to a writer.
			if _, err := tx.Exec(`UPDATE embedding_change_clock SET sequence = sequence WHERE singleton = 1`); err != nil {
				return fmt.Errorf("lock message persistence: %w", err)
			}
		}
		if len(participants) > 1 {
			if err := s.lockParticipantDirectoryMutationTxContext(ctx, tx); err != nil {
				return err
			}
		}
		q := boundQuerier{ctx: ctx, q: tx}
		participantIDs := make([]int64, len(participants))
		participantInserted := false
		for idx, participant := range participants {
			if err := ctx.Err(); err != nil {
				return err
			}
			participantID, err := ensureParticipantWith(
				q,
				s.dialect,
				participant.EmailAddress,
				participant.DisplayName,
				participant.Domain,
				func() error {
					participantInserted = true
					return nil
				},
			)
			if err != nil {
				return fmt.Errorf("ensure participant %d: %w", idx, err)
			}
			participantIDs[idx] = participantID
		}
		if participantInserted {
			if err := s.bumpParticipantDisplayNameRevisionContext(ctx, tx); err != nil {
				return err
			}
		}

		if err := ctx.Err(); err != nil {
			return err
		}
		data := build(participantIDs)
		if data == nil || data.Message == nil {
			return errors.New("persist message requires a message")
		}
		if err := s.requireSyncSource(data.Message.SourceID); err != nil {
			return err
		}
		id, err := s.persistMessageWith(ctx, tx, data)
		if err != nil {
			return err
		}
		messageID = id
		return nil
	})
	return messageID, err
}

func (s *Store) persistMessageWith(
	ctx context.Context,
	tx *loggedTx,
	data *MessagePersistData,
) (int64, error) {
	if data == nil || data.Message == nil {
		return 0, errors.New("persist message requires a message")
	}
	q := boundQuerier{ctx: ctx, q: tx}
	message := data.Message
	if data.Conversation != nil {
		conversationID, err := ensureConversationWithType(
			q, s.dialect, data.Message.SourceID,
			data.Conversation.SourceConversationID,
			data.Conversation.ConversationType,
			data.Conversation.Title,
		)
		if err != nil {
			return 0, fmt.Errorf("ensure conversation: %w", err)
		}
		if err := replaceConversationParticipantsTx(
			ctx, tx, s.dialect, conversationID, data.Conversation.Participants,
		); err != nil {
			return 0, fmt.Errorf("replace conversation participants: %w", err)
		}
		messageCopy := *data.Message
		messageCopy.ConversationID = conversationID
		message = &messageCopy
	}

	messageID, err := upsertMessageWith(q, s.dialect, message)
	if err != nil {
		return 0, fmt.Errorf("upsert message: %w", err)
	}
	if data.Metadata != nil {
		if err := setMessageMetadataWith(q, s.dialect, messageID, *data.Metadata); err != nil {
			return 0, fmt.Errorf("set metadata: %w", err)
		}
	}

	if err := upsertMessageBody(
		q, s.dialect, s.fts5Available, messageID, data.BodyText, data.BodyHTML,
	); err != nil {
		return 0, fmt.Errorf("upsert body: %w", err)
	}

	if len(data.RawMIME) > 0 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		rawFormat := data.RawFormat
		if rawFormat == "" {
			rawFormat = "mime"
		}
		if err := upsertMessageRawWithFormat(q, messageID, data.RawMIME, rawFormat); err != nil {
			return 0, fmt.Errorf("upsert raw: %w", err)
		}
	}

	for _, rs := range data.Recipients {
		if err := replaceMessageRecipientsTx(q, messageID, rs); err != nil {
			return 0, fmt.Errorf("store %s recipients: %w", rs.Type, err)
		}
	}

	// Unconditional, not gated on data.Recipients carrying a 'from' set: even
	// a persist that touches no recipients re-ran the upsert's attribution
	// CTE, which cannot see the envelope rows already in the table, so the
	// recompute must settle attribution against them either way.
	if err := refreshMessageAttributionWith(q, messageID); err != nil {
		return 0, err
	}

	if !data.PreserveLabels {
		if err := replaceMessageLabelsTx(q, messageID, data.LabelIDs); err != nil {
			return 0, fmt.Errorf("store labels: %w", err)
		}
	}
	if data.FTS != nil && s.fts5Available {
		fts := *data.FTS
		fts.MessageID = messageID
		if err := s.dialect.FTSUpsert(q, fts); err != nil {
			return 0, fmt.Errorf("upsert fts: %w", err)
		}
	}
	return messageID, nil
}

// Participant represents a person in the participants table.
type Participant struct {
	ID           int64
	EmailAddress sql.NullString
	DisplayName  sql.NullString
	Domain       sql.NullString
}

// EnsureParticipant gets or creates a participant by email. Atomic via
// INSERT … ON CONFLICT … DO NOTHING followed by an in-transaction lookup,
// so two goroutines (or two processes against PostgreSQL) cannot race
// between a SELECT-empty and the follow-up INSERT and both succeed — one
// would otherwise lose to the unique constraint on (email_address) with a
// 23505 error. Display name and domain are left untouched on conflict to
// preserve any hand-edited values.
func (s *Store) EnsureParticipant(email, displayName, domain string) (int64, error) {
	return s.EnsureParticipantContext(context.Background(), email, displayName, domain)
}

// EnsureParticipantContext is the request-aware form of EnsureParticipant.
func (s *Store) EnsureParticipantContext(
	ctx context.Context,
	email,
	displayName,
	domain string,
) (int64, error) {
	var id int64
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		id, err = ensureParticipantWith(
			boundQuerier{ctx: ctx, q: tx},
			s.dialect,
			email,
			displayName,
			domain,
			func() error {
				return s.bumpParticipantDisplayNameRevisionContext(ctx, tx)
			},
		)
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

func ensureParticipantWith(
	q querier,
	dialect Dialect,
	email,
	displayName,
	domain string,
	onInsert func() error,
) (int64, error) {
	// ON CONFLICT must mirror the partial unique index on
	// participants(email_address) WHERE email_address IS NOT NULL — both
	// PG and SQLite require the WHERE clause on the conflict target to
	// match the partial index exactly. INSERT ... DO NOTHING lets us use
	// RowsAffected to distinguish an actual insert from an idempotent retry.
	for range 3 {
		result, err := q.Exec(fmt.Sprintf(`
			INSERT INTO participants (email_address, display_name, domain, created_at, updated_at)
			VALUES (?, ?, ?, %s, %s)
			ON CONFLICT (email_address) WHERE email_address IS NOT NULL
				DO NOTHING
		`, dialect.Now(), dialect.Now()), email, displayName, domain)
		if err != nil {
			return 0, err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("check participant insert: %w", err)
		}
		if inserted > 0 && onInsert != nil {
			if err := onInsert(); err != nil {
				return 0, err
			}
		}
		var id int64
		err = q.QueryRow(
			`SELECT id FROM participants WHERE email_address = ?`+dialect.SelectForUpdate(),
			email,
		).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		// PostgreSQL does not retain a row lock after ON CONFLICT DO NOTHING.
		// A concurrent participant merge can therefore delete the conflicting
		// row before the SELECT. Retry so the ensure recreates the row instead
		// of leaking that transient gap to callers.
	}
	return 0, fmt.Errorf("ensure participant %q after concurrent deletion", email)
}

// EnsureParticipantsBatch gets or creates participants in batch.
// Returns a map of email -> participant ID.
func (s *Store) EnsureParticipantsBatch(addresses []mime.Address) (map[string]int64, error) {
	if len(addresses) == 0 {
		return make(map[string]int64), nil
	}

	result := make(map[string]int64)
	unique := make(map[string]mime.Address, len(addresses))
	for _, addr := range addresses {
		if addr.Email == "" {
			continue
		}
		if _, exists := unique[addr.Email]; !exists {
			unique[addr.Email] = addr
		}
	}
	if len(unique) == 0 {
		return result, nil
	}
	emails := make([]string, 0, len(unique))
	for email := range unique {
		emails = append(emails, email)
	}
	sort.Strings(emails)

	err := s.withTx(func(tx *loggedTx) error {
		if err := s.lockParticipantDirectoryMutationTxContext(
			context.Background(), tx,
		); err != nil {
			return err
		}
		inserted := false
		for _, email := range emails {
			addr := unique[email]
			id, err := ensureParticipantWith(
				tx, s.dialect, addr.Email, addr.Name, addr.Domain,
				func() error {
					inserted = true
					return nil
				},
			)
			if err != nil {
				return err
			}
			result[email] = id
		}
		if inserted {
			return s.bumpParticipantDisplayNameRevision(tx)
		}
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
		if err := s.requireSyncMessageSourceTx(tx, messageID); err != nil {
			return err
		}
		if err := replaceMessageRecipientsTx(tx, messageID, RecipientSet{
			Type:           recipientType,
			ParticipantIDs: participantIDs,
			DisplayNames:   displayNames,
		}); err != nil {
			return err
		}
		if recipientType != "from" {
			return nil
		}
		// 'from' rows are attribution input: the message upsert's CTE could not
		// see the envelope rows this call just replaced, and importers on this
		// granular path never reach persistMessageWith's final recompute.
		return refreshMessageAttributionWith(tx, messageID)
	})
}

func replaceMessageRecipientsTx(tx querier, messageID int64, rs RecipientSet) error {
	_, err := tx.Exec(`
		DELETE FROM message_recipients WHERE message_id = ? AND recipient_type = ?
	`, messageID, rs.Type)
	if err != nil {
		return err
	}

	if len(rs.ParticipantIDs) == 0 {
		return nil
	}

	// Collapse duplicates within this set. The table holds at most one row
	// per (message_id, participant_id, recipient_type, normalized envelope
	// address) — idx_message_recipients_envelope — so an exact repeat in one
	// call (a calendar event listing the same attendee twice) is redundant
	// and would otherwise trip the unique index and abort the entire write,
	// while the same participant under two envelope aliases keeps one row
	// per alias. The first occurrence's display name wins per row.
	type recipientRowKey struct {
		participantID int64
		email         string
	}
	seen := make(map[recipientRowKey]struct{}, len(rs.ParticipantIDs))
	ids := make([]int64, 0, len(rs.ParticipantIDs))
	names := make([]string, 0, len(rs.ParticipantIDs))
	emails := make([]string, 0, len(rs.ParticipantIDs))
	for i, pid := range rs.ParticipantIDs {
		email := ""
		if i < len(rs.EmailAddresses) {
			email = rs.EmailAddresses[i]
		}
		key := recipientRowKey{participantID: pid, email: strings.ToLower(email)}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		ids = append(ids, pid)
		name := ""
		if i < len(rs.DisplayNames) {
			name = rs.DisplayNames[i]
		}
		names = append(names, name)
		emails = append(emails, email)
	}

	return insertInChunks(tx, chunkInsert{
		totalRows:    len(ids),
		valuesPerRow: 5,
		prefix:       "INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name, email_address) VALUES ",
	}, func(start, end int) ([]string, []any) {
		values := make([]string, end-start)
		args := make([]any, 0, (end-start)*5)
		for i := start; i < end; i++ {
			values[i-start] = "(?, ?, ?, ?, ?)"
			args = append(args, messageID, ids[i], rs.Type, names[i], nullIfEmpty(emails[i]))
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
	SystemRole    sql.NullString
}

// LabelSystemRoleSent identifies a label whose provider metadata confirms it
// represents sent mail. It is deliberately independent of the display name.
const LabelSystemRoleSent = "sent"

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
			tx, sourceID, sourceLabelID, name, labelType, nil,
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
	systemRole *string,
) (int64, error) {
	// Look up by canonical identifier (Gmail label ID).
	var id int64
	var existingName string
	var existingType sql.NullString
	var existingRole sql.NullString
	err := q.QueryRow(`
		SELECT id, name, label_type, system_role FROM labels
		WHERE source_id = ? AND source_label_id = ?
	`, sourceID, sourceLabelID).Scan(&id, &existingName, &existingType, &existingRole)

	if err == nil {
		if existingName == name {
			if !existingType.Valid || existingType.String != labelType ||
				(systemRole != nil && !labelSystemRoleMatches(existingRole, *systemRole)) {
				if systemRole != nil {
					if _, err = q.Exec(`
						UPDATE labels SET label_type = ?, system_role = ?
						WHERE id = ?
					`, labelType, labelSystemRoleValue(*systemRole), id); err != nil {
						return 0, fmt.Errorf("update label type and role: %w", err)
					}
					return id, nil
				}
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
		if systemRole != nil {
			if _, err = q.Exec(`
				UPDATE labels SET name = ?, label_type = ?, system_role = ?
				WHERE id = ?
			`, name, labelType, labelSystemRoleValue(*systemRole), id); err != nil {
				return 0, fmt.Errorf("update label name and role: %w", err)
			}
		} else if _, err = q.Exec(`
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
	if systemRole != nil {
		if _, err = q.Exec(`
			INSERT INTO labels (source_id, source_label_id, name, label_type, system_role)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(source_id, name) DO UPDATE SET
				source_label_id = excluded.source_label_id,
				label_type = excluded.label_type,
				system_role = excluded.system_role
		`, sourceID, sourceLabelID, name, labelType, labelSystemRoleValue(*systemRole)); err != nil {
			return 0, err
		}
	} else if _, err = q.Exec(`
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

func labelSystemRoleValue(systemRole string) any {
	if systemRole == "" {
		return nil
	}
	return systemRole
}

func labelSystemRoleMatches(existing sql.NullString, systemRole string) bool {
	if systemRole == "" {
		return !existing.Valid
	}
	return existing.Valid && existing.String == systemRole
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
	Name       string
	Type       string // "system" or "user"
	SystemRole string // trusted canonical role; empty clears any stale value
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
				tx, sourceID, sourceLabelID, info.Name, info.Type, &info.SystemRole,
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

// ReconcileMessageLabels replaces or merges labels and reports whether the
// persisted label set changed.
func (s *Store) ReconcileMessageLabels(
	messageID int64, labelIDs []int64, replace bool,
) (bool, error) {
	var changed bool
	err := s.withTx(func(tx *loggedTx) error {
		var err error
		changed, err = s.reconcileMessageLabelsTx(
			tx, messageID, labelIDs, replace)
		return err
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

func (s *Store) reconcileMessageLabelsTx(
	tx *loggedTx, messageID int64, labelIDs []int64, replace bool,
) (bool, error) {
	rows, err := tx.Query(`
		SELECT label_id FROM message_labels WHERE message_id = ?
	`, messageID)
	if err != nil {
		return false, err
	}

	existing := make(map[int64]struct{})
	for rows.Next() {
		var labelID int64
		if err := rows.Scan(&labelID); err != nil {
			_ = rows.Close()
			return false, err
		}
		existing[labelID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}

	desired := make(map[int64]struct{}, len(labelIDs))
	for _, labelID := range labelIDs {
		desired[labelID] = struct{}{}
	}

	if replace {
		changed := len(existing) != len(desired)
		if !changed {
			for labelID := range desired {
				if _, ok := existing[labelID]; !ok {
					changed = true
					break
				}
			}
		}
		if !changed {
			return false, nil
		}
		if err := replaceMessageLabelsTx(tx, messageID, labelIDs); err != nil {
			return false, err
		}
		return true, nil
	}

	missing := make([]int64, 0, len(desired))
	for labelID := range desired {
		if _, ok := existing[labelID]; !ok {
			missing = append(missing, labelID)
		}
	}
	if len(missing) == 0 {
		return false, nil
	}
	if err := s.addMessageLabelsTx(tx, messageID, missing); err != nil {
		return false, err
	}
	return true, nil
}

func replaceMessageLabelsTx(tx querier, messageID int64, labelIDs []int64) error {
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
		return s.addMessageLabelsTx(tx, messageID, labelIDs)
	})
}

func (s *Store) addMessageLabelsTx(
	tx *loggedTx, messageID int64, labelIDs []int64,
) error {
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
	if s.syncGeneration != nil {
		return s.withTx(func(tx *loggedTx) error {
			if err := s.requireSyncMessageSourceTx(tx, messageID); err != nil {
				return err
			}
			return execInChunks(tx, labelIDs, []any{messageID},
				`DELETE FROM message_labels WHERE message_id = ? AND label_id IN (%s)`)
		})
	}
	return execInChunks(s.db, labelIDs, []any{messageID},
		`DELETE FROM message_labels WHERE message_id = ? AND label_id IN (%s)`)
}

// SetReplyTo links a channel reply to its parent by resolving the parent's
// source_message_id to its internal messages.id within the same source.
func (s *Store) SetReplyTo(sourceID int64, childSourceMessageID, parentSourceMessageID string) error {
	return s.withSyncSourceWriteContext(context.Background(), sourceID, func(q querier) error {
		_, err := q.Exec(`
			UPDATE messages SET reply_to_message_id =
			  (SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?)
			WHERE source_id = ? AND source_message_id = ?`,
			sourceID, parentSourceMessageID, sourceID, childSourceMessageID)
		return err
	})
}

// SetMessageEdited marks a message as edited at the source. UpsertMessage
// does not write is_edited, so importers that observe an edit flag call this
// after upserting.
func (s *Store) SetMessageEdited(messageID int64) error {
	return s.withSyncMessageWriteContext(context.Background(), messageID, func(q querier) error {
		_, err := q.Exec(`UPDATE messages SET is_edited = TRUE WHERE id = ?`, messageID)
		return err
	})
}

// SetMessageReplyContext links a message to the message it replies to while
// preserving the scoped sync-generation fence of the writer.
func (s *Store) SetMessageReplyContext(ctx context.Context, messageID, replyToMessageID int64) error {
	return s.withSyncMessageWriteContext(ctx, messageID, func(q querier) error {
		_, err := q.Exec(`UPDATE messages SET reply_to_message_id = ? WHERE id = ?`,
			replyToMessageID, messageID)
		return err
	})
}

// MarkMessageDeleted marks a message as deleted from the source.
func (s *Store) MarkMessageDeleted(sourceID int64, sourceMessageID string) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	write := func(q chunkQuerier) error {
		_, err := q.Exec(fmt.Sprintf(`
			UPDATE messages
			SET deleted_from_source_at = %s
			WHERE source_id = ? AND source_message_id = ? AND deleted_from_source_at IS NULL
		`, s.dialect.Now()), sourceID, sourceMessageID)
		return err
	}
	if s.syncGeneration == nil {
		return write(s.db)
	}
	return s.withTx(func(tx *loggedTx) error { return write(tx) })
}

// ClearMessageDeletedFromSource clears the upstream tombstone when a message
// reappears during a provider repair scan.
func (s *Store) ClearMessageDeletedFromSource(sourceID int64, sourceMessageID string) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	write := func(q querier) error {
		_, err := q.Exec(`
			UPDATE messages
			SET deleted_from_source_at = NULL
			WHERE source_id = ? AND source_message_id = ?
		`, sourceID, sourceMessageID)
		return err
	}
	if s.syncGeneration == nil {
		return write(s.db)
	}
	return s.withTx(func(tx *loggedTx) error { return write(tx) })
}

// MarkMessagesDeletedBatch marks multiple messages as deleted from the source in a single transaction.
func (s *Store) MarkMessagesDeletedBatch(sourceID int64, sourceMessageIDs []string) error {
	if len(sourceMessageIDs) == 0 {
		return nil
	}
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	write := func(q chunkQuerier) error {
		return execInChunks(q, sourceMessageIDs, []any{sourceID},
			fmt.Sprintf(`UPDATE messages SET deleted_from_source_at = %s WHERE source_id = ? AND source_message_id IN (%%s) AND deleted_from_source_at IS NULL`, s.dialect.Now()))
	}
	if s.syncGeneration == nil {
		return write(s.db)
	}
	return s.withTx(func(tx *loggedTx) error { return write(tx) })
}

// ReconcileSourceMessageSnapshot tombstones locally live messages that are
// absent from one complete provider snapshot. The sync-scoped Store fences the
// current generation before reading or writing, and the transaction ensures an
// incomplete reconciliation cannot publish partial tombstones.
func (s *Store) ReconcileSourceMessageSnapshot(
	ctx context.Context, sourceID int64, present map[string]struct{},
) (int64, error) {
	if present == nil {
		return 0, errors.New("reconcile source message snapshot: nil snapshot")
	}
	if err := s.requireSyncSource(sourceID); err != nil {
		return 0, err
	}

	var missing []string
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT source_message_id
			FROM messages
			WHERE source_id = ?
			  AND deleted_at IS NULL
			  AND deleted_from_source_at IS NULL
		`, sourceID)
		if err != nil {
			return fmt.Errorf("reconcile source message snapshot: list live messages: %w", err)
		}
		for rows.Next() {
			var sourceMessageID string
			if err := rows.Scan(&sourceMessageID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("reconcile source message snapshot: scan live message: %w", err)
			}
			if _, ok := present[sourceMessageID]; !ok {
				missing = append(missing, sourceMessageID)
			}
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("reconcile source message snapshot: close live messages: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("reconcile source message snapshot: list live messages: %w", err)
		}
		if len(missing) == 0 {
			return nil
		}
		if err := execInChunksContext(ctx, tx, missing, []any{sourceID},
			fmt.Sprintf(`UPDATE messages SET deleted_from_source_at = %s WHERE source_id = ? AND source_message_id IN (%%s) AND deleted_from_source_at IS NULL`, s.dialect.Now())); err != nil {
			return fmt.Errorf("reconcile source message snapshot: mark missing messages: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return int64(len(missing)), nil
}

// MarkMessagesDeletedFromReader consumes newline-delimited source message IDs
// in bounded batches and commits all tombstones atomically. Any late reader or
// update failure rolls back earlier batches.
func (s *Store) MarkMessagesDeletedFromReader(sourceID int64, reader io.Reader, batchSize int) error {
	if reader == nil {
		return errors.New("mark messages deleted from reader: nil reader")
	}
	if batchSize <= 0 {
		return errors.New("mark messages deleted from reader: batch size must be positive")
	}
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	return s.withTx(func(tx *loggedTx) error {
		batch := make([]string, 0, batchSize)
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			if err := execInChunks(tx, batch, []any{sourceID},
				fmt.Sprintf(`UPDATE messages SET deleted_from_source_at = %s WHERE source_id = ? AND source_message_id IN (%%s) AND deleted_from_source_at IS NULL`, s.dialect.Now())); err != nil {
				return err
			}
			batch = batch[:0]
			return nil
		}

		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			messageID := scanner.Text()
			if messageID == "" {
				return errors.New("mark messages deleted from reader: empty source message ID")
			}
			batch = append(batch, messageID)
			if len(batch) == batchSize {
				if err := flush(); err != nil {
					return fmt.Errorf("mark messages deleted from reader: update: %w", err)
				}
			}
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("mark messages deleted from reader: %w", err)
		}
		if err := flush(); err != nil {
			return fmt.Errorf("mark messages deleted from reader: update: %w", err)
		}
		return nil
	})
}

// MarkMessageDeletedByGmailID marks a message as deleted by its Gmail ID.
// This is used by the deletion executor which only has the Gmail message ID.
// Both remote trash and permanent deletion are recorded locally by setting
// deleted_from_source_at; the archived message row is never removed.
//
// This compatibility entry point is intentionally unscoped. New deletion
// execution resolves a manifest source and uses
// MarkMessageDeletedBySourceMessageID instead.
func (s *Store) MarkMessageDeletedByGmailID(permanent bool, gmailID string) error {
	return s.MarkMessageDeletedBySourceMessageID(0, permanent, gmailID)
}

// MarkMessageDeletedBySourceMessageID marks only the message belonging to
// sourceID. A zero sourceID retains the legacy unscoped behavior for version-1
// deletion manifests.
func (s *Store) MarkMessageDeletedBySourceMessageID(sourceID int64, _ bool, gmailID string) error {
	sourceClause := ""
	args := []any{gmailID}
	if sourceID > 0 {
		sourceClause = " AND source_id = ?"
		args = append(args, sourceID)
	}
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE messages
		SET deleted_from_source_at = %s
		WHERE source_message_id = ?%s AND deleted_from_source_at IS NULL
	`, s.dialect.Now(), sourceClause), args...)
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
// This compatibility entry point is intentionally unscoped. New deletion
// execution uses MarkMessagesDeletedBySourceMessageIDBatch.
func (s *Store) MarkMessagesDeletedByGmailIDBatch(gmailIDs []string) error {
	return s.MarkMessagesDeletedBySourceMessageIDBatch(0, gmailIDs)
}

// MarkMessagesDeletedBySourceMessageIDBatch scopes batch archive marking to
// sourceID. A zero sourceID preserves legacy version-1 behavior.
func (s *Store) MarkMessagesDeletedBySourceMessageIDBatch(sourceID int64, gmailIDs []string) error {
	if len(gmailIDs) == 0 {
		return nil
	}

	const chunkSize = 500
	var firstErr error

	for i := 0; i < len(gmailIDs); i += chunkSize {
		end := min(i+chunkSize, len(gmailIDs))
		chunk := gmailIDs[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		for j, id := range chunk {
			placeholders[j] = "?"
			args = append(args, id)
		}
		sourceClause := ""
		if sourceID > 0 {
			sourceClause = " AND source_id = ?"
			args = append(args, sourceID)
		}

		query := fmt.Sprintf(
			`UPDATE messages SET deleted_from_source_at = %s WHERE source_message_id IN (%s)%s AND deleted_from_source_at IS NULL`,
			s.dialect.Now(), strings.Join(placeholders, ","), sourceClause)

		if _, err := s.db.Exec(query, args...); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// Fall back to individual updates for this chunk
			for _, id := range chunk {
				s.MarkMessageDeletedBySourceMessageID(sourceID, false, id) //nolint:errcheck,gosec // best-effort
			}
		}
	}

	return firstErr
}

// CountMessagesForSource returns the count of messages for a specific source (account).
func (s *Store) CountMessagesForSource(sourceID int64) (int64, error) {
	return s.CountMessagesForSourceContext(context.Background(), sourceID)
}

// CountMessagesForSourceContext is the request-aware form of
// CountMessagesForSource.
func (s *Store) CountMessagesForSourceContext(ctx context.Context, sourceID int64) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, fmt.Sprintf(`
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

// CountMessagesPerMailbox returns a map of mailbox name to message count
// for a given source. It counts live messages grouped by their label name
// using the message_labels/labels join. Only messages with labels appear
// in the result (messages without any folder/label mapping are excluded).
func (s *Store) CountMessagesPerMailbox(sourceID int64) (map[string]int64, error) {
	result := make(map[string]int64)
	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT l.name, COUNT(DISTINCT m.id)
		  FROM messages m
		  JOIN message_labels ml ON ml.message_id = m.id
		  JOIN labels l ON l.id = ml.label_id
		 WHERE m.source_id = ? AND l.source_id = ? AND %s
		 GROUP BY l.name
	`, LiveMessagesWhere("", true)), sourceID, sourceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		var count int64
		if err := rows.Scan(&name, &count); err != nil {
			return nil, err
		}
		result[name] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
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
	doc := FTSDoc{
		MessageID: messageID,
		Subject:   subject,
		Body:      bodyText,
		FromAddr:  fromAddr,
		ToAddrs:   toAddrs,
		CcAddrs:   ccAddrs,
	}
	if s.syncGeneration != nil {
		return s.withTx(func(tx *loggedTx) error {
			if err := s.requireSyncMessageSourceTx(tx, messageID); err != nil {
				return err
			}
			return s.dialect.FTSUpsert(tx, doc)
		})
	}
	return s.dialect.FTSUpsert(s.db, doc)
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
	return s.BackfillFTSContext(context.Background(), progress)
}

// BackfillFTSContext is the request-aware form of BackfillFTS.
func (s *Store) BackfillFTSContext(
	ctx context.Context,
	progress func(done, total int64),
) (int64, error) {
	if !s.fts5Available {
		return 0, nil
	}

	minID, maxID, err := s.messageIDRangeContext(ctx)
	if err != nil {
		return 0, err
	}
	if maxID == 0 {
		return 0, nil
	}

	// runMaintenance disables the pool-wide 30s statement_timeout for the
	// clear: FTSClearSQL is a full-table tsvector rewrite that exceeds 30s on
	// a large archive (finding S1). No-op timeout reset on SQLite.
	if err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		_, err := tx.ExecContext(ctx, s.dialect.FTSClearSQL())
		return err
	}); err != nil {
		return 0, fmt.Errorf("clear FTS: %w", err)
	}

	return s.backfillFTSRangeContext(ctx, minID, maxID, progress)
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
	return s.RebuildFTSContext(context.Background(), progress)
}

// RebuildFTSContext is the request-aware form of RebuildFTS.
func (s *Store) RebuildFTSContext(
	ctx context.Context,
	progress func(done, total int64),
) (int64, error) {
	// runMaintenance disables the pool-wide 30s statement_timeout for the
	// schema teardown/rebuild. On PG, FTSRebuildSchema runs a full-table
	// `UPDATE messages SET search_fts = NULL` (identical cost to the hatched
	// FTSClearSQL) plus a GIN rebuild over a populated table — both can exceed
	// 30s on a large archive and would cancel the rebuild-fts recovery command
	// with SQLSTATE 57014 (finding S1). On SQLite the reset SQL is "" so this
	// is an ordinary transaction around the DROP/CREATE of messages_fts.
	if err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		return s.dialect.FTSRebuildSchema(ctx, tx)
	}); err != nil {
		return 0, err
	}

	minID, maxID, err := s.messageIDRangeContext(ctx)
	if err != nil {
		return 0, err
	}
	if maxID == 0 {
		s.fts5Available = true
		return 0, nil
	}

	indexed, err := s.backfillFTSRangeContext(ctx, minID, maxID, progress)
	if err != nil {
		return indexed, err
	}
	s.fts5Available = true
	return indexed, nil
}

// messageIDRangeContext returns (minID, maxID) using MIN/MAX B-tree lookups
// rather than COUNT(*), which would scan the whole table.
func (s *Store) messageIDRangeContext(ctx context.Context) (int64, int64, error) {
	var minID, maxID int64
	err := s.db.QueryRowContext(ctx,
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
func (s *Store) backfillFTSRangeContext(
	ctx context.Context,
	minID, maxID int64,
	progress func(done, total int64),
) (int64, error) {
	const batchSize = 5000
	idRange := maxID - minID + 1
	var indexed int64
	cursor := minID

	for cursor <= maxID {
		batchEnd := cursor + batchSize
		n, err := s.backfillFTSBatchContext(ctx, cursor, batchEnd)
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
			n, err = s.backfillFTSRowByRowContext(ctx, cursor, batchEnd)
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
func (s *Store) backfillFTSRowByRowContext(
	ctx context.Context,
	fromID, toID int64,
) (int64, error) {
	var indexed int64
	for id := fromID; id < toID; id++ {
		n, err := s.backfillFTSBatchContext(ctx, id, id+1)
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
			if _, uerr := s.db.ExecContext(ctx,
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
// thus a no-op) in production; only export_test.go ever sets it, and it is
// per-Store: see the field's declaration on Store.

// backfillFTSBatch inserts FTS rows for messages with id in [fromID, toID).
//
// Each batch runs under runMaintenance so the pool-wide 30s statement_timeout
// is disabled for the batch: a 5000-row tsvector rewrite can exceed 30s on a
// large archive (finding S1). Each batch remains its own committed transaction,
// preserving the existing "partial progress is preserved if interrupted"
// semantics. No-op timeout reset on SQLite.
func (s *Store) backfillFTSBatchContext(
	ctx context.Context,
	fromID, toID int64,
) (int64, error) {
	if s.backfillFTSBatchErrHook != nil {
		if err := s.backfillFTSBatchErrHook(fromID, toID); err != nil {
			return 0, err
		}
	}
	var affected int64
	err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		result, err := tx.ExecContext(ctx, s.dialect.FTSBackfillBatchSQL(), fromID, toID)
		if err != nil {
			return err
		}
		affected, err = result.RowsAffected()
		return err
	})
	return affected, err
}

const latestConversationPreviewSubquery = `(SELECT snippet FROM messages
	WHERE conversation_id = conversations.id
	ORDER BY COALESCE(sent_at, received_at, internal_date) DESC, id DESC
	LIMIT 1)`

// RecomputeConversationStats updates the denormalized stats columns on all conversations
// belonging to the given source. It recomputes message_count, participant_count,
// last_message_at, and last_message_preview from the current table state.
// Safe to call multiple times — always produces the same result (idempotent).
func (s *Store) RecomputeConversationStats(sourceID int64) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	write := func(q querier) error {
		return s.recomputeConversationStatsWith(q, "source_id = ?", sourceID)
	}
	if s.syncGeneration != nil {
		return s.withTx(func(tx *loggedTx) error { return write(tx) })
	}
	return write(s.db)
}

// RecomputeConversationStatsForMessage updates the denormalized stats only for
// the conversation containing messageID.
func (s *Store) RecomputeConversationStatsForMessage(messageID int64) error {
	return s.RecomputeConversationStatsForMessageContext(context.Background(), messageID)
}

// RecomputeConversationStatsForMessageContext is the request-aware form of
// RecomputeConversationStatsForMessage.
func (s *Store) RecomputeConversationStatsForMessageContext(ctx context.Context, messageID int64) error {
	write := func(q querier) error {
		if err := s.requireSyncMessageSourceTx(q, messageID); err != nil {
			return err
		}
		return s.recomputeConversationStatsWith(q,
			"id = (SELECT conversation_id FROM messages WHERE id = ?)", messageID)
	}
	if s.syncGeneration != nil {
		return s.withTxContext(ctx, func(tx *loggedTx) error {
			return write(boundQuerier{ctx: ctx, q: tx})
		})
	}
	return write(boundQuerier{ctx: ctx, q: s.db})
}

func (s *Store) recomputeConversationStatsWith(q querier, whereClause string, arg any) error {
	_, err := q.Exec(fmt.Sprintf(`
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
			last_message_preview = %s
		WHERE %s
	`, latestConversationPreviewSubquery, whereClause), arg)
	if err != nil {
		return fmt.Errorf("recompute conversation stats: %w", err)
	}
	return nil
}

// RecomputeConversationPreviewIfMatches refreshes one denormalized preview
// from current message state only if it still equals expected. It returns
// whether the guarded row was updated.
func (s *Store) RecomputeConversationPreviewIfMatches(conversationID int64, expected string) (bool, error) {
	result, err := s.db.Exec(s.Rebind(fmt.Sprintf(`UPDATE conversations
		SET last_message_preview = %s
		WHERE id = ? AND last_message_preview = ?`, latestConversationPreviewSubquery)),
		conversationID, expected)
	if err != nil {
		return false, fmt.Errorf("recompute conversation preview: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read recomputed conversation preview count: %w", err)
	}
	return updated > 0, nil
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
// failures). Policy-skipped markers are yielded only when the current policy
// now permits them, or when a participant limit is configured but no
// authoritative roster is archived — the importer re-resolves membership
// before it evaluates the threshold. Used to retry just actionable gaps
// instead of re-fetching everything or repeatedly walking unchanged
// exclusions.
func (s *Store) ForEachTeamsIncompleteHostedContentBody(
	sourceID int64,
	policy attachmentpolicy.Policy,
	fn func(messageID int64, bodyHTML string) error,
) error {
	type bodyRow struct {
		id             int64
		body           string
		hostedRefCount int
	}
	var buf []bodyRow

	rows, err := s.db.Query(`
		SELECT mb.message_id, mb.body_html,
		       (SELECT COUNT(*) FROM attachments a
		        WHERE a.message_id = mb.message_id
		          AND a.storage_path NOT LIKE 'http%' AND a.storage_path != ''
		          AND a.content_hash != ''),
		       EXISTS (
		         SELECT 1 FROM attachments a
		         WHERE a.message_id = mb.message_id
		           AND a.source_attachment_id LIKE 'teams:inline:%'
		           AND COALESCE(a.content_hash, '') = ''
		       )
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
		var hasUnstoredMarker bool
		if err := rows.Scan(&messageID, &bodyHTML, &localAttachmentRows, &hasUnstoredMarker); err != nil {
			_ = rows.Close()
			return err
		}
		if !bodyHTML.Valid || bodyHTML.String == "" {
			continue
		}
		hostedRefCount := countDistinctHostedContentRefs(bodyHTML.String)
		if hostedRefCount > localAttachmentRows || hasUnstoredMarker {
			buf = append(buf, bodyRow{id: messageID, body: bodyHTML.String, hostedRefCount: hostedRefCount})
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
		refs, err := s.MessageTeamsInlineAttachments(r.id)
		if err != nil {
			return err
		}
		eligible := r.hostedRefCount > len(refs)
		var membership ConversationMembership
		membershipPolicy := policy
		membershipLoaded := false
		for _, ref := range refs {
			if ref.ContentHash != "" {
				continue
			}
			if attachmentpolicy.RetryEligible(ref.State) {
				eligible = true
				break
			}
			if ref.State != attachmentpolicy.StateSkipped {
				continue
			}
			if !membershipLoaded {
				membership, err = s.AttachmentConversationMembership(r.id)
				if err != nil {
					return err
				}
				membershipLoaded = true
				if !membership.RosterArchived && policy.MaxParticipants > 0 {
					// No authoritative roster is archived, so the accumulated
					// participant count cannot decide the threshold. Yield the
					// message and let the importer re-resolve membership before
					// it evaluates the policy; the other rules still apply here.
					membershipPolicy.MaxParticipants = 0
				}
			}
			if membershipPolicy.Allows(membership.Conversation, int64(ref.Size)) {
				eligible = true
				break
			}
		}
		if !eligible {
			continue
		}
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
	if err := s.requireSyncSource(sourceID); err != nil {
		return 0, err
	}
	if s.syncGeneration != nil {
		var id int64
		err := s.withTx(func(tx *loggedTx) error {
			var err error
			id, err = ensureConversationWithType(tx, s.dialect, sourceID, sourceConversationID, conversationType, title)
			return err
		})
		return id, err
	}
	return ensureConversationWithType(s.db, s.dialect, sourceID, sourceConversationID, conversationType, title)
}

func ensureConversationWithType(q querier, dialect Dialect, sourceID int64, sourceConversationID, conversationType, title string) (int64, error) {
	now := dialect.Now()
	var id int64
	// The conflict UPDATE only fires when it would change something. The
	// SQLite activity conversation trigger requeues every message in the
	// conversation on ANY conversations UPDATE, so an unconditional upsert
	// would replay whole threads once per persisted message. A filtered
	// conflict emits no RETURNING row, hence the lookup fallback.
	err := q.QueryRow(fmt.Sprintf(`
		INSERT INTO conversations (source_id, source_conversation_id, conversation_type, title, created_at, updated_at)
		VALUES (?, ?, ?, ?, %s, %s)
		ON CONFLICT (source_id, source_conversation_id) DO UPDATE
		SET conversation_type = EXCLUDED.conversation_type,
		    title = CASE WHEN EXCLUDED.title IS NOT NULL AND EXCLUDED.title != ''
		                 THEN EXCLUDED.title ELSE conversations.title END,
		    updated_at = %s
		WHERE conversations.conversation_type <> EXCLUDED.conversation_type
		   OR (EXCLUDED.title IS NOT NULL AND EXCLUDED.title != ''
		       AND COALESCE(conversations.title, '') <> EXCLUDED.title)
		RETURNING id
	`, now, now, now), sourceID, sourceConversationID, conversationType, title).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if err := q.QueryRow(
		`SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?`,
		sourceID, sourceConversationID,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("ensure conversation %d/%q: %w",
			sourceID, sourceConversationID, err)
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

	// The conflict target mirrors the partial unique index on
	// participants(phone_number) WHERE phone_number IS NOT NULL exactly,
	// which is required by both PG and SQLite for partial-index ON CONFLICT
	// to bind. INSERT ... DO NOTHING lets the actual insert be distinguished
	// from an existing participant; a guarded UPDATE then reports whether an
	// existing blank display name was really filled.
	var id int64
	err := s.withTx(func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTx(tx); err != nil {
			return err
		}
		displayNameChanged := false
		now := s.dialect.Now()
		for range 3 {
			insertResult, err := tx.Exec(fmt.Sprintf(`
				INSERT INTO participants (phone_number, display_name, created_at, updated_at)
				VALUES (?, ?, %s, %s)
				ON CONFLICT (phone_number) WHERE phone_number IS NOT NULL
					DO NOTHING
			`, now, now), phone, displayName)
			if err != nil {
				return fmt.Errorf("insert participant by phone: %w", err)
			}
			inserted, err := insertResult.RowsAffected()
			if err != nil {
				return fmt.Errorf("check participant by phone insert: %w", err)
			}
			if inserted > 0 {
				if err := s.bumpParticipantDisplayNameRevision(tx); err != nil {
					return err
				}
			}
			if inserted == 0 && displayName != "" {
				updateResult, err := tx.Exec(`
					UPDATE participants SET display_name = ?
					WHERE phone_number = ?
					  AND COALESCE(NULLIF(TRIM(display_name), ''), '') = ''
					  AND ? != ''
					  AND (display_name IS NULL OR display_name <> ?)
				`, displayName, phone, displayName, displayName)
				if err != nil {
					return fmt.Errorf("backfill participant by phone: %w", err)
				}
				displayNameChanged, err = s.bumpParticipantDisplayNameRevisionIfChanged(tx, updateResult)
				if err != nil {
					return err
				}
			}
			lookupErr := tx.QueryRow(
				`SELECT id FROM participants WHERE phone_number = ?`+s.dialect.SelectForUpdate(),
				phone,
			).Scan(&id)
			if lookupErr == nil {
				break
			}
			if !errors.Is(lookupErr, sql.ErrNoRows) {
				return fmt.Errorf("lookup participant by phone: %w", lookupErr)
			}
		}
		if id == 0 {
			return fmt.Errorf("ensure participant by phone %q after concurrent deletion", phone)
		}

		// Ensure a participant_identifiers row exists for this identifierType
		// and attach service/scope metadata whenever the importer namespace is
		// unambiguous. A repeat call repairs metadata but does not repoint the
		// identifier away from its existing participant.
		classificationColumns, err := s.participantIdentifierClassificationColumnsTx(tx)
		if err != nil {
			return err
		}
		finish := func(result sql.Result) error {
			if err := s.bumpParticipantIdentifierRevisionIfChanged(tx, result); err != nil {
				return err
			}
			if !displayNameChanged {
				return nil
			}
			return s.invalidateParticipantPersonEnrichmentTx(
				context.Background(), tx, id)
		}
		if !classificationColumns {
			result, err := tx.Exec(`INSERT INTO participant_identifiers (
					participant_id, identifier_type, identifier_value, is_primary
				) VALUES (?, ?, ?, TRUE)
				ON CONFLICT (identifier_type, identifier_value) DO NOTHING`,
				id, identifierType, phone)
			if err != nil {
				return fmt.Errorf("insert participant identifier: %w", err)
			}
			return finish(result)
		}
		serviceSlug, scopeKind, scopeValue := participantIdentifierClassificationValues(
			identifierType, phone,
		)
		result, err := tx.Exec(`INSERT INTO participant_identifiers (
				participant_id, identifier_type, identifier_value, is_primary,
				service_id, scope_kind, scope_value
			) VALUES (?, ?, ?, TRUE,
				(SELECT id FROM communication_services WHERE slug = ?), ?, ?)
			ON CONFLICT (identifier_type, identifier_value) DO UPDATE SET
				service_id = COALESCE(excluded.service_id, participant_identifiers.service_id),
				scope_kind = CASE WHEN excluded.service_id IS NOT NULL
					THEN excluded.scope_kind ELSE participant_identifiers.scope_kind END,
				scope_value = CASE WHEN excluded.service_id IS NOT NULL
					THEN excluded.scope_value ELSE participant_identifiers.scope_value END
			WHERE excluded.service_id IS NOT NULL AND (
				participant_identifiers.service_id IS NULL OR
				participant_identifiers.service_id <> excluded.service_id OR
				(participant_identifiers.scope_kind IS NULL AND
					excluded.scope_kind IS NOT NULL) OR
				(participant_identifiers.scope_kind IS NOT NULL AND
					excluded.scope_kind IS NULL) OR
				participant_identifiers.scope_kind <> excluded.scope_kind OR
				(participant_identifiers.scope_value IS NULL AND
					excluded.scope_value IS NOT NULL) OR
				(participant_identifiers.scope_value IS NOT NULL AND
					excluded.scope_value IS NULL) OR
				participant_identifiers.scope_value <> excluded.scope_value
			)`,
			id, identifierType, phone, serviceSlug, scopeKind, scopeValue)
		if err != nil {
			return fmt.Errorf("insert participant identifier: %w", err)
		}
		return finish(result)
	})
	if err != nil {
		return 0, err
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
		// Serialize the curated binding check with promotion and link/unlink
		// mutations before this transaction repoints any archive references.
		if err := s.lockIdentityMutationTx(tx); err != nil {
			return err
		}
		if err := s.lockParticipantDirectoryMutationTxContext(
			context.Background(), tx,
		); err != nil {
			return err
		}
		if err := s.lockParticipantObservationMergeTx(
			context.Background(), tx, oldID, newID,
		); err != nil {
			return err
		}
		if err := s.verifyParticipantsExistTx(tx, oldID, newID); err != nil {
			return err
		}
		edges, err := s.loadLinkEdgesTx(tx)
		if err != nil {
			return err
		}
		personID, unionMembers, err := s.personForClusterUnionTx(
			context.Background(), tx, oldID, newID, edges,
		)
		if err != nil {
			return err
		}
		personRevisionBumped := false
		if personID != 0 {
			changed, err := s.mergePersonBindingsTx(
				context.Background(), tx, personID, oldID, unionMembers)
			if err != nil {
				return err
			}
			if changed {
				if err := s.bumpPersonRevisionsTx(
					context.Background(), tx, personID); err != nil {
					return err
				}
				personRevisionBumped = true
			}
		}

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
		// Collide on the full unique key including the normalized envelope
		// address (idx_message_recipients_envelope), not just (message_id,
		// recipient_type): a row of the absorbed participant whose envelope
		// snapshot differs from every surviving row's must survive the
		// repoint, or the merge would destroy the immutable alias evidence
		// identity discovery classifies from.
		if _, err := tx.Exec(`
			DELETE FROM message_recipients WHERE participant_id = ? AND EXISTS (
				SELECT 1 FROM message_recipients m2 WHERE m2.message_id = message_recipients.message_id
				  AND m2.participant_id = ? AND m2.recipient_type = message_recipients.recipient_type
				  AND LOWER(COALESCE(m2.email_address, '')) = LOWER(COALESCE(message_recipients.email_address, '')))`, oldID, newID); err != nil {
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
		if err := s.rewriteObservationsForMergeTx(
			context.Background(), tx, oldID, newID,
		); err != nil {
			return err
		}
		if err := s.rewriteIdentityMatchCandidatesForMergeTx(
			context.Background(), tx, oldID, newID, edges,
		); err != nil {
			return err
		}
		// Sender and identifier repoints can add or remove identity evidence.
		// Repair the primary-store provenance before committing the merge.
		if err := refreshParticipantMessageAttributionContext(
			context.Background(), tx, newID,
		); err != nil {
			return err
		}
		// Repoint (and, if needed, restructure) any link edges referencing
		// oldID before the delete below drops them via ON DELETE CASCADE.
		if err := s.rewriteLinksForMerge(tx, oldID, newID); err != nil {
			return err
		}
		// Candidate and link endpoints must both reference the survivor before
		// unsupported generated matches are withdrawn. Otherwise an owned link
		// can evade cleanup because it still names the absorbed participant.
		if err := s.reconcileCurrentObservationIdentityMatchesTxContext(
			context.Background(), tx,
		); err != nil {
			return err
		}
		// Bump unconditionally, even when the merge touched no link edges:
		// absorbing a participant whose email matches a confirmed account
		// identity changes owner_participants' content (the baked dataset
		// still lists the deleted oldID), and the refresh this triggers is
		// cheap and idempotent, so it is not worth tracking that case
		// separately from the link-touching one.
		if _, err := s.bumpIdentityRevision(tx); err != nil {
			return err
		}
		// Also bump the account-identity revision: the primary rows were
		// repaired above, but existing message Parquet shards still bake the
		// pre-merge attribution and require a full rebuild.
		if err := s.bumpAccountIdentityRevision(tx); err != nil {
			return err
		}
		if err := s.bumpParticipantIdentifierRevision(tx); err != nil {
			return err
		}
		if err := rewritePersonMergeParticipantLineageTx(
			context.Background(), tx, oldID, newID,
		); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM participants WHERE id = ?`, oldID)
		if err != nil {
			return err
		}
		if personID != 0 {
			if err := s.publishPersonIdentityScopeChangesTx(
				context.Background(), tx, []int64{personID},
				peoplesweep.EvidenceEffectIdentityReassigned); err != nil {
				return err
			}
			if personRevisionBumped {
				return s.invalidatePersonEnrichmentIdentitiesAfterRevisionTx(
					context.Background(), tx, personID)
			}
			return s.invalidatePersonEnrichmentIdentitiesTx(
				context.Background(), tx, personID)
		}
		return nil
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

// AdoptLegacyParticipantIdentifier atomically upgrades one legacy identifier
// to a scoped value. The legacy value is consumed on success, so another
// account cannot claim the same unscoped identifier after the first account
// adopts it. If the legacy owner already has a scoped identifier in the
// supplied namespace, the legacy row is removed and no participant is
// returned: its ownership is ambiguous and must not be reused.
//
// scopedPrefix is a provider-owned namespace marker (for example, "beeper:")
// used only to distinguish account-scoped identifiers from other identifiers
// of the same type, such as a phone value. The identity lock serializes this
// migration with other participant-identifier writers and source cleanup.
func (s *Store) AdoptLegacyParticipantIdentifier(
	identifierType, legacyValue, scopedValue, scopedPrefix string,
) (int64, error) {
	identifierType = strings.TrimSpace(identifierType)
	legacyValue = strings.TrimSpace(legacyValue)
	scopedValue = strings.TrimSpace(scopedValue)
	scopedPrefix = strings.TrimSpace(scopedPrefix)
	if identifierType == "" || legacyValue == "" || scopedValue == "" {
		return 0, errors.New("legacy participant identifier values are required")
	}
	if legacyValue == scopedValue {
		return 0, nil
	}

	var adoptedID int64
	err := s.withTx(func(tx *loggedTx) error {
		// Fast path for the common case where a concurrent import already
		// created the scoped identifier.
		var currentID int64
		lookupErr := tx.QueryRow(`
			SELECT participant_id FROM participant_identifiers
			WHERE identifier_type = ? AND identifier_value = ?
		`, identifierType, scopedValue).Scan(&currentID)
		if lookupErr == nil {
			return nil
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return fmt.Errorf("lookup scoped participant identifier: %w", lookupErr)
		}

		// The write path must take the identity lock before touching
		// participant_identifiers. Re-check all rows after the lock because
		// another identity mutation may have completed while this call waited.
		if err := s.lockIdentityMutationTx(tx); err != nil {
			return err
		}
		lookupErr = tx.QueryRow(`
			SELECT participant_id FROM participant_identifiers
			WHERE identifier_type = ? AND identifier_value = ?
		`, identifierType, scopedValue).Scan(&currentID)
		if lookupErr == nil {
			return nil
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return fmt.Errorf("recheck scoped participant identifier: %w", lookupErr)
		}

		var legacyID int64
		lookup := `SELECT participant_id FROM participant_identifiers
			WHERE identifier_type = ? AND identifier_value = ?` + s.dialect.SelectForUpdate()
		lookupErr = tx.QueryRow(lookup, identifierType, legacyValue).Scan(&legacyID)
		if errors.Is(lookupErr, sql.ErrNoRows) {
			return nil
		}
		if lookupErr != nil {
			return fmt.Errorf("lookup legacy participant identifier: %w", lookupErr)
		}

		// A legacy participant may also own a phone identifier of the same
		// type. Only a second value in the provider's scoped namespace makes
		// the raw value ambiguous.
		var hasScoped bool
		if scopedPrefix != "" {
			if err := tx.QueryRow(`
				SELECT EXISTS (
					SELECT 1 FROM participant_identifiers
					WHERE participant_id = ? AND identifier_type = ?
					  AND identifier_value <> ? AND identifier_value LIKE ?
				)
			`, legacyID, identifierType, legacyValue, scopedPrefix+"%").Scan(&hasScoped); err != nil {
				return fmt.Errorf("check legacy participant scoped identifiers: %w", err)
			}
		}
		if hasScoped {
			if _, err := tx.Exec(`DELETE FROM participant_identifiers
				WHERE identifier_type = ? AND identifier_value = ?`,
				identifierType, legacyValue); err != nil {
				return fmt.Errorf("remove ambiguous legacy participant identifier: %w", err)
			}
			if err := s.bumpParticipantIdentifierRevision(tx); err != nil {
				return err
			}
			return nil
		}

		if _, err := tx.Exec(`UPDATE participant_identifiers
			SET identifier_value = ?
			WHERE identifier_type = ? AND identifier_value = ?`,
			scopedValue, identifierType, legacyValue); err != nil {
			return fmt.Errorf("migrate legacy participant identifier: %w", err)
		}
		if err := s.bumpParticipantIdentifierRevision(tx); err != nil {
			return err
		}
		adoptedID = legacyID
		return nil
	})
	if err != nil {
		return 0, err
	}
	return adoptedID, nil
}

// SetParticipantIdentifier points identifier (type, value) at participantID,
// creating the row or re-pointing an existing one (idempotent). Importers use
// it to persist alternate identifiers on an already-resolved participant so
// later runs unify instead of forking a new participant.
//
// Any write that actually changes the mapping bumps the participant-identifier
// revision: identifiers bake into the identity directory (relationship_people
// search values and the participant_identifiers Parquet export), and the
// derived-dataset refresh repairs that drift cheaply. When the changed
// identifier additionally matches a confirmed account-identity address, it is
// owner evidence: the mutation repairs affected messages in the primary store,
// while the analytics cache bakes it into owner_participants, the per-row
// is_owner flags of the relationship activity index, and the export-derived
// is_from_me flag in message shards. It therefore also bumps the identity and
// account-identity revisions the way MergeParticipants does, forcing the full
// rebuild that re-derives committed shards. No-op calls (the common importer
// re-run) bump nothing.
func (s *Store) SetParticipantIdentifier(participantID int64, identifierType, identifierValue string) error {
	identifierType = strings.TrimSpace(identifierType)
	identifierValue = strings.TrimSpace(identifierValue)
	if identifierType == "" || identifierValue == "" {
		return errors.New("identifier type and value are required")
	}
	return s.withTx(func(tx *loggedTx) error {
		// Fast path first, read-only: importer re-runs hit the no-op case
		// constantly, and it must not take any write lock.
		existingParticipantID, exists, err := participantIdentifierTargetTx(
			tx, identifierType, identifierValue,
		)
		if err != nil || (exists && existingParticipantID == participantID) {
			return err
		}
		// The write path may bump the identity revision below (owner
		// evidence), so the identity-mutation row lock must come BEFORE the
		// participant_identifiers write: BeginExclusive takes that row and
		// then LOCK TABLE participant_identifiers, and the reverse order
		// here would deadlock against a serialized source removal. Re-check
		// the no-op case under the lock: a concurrent call may have set the
		// same mapping while we waited.
		if err := s.lockIdentityMutationTx(tx); err != nil {
			return err
		}
		existingParticipantID, exists, err = participantIdentifierTargetTx(
			tx, identifierType, identifierValue,
		)
		if err != nil || (exists && existingParticipantID == participantID) {
			return err
		}
		serviceSlug, scopeKind, scopeValue := participantIdentifierClassificationValues(
			identifierType, identifierValue,
		)
		classificationColumns, err := s.participantIdentifierClassificationColumnsTx(tx)
		if err != nil {
			return err
		}
		var setErr error
		if classificationColumns {
			_, setErr = tx.Exec(`
				INSERT INTO participant_identifiers (
					participant_id, identifier_type, identifier_value, is_primary,
					service_id, scope_kind, scope_value
				) VALUES (?, ?, ?, FALSE,
					(SELECT id FROM communication_services WHERE slug = ?), ?, ?)
				ON CONFLICT (identifier_type, identifier_value) DO UPDATE SET
					participant_id = excluded.participant_id,
					service_id = COALESCE(excluded.service_id, participant_identifiers.service_id),
					scope_kind = CASE WHEN excluded.service_id IS NOT NULL
						THEN excluded.scope_kind ELSE participant_identifiers.scope_kind END,
					scope_value = CASE WHEN excluded.service_id IS NOT NULL
						THEN excluded.scope_value ELSE participant_identifiers.scope_value END
			`, participantID, identifierType, identifierValue,
				serviceSlug, scopeKind, scopeValue)
		} else {
			// Cache inspection can open a legacy archive before InitSchema adds
			// service metadata. Preserve that read/repair workflow; the v2
			// migration classifies this row when the schema is initialized.
			_, setErr = tx.Exec(`
				INSERT INTO participant_identifiers (
					participant_id, identifier_type, identifier_value, is_primary
				) VALUES (?, ?, ?, FALSE)
				ON CONFLICT (identifier_type, identifier_value) DO UPDATE SET
					participant_id = excluded.participant_id
			`, participantID, identifierType, identifierValue)
		}
		if setErr != nil {
			return fmt.Errorf("set participant identifier: %w", setErr)
		}
		if err := s.bumpParticipantIdentifierRevision(tx); err != nil {
			return err
		}
		var ownerEvidence bool
		if err := tx.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM account_identities ai
				WHERE (? = 'email' AND lower(?) = lower(ai.address))
				   OR (? != 'email' AND ? = ai.address)
			)
		`, identifierType, identifierValue, identifierType, identifierValue).Scan(&ownerEvidence); err != nil {
			return fmt.Errorf("check identifier owner evidence: %w", err)
		}
		if !ownerEvidence {
			return nil
		}
		if err := refreshParticipantMessageAttributionContext(
			context.Background(), tx, existingParticipantID, participantID,
		); err != nil {
			return err
		}
		if _, err := s.bumpIdentityRevision(tx); err != nil {
			return err
		}
		return s.bumpAccountIdentityRevision(tx)
	})
}

// ParticipantEmailRepair is one maintenance rewrite of a participant's
// email_address (encoding repair).
type ParticipantEmailRepair struct {
	ParticipantID int64
	EmailAddress  string
}

// RepairParticipantEmailAddresses applies one maintenance batch of email
// rewrites and settles ownership-derived state IN THE SAME TRANSACTION. The
// email is an attribution and activity-ownership surface: a repaired address
// can start or stop matching a confirmed account identity, so the persisted
// message attribution of the touched participants is re-derived and both
// identity revisions advance — activity projection and published caches key
// staleness on those revisions and would otherwise never reproject. Atomicity
// is the recovery story: the rewrite makes the address valid UTF-8, so a
// rerun's scan can never rediscover a row whose rewrite committed without
// its refresh, and a failed batch rolls back entirely and stays discoverable.
func (s *Store) RepairParticipantEmailAddresses(repairs []ParticipantEmailRepair) error {
	if len(repairs) == 0 {
		return nil
	}
	return s.withTx(func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTx(tx); err != nil {
			return err
		}
		participantIDs := make([]int64, 0, len(repairs))
		for _, repair := range repairs {
			result, err := tx.Exec(
				`UPDATE participants SET email_address = ? WHERE id = ?
				 AND (email_address IS NULL OR email_address <> ?)`,
				repair.EmailAddress, repair.ParticipantID, repair.EmailAddress,
			)
			if err != nil {
				return fmt.Errorf(
					"repair participant email %d: %w", repair.ParticipantID, err)
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("check participant email %d repair: %w", repair.ParticipantID, err)
			}
			if changed > 0 {
				participantIDs = append(participantIDs, repair.ParticipantID)
			}
		}
		const chunkSize = 500
		for start := 0; start < len(participantIDs); start += chunkSize {
			end := min(start+chunkSize, len(participantIDs))
			if err := refreshParticipantMessageAttributionContext(
				context.Background(), tx, participantIDs[start:end]...,
			); err != nil {
				return err
			}
		}
		if _, err := s.bumpIdentityRevision(tx); err != nil {
			return err
		}
		if err := s.bumpAccountIdentityRevision(tx); err != nil {
			return err
		}
		return s.invalidateParticipantPersonEnrichmentTx(
			context.Background(), tx, participantIDs...)
	})
}

func (s *Store) participantIdentifierClassificationColumnsTx(
	tx *loggedTx,
) (bool, error) {
	var count int
	query := `SELECT COUNT(*) FROM pragma_table_info('participant_identifiers')
		WHERE name IN ('service_id', 'scope_kind', 'scope_value')`
	if s.IsPostgreSQL() {
		query = `SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = 'participant_identifiers'
			  AND column_name IN ('service_id', 'scope_kind', 'scope_value')`
	}
	if err := tx.QueryRow(query).Scan(&count); err != nil {
		return false, fmt.Errorf("inspect participant identifier classification schema: %w", err)
	}
	return count == 3, nil
}

// participantIdentifierTargetTx returns the participant currently owning an
// identifier, if any, without taking any lock.
func participantIdentifierTargetTx(
	tx *loggedTx,
	identifierType,
	identifierValue string,
) (int64, bool, error) {
	var existingID int64
	err := tx.QueryRow(`
		SELECT participant_id FROM participant_identifiers
		WHERE identifier_type = ? AND identifier_value = ?
	`, identifierType, identifierValue).Scan(&existingID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("lookup participant identifier: %w", err)
	}
	return existingID, err == nil, nil
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
	err := s.withTx(func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTx(tx); err != nil {
			return err
		}
		err := tx.QueryRow(`
			SELECT participant_id FROM participant_identifiers
			WHERE identifier_type = ? AND identifier_value = ?
		`, identifierType, identifierValue).Scan(&participantID)
		if err == nil {
			if displayName != "" {
				result, err := tx.Exec(`
					UPDATE participants SET display_name = ?
					WHERE id = ? AND (display_name IS NULL OR display_name = '')
				`, displayName, participantID)
				if err != nil {
					return fmt.Errorf("backfill participant display name: %w", err)
				}
				changed, err := s.bumpParticipantDisplayNameRevisionIfChanged(tx, result)
				if err != nil {
					return err
				}
				if changed {
					return s.invalidateParticipantPersonEnrichmentTx(
						context.Background(), tx, participantID)
				}
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lookup participant identifier: %w", err)
		}

		now := s.dialect.Now()
		if err := tx.QueryRow(fmt.Sprintf(`
			INSERT INTO participants (display_name, created_at, updated_at)
			VALUES (?, %s, %s)
			RETURNING id
		`, now, now), displayName).Scan(&participantID); err != nil {
			return fmt.Errorf("insert participant: %w", err)
		}
		if err := s.bumpParticipantDisplayNameRevision(tx); err != nil {
			return err
		}
		classificationColumns, err := s.participantIdentifierClassificationColumnsTx(tx)
		if err != nil {
			return err
		}
		if !classificationColumns {
			_, err = tx.Exec(`
				INSERT INTO participant_identifiers (
					participant_id, identifier_type, identifier_value, display_value, is_primary
				) VALUES (?, ?, ?, ?, TRUE)
			`, participantID, identifierType, identifierValue, identifierValue)
			if err != nil {
				return fmt.Errorf("insert participant identifier: %w", err)
			}
		} else {
			serviceSlug, scopeKind, scopeValue := participantIdentifierClassificationValues(
				identifierType, identifierValue,
			)
			_, err = tx.Exec(`
				INSERT INTO participant_identifiers (
					participant_id, identifier_type, identifier_value, display_value,
					is_primary, service_id, scope_kind, scope_value
				) VALUES (?, ?, ?, ?, TRUE,
					(SELECT id FROM communication_services WHERE slug = ?), ?, ?)
			`, participantID, identifierType, identifierValue, identifierValue,
				serviceSlug, scopeKind, scopeValue)
			if err != nil {
				return fmt.Errorf("insert participant identifier: %w", err)
			}
		}
		return s.bumpParticipantIdentifierRevision(tx)
	})
	if err != nil {
		return 0, err
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

	var updated bool
	err := s.withTx(func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTx(tx); err != nil {
			return err
		}
		participantIDs, err := s.participantIdentityUpdateIDsTx(tx,
			`SELECT id FROM participants
			 WHERE phone_number = ? AND (display_name IS NULL OR display_name = '')`, phone)
		if err != nil {
			return err
		}
		result, err := tx.Exec(fmt.Sprintf(`
			UPDATE participants SET display_name = ?, updated_at = %s
			WHERE phone_number = ? AND (display_name IS NULL OR display_name = '')
		`, s.dialect.Now()), displayName, phone)
		if err != nil {
			return err
		}
		updated, err = s.bumpParticipantDisplayNameRevisionIfChanged(tx, result)
		if err != nil || !updated {
			return err
		}
		return s.invalidateParticipantPersonEnrichmentTx(
			context.Background(), tx, participantIDs...)
	})
	if err != nil {
		return false, err
	}
	return updated, nil
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

	var updated bool
	err := s.withTx(func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTx(tx); err != nil {
			return err
		}
		participantIDs, err := s.participantIdentityUpdateIDsTx(tx, `SELECT id FROM participants
			WHERE phone_number = ?
			  AND (display_name IS NULL OR display_name = '' OR display_name = phone_number)
			  AND (display_name IS NULL OR display_name <> ?)
			  AND EXISTS (
			      SELECT 1 FROM participant_identifiers pi
			      WHERE pi.participant_id = participants.id
			        AND pi.identifier_type = 'imessage'
			  )`, phone, displayName)
		if err != nil {
			return err
		}
		result, err := tx.Exec(fmt.Sprintf(`
			UPDATE participants SET display_name = ?, updated_at = %s
			WHERE phone_number = ?
			  AND (display_name IS NULL OR display_name = '' OR display_name = phone_number)
			  AND (display_name IS NULL OR display_name <> ?)
			  AND EXISTS (
			      SELECT 1 FROM participant_identifiers pi
			      WHERE pi.participant_id = participants.id
			        AND pi.identifier_type = 'imessage'
			  )
		`, s.dialect.Now()), displayName, phone, displayName)
		if err != nil {
			return err
		}
		updated, err = s.bumpParticipantDisplayNameRevisionIfChanged(tx, result)
		if err != nil || !updated {
			return err
		}
		return s.invalidateParticipantPersonEnrichmentTx(
			context.Background(), tx, participantIDs...)
	})
	if err != nil {
		return false, err
	}
	return updated, nil
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

	var updated bool
	err := s.withTx(func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTx(tx); err != nil {
			return err
		}
		participantIDs, err := s.participantIdentityUpdateIDsTx(tx,
			`SELECT id FROM participants
			 WHERE LOWER(email_address) = LOWER(?)
			   AND (display_name IS NULL OR display_name = '')`, email)
		if err != nil {
			return err
		}
		result, err := tx.Exec(fmt.Sprintf(`
			UPDATE participants SET display_name = ?, updated_at = %s
			WHERE LOWER(email_address) = LOWER(?) AND (display_name IS NULL OR display_name = '')
		`, s.dialect.Now()), displayName, email)
		if err != nil {
			return err
		}
		updated, err = s.bumpParticipantDisplayNameRevisionIfChanged(tx, result)
		if err != nil || !updated {
			return err
		}
		return s.invalidateParticipantPersonEnrichmentTx(
			context.Background(), tx, participantIDs...)
	})
	if err != nil {
		return false, err
	}
	return updated, nil
}

func (s *Store) participantIdentityUpdateIDsTx(
	tx *loggedTx, query string, args ...any,
) ([]int64, error) {
	rows, err := tx.Query(query+` ORDER BY id`+s.dialect.SelectForUpdate(), args...)
	if err != nil {
		return nil, fmt.Errorf("lock participant identity updates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	participantIDs := make([]int64, 0)
	for rows.Next() {
		var participantID int64
		if err := rows.Scan(&participantID); err != nil {
			return nil, fmt.Errorf("scan participant identity update: %w", err)
		}
		participantIDs = append(participantIDs, participantID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate participant identity updates: %w", err)
	}
	return participantIDs, nil
}

// EnsureConversationParticipant adds a participant to a conversation.
// Uses INSERT OR IGNORE to be idempotent.
func (s *Store) EnsureConversationParticipant(conversationID, participantID int64, role string) error {
	write := func(q querier) error {
		if err := s.requireSyncConversationSourceTx(q, conversationID); err != nil {
			return err
		}
		_, err := q.Exec(s.dialect.InsertOrIgnore(fmt.Sprintf(`INSERT OR IGNORE INTO conversation_participants (conversation_id, participant_id, role, joined_at)
			VALUES (?, ?, ?, %s)`, s.dialect.Now())), conversationID, participantID, role)
		return err
	}
	if s.syncGeneration == nil {
		return write(s.db)
	}
	return s.withTx(func(tx *loggedTx) error { return write(tx) })
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
		if err := s.requireSyncConversationSourceTx(tx, conversationID); err != nil {
			return err
		}
		return replaceConversationParticipantsTx(
			context.Background(), tx, s.dialect, conversationID, participants,
		)
	})
}

func replaceConversationParticipantsTx(
	ctx context.Context,
	tx *loggedTx,
	dialect Dialect,
	conversationID int64,
	participants []ConversationParticipantRef,
) error {
	q := boundQuerier{ctx: ctx, q: tx}
	if dialect.DriverName() != postgresDriverName {
		// Acquire SQLite's writer slot before taking the journal snapshot. A
		// deferred transaction that reads first cannot upgrade its stale WAL
		// snapshot if another writer commits before the membership DELETE.
		if _, err := q.Exec(`UPDATE embedding_change_clock SET sequence = sequence WHERE singleton = 1`); err != nil {
			return fmt.Errorf("lock conversation membership journal: %w", err)
		}
	}
	var journalBefore int64
	if err := q.QueryRow(`SELECT sequence FROM embedding_change_clock WHERE singleton = 1`).Scan(&journalBefore); err != nil {
		return fmt.Errorf("read embedding change clock before membership snapshot: %w", err)
	}
	desired := make(map[int64]string, len(participants))
	for _, participant := range participants {
		if participant.ParticipantID == 0 {
			continue
		}
		desired[participant.ParticipantID] = participant.Role
	}
	ids := make([]int64, 0, len(desired))
	for participantID := range desired {
		ids = append(ids, participantID)
	}
	slices.Sort(ids)
	rows, err := tx.QueryContext(ctx, `
		SELECT participant_id FROM conversation_participants
		WHERE conversation_id = ? ORDER BY participant_id`, conversationID)
	if err != nil {
		return err
	}
	stale := make([]int64, 0)
	for rows.Next() {
		var participantID int64
		if err := rows.Scan(&participantID); err != nil {
			_ = rows.Close()
			return err
		}
		if _, keep := desired[participantID]; !keep {
			stale = append(stale, participantID)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := execInChunksContext(ctx, tx, stale, []any{conversationID}, `
		DELETE FROM conversation_participants
		WHERE conversation_id = ? AND participant_id IN (%s)`); err != nil {
		return err
	}
	for _, participantID := range ids {
		if _, err := q.Exec(fmt.Sprintf(`
			INSERT INTO conversation_participants (conversation_id, participant_id, role, joined_at)
			VALUES (?, ?, ?, %s)
			ON CONFLICT (conversation_id, participant_id) DO UPDATE SET role = excluded.role
			WHERE conversation_participants.role IS DISTINCT FROM excluded.role`, dialect.Now()),
			conversationID, participantID, desired[participantID]); err != nil {
			return err
		}
	}
	return consolidateConversationParticipantJournal(q, dialect, conversationID, journalBefore)
}

func consolidateConversationParticipantJournal(tx querier, dialect Dialect, conversationID, after int64) error {
	var retained sql.NullInt64
	err := tx.QueryRow(dialect.Rebind(`
		SELECT MAX(sequence) FROM embedding_changes
		WHERE sequence > ? AND kind = 'conversation_participant'
		  AND (old_conversation_id = ? OR new_conversation_id = ?)`),
		after, conversationID, conversationID).Scan(&retained)
	if err != nil {
		return fmt.Errorf("read membership journal snapshot: %w", err)
	}
	if !retained.Valid {
		return nil
	}
	if _, err := tx.Exec(dialect.Rebind(`
		DELETE FROM embedding_changes
		WHERE sequence > ? AND sequence <> ? AND kind = 'conversation_participant'
		  AND (old_conversation_id = ? OR new_conversation_id = ?)`),
		after, retained.Int64, conversationID, conversationID); err != nil {
		return fmt.Errorf("coalesce membership journal snapshot: %w", err)
	}
	if _, err := tx.Exec(dialect.Rebind(`
		UPDATE embedding_changes
		SET old_conversation_id = ?, new_conversation_id = ?, participant_id = NULL
		WHERE sequence = ?`), conversationID, conversationID, retained.Int64); err != nil {
		return fmt.Errorf("normalize membership journal snapshot: %w", err)
	}
	return nil
}

// UpsertReaction inserts or ignores a reaction.
func (s *Store) UpsertReaction(messageID, participantID int64, reactionType, reactionValue string, createdAt time.Time) error {
	write := func(q querier) error {
		if err := s.requireSyncMessageSourceTx(q, messageID); err != nil {
			return err
		}
		_, err := q.Exec(s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO reactions (message_id, participant_id, reaction_type, reaction_value, created_at)
			VALUES (?, ?, ?, ?, ?)`), messageID, participantID, reactionType, reactionValue, createdAt)
		return err
	}
	if s.syncGeneration != nil {
		return s.withTx(func(tx *loggedTx) error { return write(tx) })
	}
	return write(s.db)
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
	if s.syncGeneration != nil {
		return s.withTx(func(tx *loggedTx) error {
			if err := s.requireSyncMessageSourceTx(tx, messageID); err != nil {
				return err
			}
			return upsertMessageRawWithFormat(tx, messageID, rawData, format)
		})
	}
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

// UpsertAttachment is the compatibility write path for callers without stable
// occurrence provenance. It fails closed to role unknown/legacy_api and keeps
// the legacy best-effort content-hash identity. New and source-aware writers
// use UpsertAttachmentRecord. `size` is widened to int64 at the bind boundary
// so 32-bit builds cannot truncate large attachments before the column (BIGINT
// on PG, INTEGER on SQLite).
//
// When contentHash is empty (the rare untyped-blob path used by some
// importers), the unique index does not cover the row; a best-effort
// (message_id, empty-hash) match is used to avoid trivial duplicates,
// but two concurrent empty-hash inserts on the same message may both
// succeed.
func (s *Store) UpsertAttachment(messageID int64, filename, mimeType, storagePath, contentHash string, size int) error {
	return s.UpsertAttachmentRecord(context.Background(), messageID, AttachmentWrite{
		Filename:    filename,
		MIMEType:    mimeType,
		StoragePath: storagePath,
		ContentHash: contentHash,
		Size:        int64(size),
		Role:        AttachmentRoleUnknown,
		RoleSource:  AttachmentRoleSourceLegacyAPI,
	})
}

// RecomputeMessageAttachmentStats refreshes the denormalized attachment flags
// on one message from its current attachment rows.
func (s *Store) RecomputeMessageAttachmentStats(messageID int64) error {
	write := func(q querier) error {
		if err := s.requireSyncMessageSourceTx(q, messageID); err != nil {
			return err
		}
		_, err := q.Exec(`
			UPDATE messages
			SET has_attachments = (SELECT COUNT(*) FROM attachments WHERE message_id = ?) > 0,
			    attachment_count = (SELECT COUNT(*) FROM attachments WHERE message_id = ?)
			WHERE id = ?
		`, messageID, messageID, messageID)
		return err
	}
	if s.syncGeneration == nil {
		return write(s.db)
	}
	return s.withTx(func(tx *loggedTx) error { return write(tx) })
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
	// Metadata is importer-supplied JSON stored in attachments.attachment_metadata
	// (e.g. the source URL a link-preview attachment was forwarded from). Empty
	// stores NULL; callers own the shape and must supply valid JSON.
	Metadata      string
	Role          AttachmentRole
	RoleSource    AttachmentRoleSource
	SourcePartKey string
	ContentID     string
	// State and SkipReason distinguish unfinished fetches from deliberate policy exclusions.
	State      attachmentpolicy.DownloadState
	SkipReason attachmentpolicy.SkipReason
}

// replaceMessageAttachmentsWhere atomically deletes a message's attachment
// rows matching deleteWhere and inserts refs. Refs with an empty StoragePath
// (and, when requireHash is set, an empty ContentHash) are skipped.
func (s *Store) replaceMessageAttachmentsWhere(
	messageID int64, deleteWhere string, requireHash bool, refs []AttachmentRef, deleteArgs ...any,
) error {
	return s.withTx(func(tx *loggedTx) error {
		args := append([]any{messageID}, deleteArgs...)
		if _, err := tx.Exec(`DELETE FROM attachments WHERE message_id = ? AND (`+deleteWhere+`)`, args...); err != nil {
			return err
		}
		for _, ref := range refs {
			if ref.StoragePath == "" || (requireHash && ref.ContentHash == "") {
				continue
			}
			write := AttachmentWrite{
				Filename:           ref.Filename,
				MIMEType:           ref.MimeType,
				StoragePath:        ref.StoragePath,
				ContentHash:        ref.ContentHash,
				Size:               int64(ref.Size),
				SourceAttachmentID: ref.SourceAttachmentID,
				MediaType:          ref.MediaType,
				Width:              ref.Width,
				Height:             ref.Height,
				DurationMS:         ref.DurationMS,
				Metadata:           ref.Metadata,
				Role:               ref.Role,
				RoleSource:         ref.RoleSource,
				SourcePartKey:      ref.SourcePartKey,
				ContentID:          ref.ContentID,
				State:              ref.State,
				SkipReason:         ref.SkipReason,
			}.normalized()
			if write.SourcePartKey == "" && write.SourceAttachmentID != "" {
				// Provider attachment IDs are already namespaced by every caller of
				// this replacement path. They are the stable occurrence identity;
				// unlike a content hash, they preserve two source parts with the
				// same bytes and keep hashless pending rows distinct.
				write.SourcePartKey = write.SourceAttachmentID
			}
			if err := write.validate(); err != nil {
				return err
			}
			if err := s.upsertAttachmentRecord(tx, messageID, write); err != nil {
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
// a message. When preserveLegacy is false, it also removes legacy unmarked
// Teams inline rows produced before stable source attachment IDs were added.
// Callers preserve those rows while a current hosted-content replacement is
// excluded or unfinished, so an ordinary resync cannot detach archived media.
// URL-backed reference/recording attachments are always left untouched.
func (s *Store) ReplaceMessageInlineAttachments(messageID int64, refs []AttachmentRef, preserveLegacy bool) error {
	deleteWhere := `source_attachment_id LIKE 'teams:inline:%'`
	if !preserveLegacy {
		deleteWhere += ` OR (
		  (source_attachment_id IS NULL OR source_attachment_id = '')
		  AND storage_path != ''
		  AND storage_path NOT LIKE 'http://%'
		  AND storage_path NOT LIKE 'https://%'
		  AND content_hash IS NOT NULL
		  AND content_hash != ''
		  AND COALESCE(filename, '') = ''
		  AND COALESCE(mime_type, '') = ''
		)`
	}
	return s.replaceMessageAttachmentsWhere(messageID, deleteWhere, false, refs)
}

// MessageTeamsInlineAttachments returns Teams-managed inline media rows keyed
// by their stable hosted-content source identifier.
func (s *Store) MessageTeamsInlineAttachments(messageID int64) (map[string]AttachmentRef, error) {
	return s.messageProviderAttachments(messageID, "teams:inline:")
}

// ReplaceMessageBeeperAttachments replaces Beeper-managed attachment rows for
// a message (rows whose source_attachment_id carries the "beeper:" prefix).
// Rows with a content hash are downloaded media; rows without one are
// pending-download markers whose storage_path holds the source asset URL, so
// a later retry pass can find and repair them.
func (s *Store) ReplaceMessageBeeperAttachments(messageID int64, refs []AttachmentRef) error {
	return s.replaceMessageProviderAttachments(messageID, "beeper:", refs)
}

// MessageBeeperAttachments returns the message's existing Beeper-managed
// attachment rows keyed by source_attachment_id, so re-persisting a message
// can keep already-downloaded media without re-fetching it.
func (s *Store) MessageBeeperAttachments(messageID int64) (map[string]AttachmentRef, error) {
	return s.messageProviderAttachments(messageID, "beeper:")
}

// ArchivedRawMessage is one archived message paired with the verbatim provider
// payload stored for it, decompressed.
type ArchivedRawMessage struct {
	MessageID      int64
	ConversationID int64
	RawData        []byte
	// BodyText is the currently stored plain-text body, so a caller
	// re-deriving it can skip rows that would not change.
	BodyText string
}

// ScanArchivedRawMessages returns up to limit archived messages for a source
// whose raw payload is in the given format, ordered by message ID and starting
// after afterID. Paging by ID keeps a full-archive walk bounded in memory;
// callers loop until an empty batch comes back.
func (s *Store) ScanArchivedRawMessages(sourceID int64, format string, afterID int64, limit int) ([]ArchivedRawMessage, error) {
	rows, err := s.db.Query(s.Rebind(`
		SELECT m.id, m.conversation_id, r.raw_data, r.compression, COALESCE(b.body_text, '')
		FROM messages m
		JOIN message_raw r ON r.message_id = m.id
		LEFT JOIN message_bodies b ON b.message_id = m.id
		WHERE m.source_id = ? AND r.raw_format = ? AND m.id > ?
		ORDER BY m.id
		LIMIT ?
	`), sourceID, format, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("scan archived raw messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ArchivedRawMessage
	for rows.Next() {
		var item ArchivedRawMessage
		var raw []byte
		var compression sql.NullString
		if err := rows.Scan(&item.MessageID, &item.ConversationID, &raw, &compression, &item.BodyText); err != nil {
			return nil, err
		}
		if compression.Valid && compression.String == "zlib" {
			r, zerr := zlib.NewReader(bytes.NewReader(raw))
			if zerr != nil {
				return nil, fmt.Errorf("zlib reader for message %d: %w", item.MessageID, zerr)
			}
			raw, err = io.ReadAll(r)
			_ = r.Close()
			if err != nil {
				return nil, fmt.Errorf("decompress message %d: %w", item.MessageID, err)
			}
		}
		item.RawData = raw
		out = append(out, item)
	}
	return out, rows.Err()
}

// SetBeeperAttachmentClassification refreshes link-preview metadata and role
// on a message's Beeper-managed attachment rows. Sticker is explicit provider
// evidence and takes precedence over the message-level preview shape.
func (s *Store) SetBeeperAttachmentClassification(
	messageID int64,
	metadata string,
	isPreview bool,
) (int64, error) {
	role := AttachmentRoleStandalone
	if isPreview {
		role = AttachmentRolePreview
	}
	res, err := s.db.Exec(s.Rebind(fmt.Sprintf(`
		UPDATE attachments
		SET attachment_metadata = %s,
		    attachment_role = CASE
		        WHEN attachment_role = 'sticker' THEN attachment_role
		        ELSE ?
		    END,
		    role_source = CASE
		        WHEN attachment_role = 'sticker' THEN role_source
		        ELSE 'importer_semantics'
		    END
		WHERE message_id = ? AND source_attachment_id LIKE 'beeper:%%'
		  AND (
		      %s
		      OR (attachment_role != 'sticker' AND attachment_role != ?)
		      OR (attachment_role != 'sticker' AND role_source != 'importer_semantics')
		  )
	`, s.dialect.JSONBindExpr(), s.dialect.JSONIsDistinctExpr("attachment_metadata"))),
		nullIfEmpty(metadata), string(role), messageID, nullIfEmpty(metadata), string(role))
	if err != nil {
		return 0, fmt.Errorf("set beeper attachment classification: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("set beeper attachment classification: rows affected: %w", err)
	}
	return n, nil
}

// UpdateMessageDerivedText atomically updates the text fields derived from one
// provider payload. A body, snippet, or FTS failure rolls the whole update
// back, so callers can safely retry every derived field together.
func (s *Store) UpdateMessageDerivedText(
	messageID int64, bodyText, bodyHTML, snippet sql.NullString, fts FTSDoc,
) error {
	fts.MessageID = messageID
	return s.withTx(func(tx *loggedTx) error {
		if err := upsertMessageBody(tx, s.dialect, s.fts5Available, messageID, bodyText, bodyHTML); err != nil {
			return fmt.Errorf("update derived message body: %w", err)
		}
		if _, err := tx.Exec(`UPDATE messages SET snippet = ? WHERE id = ?`, snippet, messageID); err != nil {
			return fmt.Errorf("update derived message snippet: %w", err)
		}
		if s.fts5Available {
			if err := s.dialect.FTSUpsert(tx, fts); err != nil {
				return fmt.Errorf("update derived message FTS: %w", err)
			}
		}
		return nil
	})
}

// ArchivedSourceMessageIDs returns the subset of sourceMessageIDs already
// archived for a source. Used to decide whether a page fetched from the
// provider contains anything new without re-persisting it first.
func (s *Store) ArchivedSourceMessageIDs(sourceID int64, sourceMessageIDs []string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	if len(sourceMessageIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(sourceMessageIDs))
	args := make([]any, 0, len(sourceMessageIDs)+1)
	args = append(args, sourceID)
	for i, id := range sourceMessageIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := s.Rebind(`SELECT source_message_id FROM messages WHERE source_id = ? AND source_message_id IN (` +
		strings.Join(placeholders, ",") + `)`)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("look up archived source message IDs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
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

// ListBeeperPendingAttachmentMessages returns the messages of a beeper
// source that still have pending attachment markers, so a retry pass can
// re-fetch their media. Buffered (not a cursor callback): callers do slow
// network and write work per item, which must not hold a read cursor open.
func (s *Store) ListBeeperPendingAttachmentMessages(sourceID int64) ([]BeeperPendingAttachmentMessage, error) {
	return s.listPendingAttachmentMessages(sourceID, "beeper:")
}

// ListSlackRecentReplyThreadRoots returns the source_message_ids of thread
// roots in one conversation that have an archived REPLY sent at or after
// since. The archive is the index Slack does not provide: the maintenance
// rescan repairs recent messages by MESSAGE age, and a recent reply can
// hang under a root far older than any history window — selecting threads
// through the root's age alone leaves such replies unrepairable.
func (s *Store) ListSlackRecentReplyThreadRoots(sourceID, conversationID int64, since time.Time) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT p.source_message_id
		FROM messages p
		WHERE p.source_id = ?
		  AND p.conversation_id = ?
		  AND EXISTS (
		      SELECT 1 FROM messages c
		      WHERE c.reply_to_message_id = p.id AND c.sent_at >= ?
		  )
		ORDER BY p.source_message_id
	`, sourceID, conversationID, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var roots []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		roots = append(roots, id)
	}
	return roots, rows.Err()
}

// ListSlackPendingAttachmentMessages returns the messages of a slack source
// that still have pending attachment markers. Metadata-only link rows
// (media_type "link") are deliberate non-downloads, and hashless
// duplicate-content alias rows resolving to a trusted local CAS path are
// downloaded (see normalizeSlackAttachmentRefs) — neither is pending work.
func (s *Store) ListSlackPendingAttachmentMessages(sourceID int64) ([]PendingAttachmentMessage, error) {
	rows, err := s.db.Query(`
		SELECT m.id, m.source_message_id, c.source_conversation_id,
		       a.storage_path, COALESCE(a.content_hash, ''), COALESCE(a.media_type, ''),
		       COALESCE(a.attachment_state, '')
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN attachments a ON a.message_id = m.id
		WHERE m.source_id = ?
		  AND a.source_attachment_id LIKE ?
		ORDER BY m.id, a.id
	`, sourceID, "slack:%")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var pending []PendingAttachmentMessage
	var current PendingAttachmentMessage
	var haveCurrent, currentPending bool
	flushCurrent := func() {
		if haveCurrent && currentPending {
			pending = append(pending, current)
		}
	}
	for rows.Next() {
		var item PendingAttachmentMessage
		var ref AttachmentRef
		if err := rows.Scan(
			&item.MessageID, &item.SourceMessageID, &item.ChatID,
			&ref.StoragePath, &ref.ContentHash, &ref.MediaType, &ref.State,
		); err != nil {
			return nil, err
		}
		if !haveCurrent || item.MessageID != current.MessageID {
			flushCurrent()
			current = item
			haveCurrent = true
			currentPending = false
		}
		if attachmentpolicy.RetryEligible(ref.State) && ref.MediaType != "link" &&
			ref.ContentHash == "" && !casAttachmentDownloaded(ref) {
			currentPending = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	flushCurrent()
	return pending, nil
}

// ReplaceMessageSlackAttachments replaces Slack-managed attachment rows for
// a message (rows whose source_attachment_id carries the "slack:" prefix).
// Stable Slack file IDs are persisted as source-part keys so every file keeps
// one row even when several files on a message have identical bytes.
func (s *Store) ReplaceMessageSlackAttachments(messageID int64, refs []AttachmentRef) error {
	return s.replaceMessageProviderAttachments(messageID, "slack:", refs)
}

// MessageSlackAttachments returns the message's existing Slack-managed
// attachment rows keyed by source_attachment_id, so re-persisting a message
// can keep already-downloaded media without re-fetching it.
func (s *Store) MessageSlackAttachments(messageID int64) (map[string]AttachmentRef, error) {
	refs, err := s.messageProviderAttachments(messageID, "slack:")
	if err != nil {
		return nil, err
	}
	for sourceAttachmentID, ref := range refs {
		if ref.ContentHash == "" {
			if pathHash, ok := casPathHash(ref.StoragePath); ok {
				ref.ContentHash = pathHash
				refs[sourceAttachmentID] = ref
			}
		}
	}
	return refs, nil
}

// ReplaceMessageLinkAttachments replaces URL-backed attachment rows for a message.
// It intentionally leaves content-addressed local attachment paths (for example
// downloaded inline media) untouched, and preserves Teams-managed inline marker
// rows (source_attachment_id 'teams:inline:%'), whose URL-backed storage_path
// records a durable pending/skipped/failed outcome, not a link attachment.
func (s *Store) ReplaceMessageLinkAttachments(messageID int64, refs []AttachmentRef) error {
	return s.replaceMessageAttachmentsWhere(messageID,
		`(storage_path LIKE 'http://%' OR storage_path LIKE 'https://%')
		 AND COALESCE(source_attachment_id, '') NOT LIKE 'teams:inline:%'`, false, refs)
}
