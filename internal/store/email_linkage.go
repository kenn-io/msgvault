package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	internalmime "go.kenn.io/msgvault/internal/mime"
)

// Email linkage indexes the identifiers an archived email names in its
// headers and attached originals (email_link_keys). messages.email_linkage_outcome
// IS NULL marks an email whose keys are not derived yet; the repair pass
// fills it in, the way account_path works for account attribution.
const (
	emailLinkageRawFormat = "mime"

	// emailLinkagePendingPredicate selects emails whose keys are still pending.
	// idx_messages_email_linkage_pending repeats it verbatim so the planner
	// matches the repair query to that partial index.
	emailLinkagePendingPredicate = `email_linkage_outcome IS NULL AND COALESCE(message_type, '') IN ('', 'email')`

	emailLinkagePendingPageQuery = `SELECT id FROM messages
	WHERE source_id = ? AND ` + emailLinkagePendingPredicate + ` AND id > ?
	ORDER BY id LIMIT ?`
)

var errEmailLinkageRawTooLarge = errors.New("email linkage raw exceeds decode limit")

// EmailLinkageRepairSummary counts committed rows in a pending repair pass.
// Incomplete rows keep their extraction outcome and do not block later rows.
type EmailLinkageRepairSummary struct {
	Scanned    int64
	Incomplete int64
}

// emailLinkageInput tells refreshEmailLinkageTx where the identifiers come
// from, the way deliveryInput does for attribution. The zero value reads the
// stored raw message.
type emailLinkageInput struct {
	extracted *internalmime.EmailLinkage
	// unsupported means a non-MIME payload now holds the raw message.
	unsupported bool
	// emailChecked means the caller already knows the row is an email, so the
	// refresh skips its own message-type lookup.
	emailChecked bool
}

// emailLinkageForRaw reads the identifiers of a payload a write holds. Only a
// MIME payload has any; a write that stores another format records that.
func emailLinkageForRaw(format string, raw []byte) emailLinkageInput {
	if format != emailLinkageRawFormat {
		return emailLinkageInput{unsupported: true}
	}
	linkage := internalmime.ExtractEmailLinkage(raw)
	return emailLinkageInput{extracted: &linkage}
}

// refreshEmailLinkageTx is the one owner of email_link_keys and
// messages.email_linkage_outcome. It rewrites a message's keys from input and
// reports whether the outcome is incomplete. The caller holds the message row.
func (s *Store) refreshEmailLinkageTx(ctx context.Context, tx *loggedTx, messageID int64, input emailLinkageInput) (bool, error) {
	var err error
	if !input.emailChecked {
		var messageType sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT message_type FROM messages WHERE id = ?`, messageID).Scan(&messageType)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read linkage message: %w", err)
		}
		if !IsEmailMessageType(messageType.String) {
			_, err := tx.ExecContext(ctx, `DELETE FROM email_link_keys WHERE message_id = ?`, messageID)
			return false, err
		}
	}
	linkage := internalmime.EmailLinkage{}
	switch {
	case input.extracted != nil:
		linkage = *input.extracted
	case input.unsupported:
		linkage.Outcome = "unsupported_raw"
	default:
		linkage, err = s.storedEmailLinkage(ctx, tx, messageID)
		if err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM email_link_keys WHERE message_id = ?`, messageID); err != nil {
		return false, err
	}
	for _, keys := range []struct {
		kind   string
		values []string
	}{
		{"reply", linkage.Replies}, {"reference", linkage.References}, {"attached", linkage.AttachedOriginals},
	} {
		for _, value := range keys.values {
			if _, err := tx.ExecContext(ctx, `INSERT INTO email_link_keys (message_id, kind, value) VALUES (?, ?, ?)`,
				messageID, keys.kind, value); err != nil {
				return false, fmt.Errorf("insert linkage key: %w", err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET email_linkage_outcome = ? WHERE id = ?`, linkage.Outcome, messageID); err != nil {
		return false, fmt.Errorf("record linkage outcome: %w", err)
	}
	return linkage.Outcome != "complete", nil
}

// storedEmailLinkage extracts identifiers from the stored raw message.
// Outcomes other than complete say why the raw could not be read. A later raw
// write for the message derives its keys again, which clears missing_raw and
// unsupported_raw.
func (s *Store) storedEmailLinkage(ctx context.Context, tx *loggedTx, messageID int64) (internalmime.EmailLinkage, error) {
	var compressed []byte
	var format string
	var compression sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT raw_data, raw_format, compression FROM message_raw WHERE message_id = ?`, messageID).
		Scan(&compressed, &format, &compression)
	linkage := internalmime.EmailLinkage{}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		linkage.Outcome = "missing_raw"
	case err != nil:
		return linkage, fmt.Errorf("read linkage raw: %w", err)
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
			linkage = internalmime.ExtractEmailLinkage(raw)
		}
	}
	return linkage, nil
}

// RepairEmailLinkageContext derives the keys of every pending email of one
// source, a page per transaction. Committed pages stay derived, so a
// cancelled run resumes where it stopped. A source with nothing pending costs
// one indexed lookup, so every sync runs it.
//
// A change to the extraction rules that changes output must clear
// email_linkage_outcome for the affected rows, or derived rows keep the old
// answer.
func (s *Store) RepairEmailLinkageContext(
	ctx context.Context, sourceID int64, progress func(EmailLinkageRepairSummary),
) (EmailLinkageRepairSummary, error) {
	scanned, incomplete, err := s.derivePagesContext(ctx, sourceID, "derive email linkage",
		func(tx *loggedTx, after int64, limit int) ([]int64, error) {
			var ids []int64
			err := scanIDsTx(ctx, tx, func(rows *loggedRows) error {
				var id int64
				if err := rows.Scan(&id); err != nil {
					return err
				}
				ids = append(ids, id)
				return nil
			}, emailLinkagePendingPageQuery, sourceID, after, limit)
			return ids, err
		},
		func(tx *loggedTx, id int64) (bool, error) {
			if err := s.lockMeetingEvidenceWith(ctx, tx, id); err != nil {
				return false, err
			}
			// The page query selects only emails.
			return s.refreshEmailLinkageTx(ctx, tx, id, emailLinkageInput{emailChecked: true})
		},
		func(scanned, incomplete int64) {
			if progress != nil {
				progress(EmailLinkageRepairSummary{Scanned: scanned, Incomplete: incomplete})
			}
		})
	return EmailLinkageRepairSummary{Scanned: scanned, Incomplete: incomplete}, err
}
