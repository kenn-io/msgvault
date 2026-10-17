package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"

	internalmime "go.kenn.io/msgvault/internal/mime"
)

const emailLinkageRawFormat = "mime"

var errEmailLinkageRawTooLarge = errors.New("email linkage raw exceeds decode limit")

// EmailLinkageRepairSummary counts committed rows in a pending repair pass.
// Incomplete rows retain their extraction outcome and do not block later rows.
type EmailLinkageRepairSummary struct {
	Scanned    int64
	Incomplete int64
}

// refreshEmailLinkageTx reads raw only after the caller has locked messages.
// Persist and raw replacement call it in their existing archive transaction.
func (s *Store) refreshEmailLinkageTx(ctx context.Context, tx *loggedTx, messageID int64) (bool, error) {
	var messageType sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT message_type FROM messages WHERE id = ?`, messageID).Scan(&messageType)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read linkage message: %w", err)
	}
	if !IsEmailMessageType(messageType.String) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM email_link_keys WHERE message_id = ?`, messageID); err != nil {
			return false, err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM email_link_key_state WHERE message_id = ?`, messageID)
		return false, err
	}
	linkage := internalmime.EmailLinkage{Version: internalmime.EmailLinkageVersion, Outcome: "missing_raw"}
	var compressed []byte
	var format string
	var compression sql.NullString
	var encryption sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT raw_data, raw_format, compression, encryption_version FROM message_raw WHERE message_id = ?`, messageID).Scan(&compressed, &format, &compression, &encryption)
	hash := ""
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, fmt.Errorf("read linkage raw: %w", err)
	case encryption.Int64 != 0:
		linkage.Outcome = "encrypted_raw"
	case format != emailLinkageRawFormat:
		linkage.Outcome = "unsupported_raw"
	case compression.Valid && compression.String != "" && compression.String != "none" && compression.String != "zlib":
		linkage.Outcome = "invalid_raw"
	default:
		raw, _, decodeErr := decodeMessageRawBounded(compressed, compression, internalmime.EmailLinkageMaxBytes, errEmailLinkageRawTooLarge)
		switch {
		case errors.Is(decodeErr, errEmailLinkageRawTooLarge):
			linkage.Outcome = "size_limit"
		case decodeErr != nil:
			linkage.Outcome = "invalid_raw"
		default:
			hash = fmt.Sprintf("%x", sha256.Sum256(raw))
			linkage = internalmime.ExtractEmailLinkage(raw)
		}
	}
	// Outcomes without readable raw use a digest of its stored representation.
	// This still distinguishes replacements without interpreting ciphertext.
	if hash == "" && err == nil {
		hash = fmt.Sprintf("%x", sha256.Sum256(compressed))
	}
	var version int
	var oldHash, oldOutcome string
	err = tx.QueryRowContext(ctx, `SELECT extraction_version, outcome, raw_sha256 FROM email_link_key_state WHERE message_id = ?`, messageID).Scan(&version, &oldOutcome, &oldHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil && version == linkage.Version && oldHash == hash && oldOutcome == linkage.Outcome {
		return linkage.Outcome != "complete", nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM email_link_keys WHERE message_id = ?`, messageID); err != nil {
		return false, err
	}
	for _, keys := range []struct {
		kind   string
		values []string
	}{
		{"self", nil}, {"reply", linkage.Replies}, {"reference", linkage.References}, {"attached", linkage.AttachedOriginals},
	} {
		if keys.kind == "self" && linkage.MessageID != "" {
			keys.values = []string{linkage.MessageID}
		}
		for ordinal, value := range keys.values {
			if _, err := tx.ExecContext(ctx, `INSERT INTO email_link_keys (message_id, kind, ordinal, value) VALUES (?, ?, ?, ?)`, messageID, keys.kind, ordinal, value); err != nil {
				return false, fmt.Errorf("insert linkage key: %w", err)
			}
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO email_link_key_state (message_id, extraction_version, outcome, raw_sha256) VALUES (?, ?, ?, ?)
		ON CONFLICT(message_id) DO UPDATE SET extraction_version = excluded.extraction_version, outcome = excluded.outcome, raw_sha256 = excluded.raw_sha256`, messageID, linkage.Version, linkage.Outcome, hash)
	return linkage.Outcome != "complete", err
}

