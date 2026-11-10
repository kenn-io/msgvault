package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/mime"
)

type AutomationRepairOptions struct {
	Apply     bool
	BatchSize int
}
type AutomationRepairSummary struct{ Scanned, Written, Bulk, Automated, Unreadable, Remaining int64 }

// RepairAutomationFlags derives unknown email flags from retained MIME. Each
// keyset batch commits independently, so cancellation retains completed work.
// Written means would-write in a dry run. Missing originals remain unknown.
func (s *Store) RepairAutomationFlags(ctx context.Context, opts AutomationRepairOptions) (summary AutomationRepairSummary, err error) {
	size := opts.BatchSize
	if size == 0 {
		size = 500
	}
	if size < 1 || size > 5000 {
		return summary, errors.New("automation repair batch size must be between 1 and 5000")
	}
	if opts.Apply {
		defer func() {
			// Committed batches need cache invalidation even after cancellation.
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			err = errors.Join(err, s.PublishPendingDerivedEmailRevisionContext(cleanup))
		}()
	}
	type preparedAutomation struct {
		id    int64
		fp    [sha256.Size]byte
		flags uint32
	}
	var after int64
	haveCursor := false
	for {
		if err = ctx.Err(); err != nil {
			return summary, err
		}
		var ids []int64
		query := `SELECT m.id FROM messages m WHERE ` + listIDRepairEmailMessagePredicate + `
 AND (m.automation_flags IS NULL OR m.automation_rule_version IS NULL OR m.automation_rule_version <> ?)`
		args := []any{AutomationRuleVersion}
		// The first page includes legal zero and negative IDs.
		if haveCursor {
			query += ` AND m.id > ?`
			args = append(args, after)
		}
		query += ` ORDER BY m.id LIMIT ?`
		args = append(args, size)
		rows, queryErr := s.db.QueryContext(ctx, query, args...)
		if queryErr != nil {
			return summary, fmt.Errorf("scan automation repair: %w", queryErr)
		}
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		err = errors.Join(err, rows.Err(), rows.Close())
		if err != nil {
			return summary, err
		}
		if len(ids) == 0 {
			break
		}
		var batch AutomationRepairSummary
		var prepared []preparedAutomation
		// Parse one original at a time before taking the batch's writer lock.
		// Retain only its fingerprint and flags for verification before publication.
		for _, id := range ids {
			batch.Scanned++
			var raw []byte
			var compression, format sql.NullString
			var encryption int
			readErr := s.db.QueryRowContext(ctx, `SELECT raw_data, compression, raw_format, COALESCE(encryption_version, 0) FROM message_raw WHERE message_id = ?`, id).Scan(&raw, &compression, &format, &encryption)
			if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
				return summary, fmt.Errorf("read automation original: %w", readErr)
			}
			if errors.Is(readErr, sql.ErrNoRows) || format.String != emailLinkageRawFormat || encryption != 0 {
				batch.Unreadable++
				continue
			}
			fp := rawMIMEFingerprint(raw, compression)
			raw, readErr = decompressArchivedRaw(id, raw, compression)
			if readErr != nil {
				batch.Unreadable++
				continue
			}
			// Nested text/calendar requires a complete MIME parse.
			parsed, parseErr := mime.ParseWithRecovery(raw, "")
			flags := parsed.AutomationFlags()
			if parseErr != nil || flags == nil {
				batch.Unreadable++
				continue
			}
			prepared = append(prepared, preparedAutomation{id, fp, *flags})
		}

		if opts.Apply {
			err = s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
				if err := s.lockListIDRepairSQLiteWriter(ctx, tx); err != nil {
					return err
				}
				for _, item := range prepared {
					if s.IsPostgreSQL() {
						if err := s.lockMeetingEvidenceWith(ctx, tx, item.id); err != nil {
							return err
						}
					}
					// The lock covers snapshot verification and writing, not parsing.
					var raw []byte
					var compression sql.NullString
					readErr := tx.QueryRowContext(ctx, `SELECT raw_data, compression FROM message_raw WHERE message_id = ? AND raw_format = ? AND COALESCE(encryption_version, 0) = 0`+s.dialect.SelectForUpdate(), item.id, emailLinkageRawFormat).Scan(&raw, &compression)
					if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
						return readErr
					}
					if errors.Is(readErr, sql.ErrNoRows) || rawMIMEFingerprint(raw, compression) != item.fp {
						continue // A changed original remains eligible for a later repair.
					}
					result, writeErr := tx.ExecContext(ctx, `UPDATE messages SET automation_flags = ?, automation_rule_version = ? WHERE id = ?
 AND (automation_flags IS NULL OR automation_rule_version IS NULL OR automation_rule_version <> ?)`, item.flags, AutomationRuleVersion, item.id, AutomationRuleVersion)
					if writeErr != nil {
						return writeErr
					}
					n, countErr := result.RowsAffected()
					if countErr != nil {
						return countErr
					}
					batch.Written += n
					if n > 0 {
						if item.flags&BulkMask != 0 {
							batch.Bulk += n
						}
						if item.flags != 0 {
							batch.Automated += n
						}
					}
				}

				if batch.Written > 0 {
					return setArchiveMetadataValueTx(ctx, tx, pendingDerivedEmailRevisionKey, "1")
				}
				return nil
			})
		} else {
			batch.Written = int64(len(prepared))
			for _, item := range prepared {
				if item.flags&BulkMask != 0 {
					batch.Bulk++
				}
				if item.flags != 0 {
					batch.Automated++
				}
			}
		}
		if err != nil {
			return summary, fmt.Errorf("repair automation batch: %w", err)
		}
		summary.Scanned += batch.Scanned
		summary.Written += batch.Written
		summary.Bulk += batch.Bulk
		summary.Automated += batch.Automated
		summary.Unreadable += batch.Unreadable
		after = ids[len(ids)-1]
		haveCursor = true
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages m WHERE `+listIDRepairEmailMessagePredicate+`
 AND (m.automation_flags IS NULL OR m.automation_rule_version IS NULL OR m.automation_rule_version <> ?)`, AutomationRuleVersion).Scan(&summary.Remaining)
	return summary, err
}
