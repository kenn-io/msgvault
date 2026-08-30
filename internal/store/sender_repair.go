package store

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"strings"

	"go.kenn.io/msgvault/internal/mime"
)

const senderRepairMaxHeaderBytes = 256 << 10

var errSenderRepairHeaderTooLarge = errors.New("sender repair MIME header exceeds decode limit")

// MissingSenderCandidate is an archived RFC 5322 message that has neither a
// resolved sender nor a persisted From-recipient snapshot. RawMIME contains
// only the decoded, size-bounded MIME header needed for sender parsing; no
// provider access is required.
type MissingSenderCandidate struct {
	MessageID          int64
	SourceID           int64
	SourceType         string
	RawMIME            []byte
	RawMIMEFingerprint [sha256.Size]byte
	DecodeError        error
}

// ListMissingMIMESendersPageContext returns one ID-ordered page of email-shaped
// messages whose archived MIME can be reparsed to recover sender evidence.
// Non-email payloads and rows with either sender representation already
// populated are deliberately out of scope.
func (s *Store) ListMissingMIMESendersPageContext(
	ctx context.Context,
	afterMessageID int64,
	limit int,
) ([]MissingSenderCandidate, error) {
	if limit <= 0 {
		return nil, errors.New("list missing MIME senders: limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`
		SELECT m.id, m.source_id, s.source_type, mr.raw_data, mr.compression
		FROM messages m
		JOIN sources s ON s.id = m.source_id
		JOIN message_raw mr ON mr.message_id = m.id AND mr.raw_format = 'mime'
		WHERE (m.message_type = 'email' OR m.message_type = '' OR m.message_type IS NULL)
		  AND m.sender_id IS NULL
		  AND NOT EXISTS (
			SELECT 1 FROM message_recipients sender
			WHERE sender.message_id = m.id
			  AND LOWER(sender.recipient_type) = 'from'
		  )
		  AND m.id > ?
		ORDER BY m.id
		LIMIT ?
	`), afterMessageID, limit)
	if err != nil {
		return nil, fmt.Errorf("query missing MIME senders: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var candidates []MissingSenderCandidate
	for rows.Next() {
		var candidate MissingSenderCandidate
		var encoded []byte
		var compression sql.NullString
		if err := rows.Scan(
			&candidate.MessageID,
			&candidate.SourceID,
			&candidate.SourceType,
			&encoded,
			&compression,
		); err != nil {
			return nil, fmt.Errorf("scan missing MIME sender: %w", err)
		}
		candidate.RawMIMEFingerprint = rawMIMEFingerprint(encoded, compression)
		candidate.RawMIME, err = decodeMessageRawHeaderBounded(
			encoded,
			compression,
			senderRepairMaxHeaderBytes,
		)
		if err != nil {
			// Preserve the keyset cursor and count the row as unresolved. A
			// corrupt payload or pathological header must not block every later
			// repair candidate.
			candidate.RawMIME = nil
			candidate.DecodeError = fmt.Errorf(
				"decode message %d MIME header for sender repair: %w",
				candidate.MessageID,
				err,
			)
			candidates = append(candidates, candidate)
			continue
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate missing MIME senders: %w", err)
	}
	return candidates, nil
}

func rawMIMEFingerprint(
	encoded []byte,
	compression sql.NullString,
) [sha256.Size]byte {
	hash := sha256.New()
	if compression.Valid {
		_, _ = hash.Write([]byte{1})
		_, _ = hash.Write([]byte(compression.String))
	} else {
		_, _ = hash.Write([]byte{0})
	}
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(encoded)
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], hash.Sum(nil))
	return fingerprint
}

func decodeMessageRawHeaderBounded(
	encoded []byte,
	compression sql.NullString,
	maxBytes int64,
) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errSenderRepairHeaderTooLarge
	}
	var reader io.ReadCloser
	if compression.Valid && compression.String == "zlib" {
		zlibReader, err := zlib.NewReader(bytes.NewReader(encoded))
		if err != nil {
			return nil, fmt.Errorf("zlib reader: %w", err)
		}
		reader = zlibReader
	} else {
		reader = io.NopCloser(bytes.NewReader(encoded))
	}
	prefix, readErr := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close raw MIME reader: %w", closeErr)
	}
	if end := mimeHeaderEnd(prefix); end > 0 {
		return prefix[:end], nil
	}
	if int64(len(prefix)) > maxBytes {
		return nil, errSenderRepairHeaderTooLarge
	}
	return prefix, nil
}

func mimeHeaderEnd(raw []byte) int {
	if idx := bytes.Index(raw, []byte("\r\n\r\n")); idx >= 0 {
		return idx + len("\r\n\r\n")
	}
	if idx := bytes.Index(raw, []byte("\n\n")); idx >= 0 {
		return idx + len("\n\n")
	}
	return 0
}

// ApplySenderRepairContext atomically installs one recovered From address as
// both the message sender and its immutable envelope snapshot. The conditional
// update is an optimistic guard: any sender evidence written after planning
// aborts the repair instead of being overwritten.
func (s *Store) ApplySenderRepairContext(
	ctx context.Context,
	messageID int64,
	expectedRawMIMEFingerprint [sha256.Size]byte,
	sender mime.Address,
) error {
	email := strings.ToLower(strings.TrimSpace(sender.Email))
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Name != "" || !strings.EqualFold(parsed.Address, email) {
		return fmt.Errorf("repair message %d sender: invalid email address %q", messageID, sender.Email)
	}
	domain := strings.ToLower(strings.TrimSpace(sender.Domain))
	if at := strings.LastIndex(email, "@"); at >= 0 {
		domain = email[at+1:]
	}
	displayName := strings.TrimSpace(sender.Name)

	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if err := s.lockMessageForRecipientWrite(q, messageID); err != nil {
			return err
		}
		if s.senderRepairMessageLockHook != nil {
			s.senderRepairMessageLockHook()
		}
		var encodedRaw []byte
		var compression sql.NullString
		err := q.QueryRow(`
			SELECT raw_data, compression
			FROM message_raw
			WHERE message_id = ? AND raw_format = 'mime'
		`+s.dialect.SelectForUpdate(), messageID).Scan(&encodedRaw, &compression)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("message %d changed after sender repair planning", messageID)
			}
			return fmt.Errorf("lock message %d raw MIME for sender repair: %w", messageID, err)
		}
		if rawMIMEFingerprint(encodedRaw, compression) != expectedRawMIMEFingerprint {
			return fmt.Errorf("message %d changed after sender repair planning", messageID)
		}
		_, err = decodeMessageRawHeaderBounded(
			encodedRaw, compression, senderRepairMaxHeaderBytes,
		)
		if err != nil {
			return fmt.Errorf("recheck message %d raw MIME for sender repair: %w", messageID, err)
		}
		participantInserted := false
		participantID, err := ensureParticipantWith(
			q,
			s.dialect,
			email,
			displayName,
			domain,
			func() error {
				participantInserted = true
				return nil
			},
		)
		if err != nil {
			return fmt.Errorf("ensure repaired sender participant: %w", err)
		}
		if participantInserted {
			if err := s.bumpParticipantDisplayNameRevisionContext(ctx, tx); err != nil {
				return err
			}
		}

		result, err := q.Exec(`
			UPDATE messages
			SET sender_id = ?
			WHERE id = ?
			  AND (message_type = 'email' OR message_type = '' OR message_type IS NULL)
			  AND sender_id IS NULL
			  AND EXISTS (
				SELECT 1 FROM message_raw raw
				WHERE raw.message_id = messages.id AND raw.raw_format = 'mime'
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM message_recipients existing_sender
				WHERE existing_sender.message_id = messages.id
				  AND LOWER(existing_sender.recipient_type) = 'from'
			  )
		`, participantID, messageID)
		if err != nil {
			return fmt.Errorf("update message %d sender: %w", messageID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count message %d sender repair: %w", messageID, err)
		}
		if affected != 1 {
			return fmt.Errorf("message %d changed after sender repair planning", messageID)
		}

		if err := replaceMessageRecipientsTx(q, messageID, RecipientSet{
			Type:           "from",
			ParticipantIDs: []int64{participantID},
			DisplayNames:   []string{displayName},
			EmailAddresses: []string{email},
		}); err != nil {
			return fmt.Errorf("replace message %d From recipient: %w", messageID, err)
		}
		if err := refreshMessageAttributionWith(q, messageID); err != nil {
			return fmt.Errorf("refresh message %d attribution: %w", messageID, err)
		}
		if s.fts5Available {
			if _, err := q.Exec(
				s.dialect.FTSBackfillBatchSQL(), messageID, messageID+1,
			); err != nil {
				return fmt.Errorf("refresh message %d FTS after sender repair: %w", messageID, err)
			}
		}
		if err := s.bumpDerivedDataRevision(tx); err != nil {
			return fmt.Errorf("invalidate derived data after repairing message %d sender: %w", messageID, err)
		}
		return nil
	})
}