// RepairEmailLinkageContext repairs one source in ascending pages of 500.
// Each committed page advances a durable source high-water mark, so later syncs
// search only newer messages. Extractor version changes and source merges reset it.
func (s *Store) RepairEmailLinkageContext(ctx context.Context, sourceID int64, progress func(EmailLinkageRepairSummary)) (EmailLinkageRepairSummary, error) {
	var summary EmailLinkageRepairSummary
	for {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		var page EmailLinkageRepairSummary
		var last int64
		err := s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{sourceID}}, func(tx *loggedTx) error {
			page, last = EmailLinkageRepairSummary{}, 0
			var after int64
			var checkpointVersion int
			checkpointExists := true
			err := tx.QueryRowContext(ctx, `SELECT extraction_version, last_message_id FROM email_link_key_repair_progress WHERE source_id = ?`, sourceID).Scan(&checkpointVersion, &after)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				checkpointExists = false
				after = 0
				err = nil
			case err != nil:
				return fmt.Errorf("read email linkage repair checkpoint: %w", err)
			case checkpointVersion != internalmime.EmailLinkageVersion:
				after = 0
			}
			var ids []int64
			if err := scanIDsTx(ctx, tx, func(rows *loggedRows) error {
				var id int64
				if err := rows.Scan(&id); err != nil {
					return err
				}
				ids = append(ids, id)
				return nil
			}, `SELECT m.id FROM messages m WHERE m.source_id = ? AND m.id > ? AND COALESCE(m.message_type, '') IN ('', 'email')
			AND NOT EXISTS (SELECT 1 FROM email_link_key_state e WHERE e.message_id = m.id AND e.extraction_version = ?)
			ORDER BY m.id LIMIT 500`, sourceID, after, internalmime.EmailLinkageVersion); err != nil {
				return err
			}
			for _, id := range ids {
				if err := s.lockMeetingEvidenceWith(ctx, tx, id); err != nil {
					return err
				}
				incomplete, err := s.refreshEmailLinkageTx(ctx, tx, id)
				if err != nil {
					return err
				}
				page.Scanned++
				if incomplete {
					page.Incomplete++
				}
				last = id
			}
			if page.Scanned == 0 {
				var maxID int64
				if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM messages WHERE source_id = ?`, sourceID).Scan(&maxID); err != nil {
					return fmt.Errorf("read email linkage repair high-water mark: %w", err)
				}
				if !checkpointExists || checkpointVersion != internalmime.EmailLinkageVersion || maxID > after {
					_, err = tx.ExecContext(ctx, `INSERT INTO email_link_key_repair_progress (source_id, extraction_version, last_message_id) VALUES (?, ?, ?)
						ON CONFLICT(source_id) DO UPDATE SET extraction_version = excluded.extraction_version, last_message_id = excluded.last_message_id`,
						sourceID, internalmime.EmailLinkageVersion, maxID)
				}
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO email_link_key_repair_progress (source_id, extraction_version, last_message_id) VALUES (?, ?, ?)
				ON CONFLICT(source_id) DO UPDATE SET extraction_version = excluded.extraction_version, last_message_id = excluded.last_message_id`,
				sourceID, internalmime.EmailLinkageVersion, last); err != nil {
				return fmt.Errorf("advance email linkage repair checkpoint: %w", err)
			}
			return nil
		})
		if err != nil {
			return summary, fmt.Errorf("repair email linkage for source %d: %w", sourceID, err)
		}
		if page.Scanned == 0 {
			return summary, nil
		}
		summary.Scanned += page.Scanned
		summary.Incomplete += page.Incomplete
		if progress != nil {
			progress(summary)
		}
	}
}

// invalidateEmailLinkageRepairProgressTx restarts the destination's discovery
// after a source merge moves messages below its existing high-water mark.
func invalidateEmailLinkageRepairProgressTx(ctx context.Context, tx *loggedTx, sourceID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM email_link_key_repair_progress WHERE source_id = ?`, sourceID); err != nil {
		return fmt.Errorf("invalidate email linkage repair checkpoint: %w", err)
	}
	return nil
}
