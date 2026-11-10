package store

import (
	"context"
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
func (s *Store) RepairAutomationFlags(ctx context.Context, opts AutomationRepairOptions, progress func(AutomationRepairSummary)) (summary AutomationRepairSummary, err error) {
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
		process := func(q contextStatementQuerier) error {
			for _, id := range ids {
				batch.Scanned++
				var raw []byte
				var compression sql.NullString
				var format sql.NullString
				var encryption int
				// One original at a time bounds memory independently of batch size. Lock
				// the snapshot through the write; a later raw change invalidates it again.
				query := `SELECT raw_data, compression, raw_format, COALESCE(encryption_version, 0) FROM message_raw WHERE message_id = ?`
				if opts.Apply {
					query += s.dialect.SelectForUpdate()
				}
				readErr := q.QueryRowContext(ctx, query, id).Scan(&raw, &compression, &format, &encryption)
				if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
					return readErr
				}
				if errors.Is(readErr, sql.ErrNoRows) || format.String != "mime" || encryption != 0 {
					batch.Unreadable++
					continue
				}
				raw, readErr = decompressArchivedRaw(id, raw, compression)
				if readErr != nil {
					batch.Unreadable++
					continue
				}
				// MIME parts can carry text/calendar below a multipart header. A header-only
				// parse would publish a false known-human flag set for those messages.
				parsed, parseErr := mime.ParseWithRecovery(raw, "")
				if parseErr != nil || parsed.Incomplete {
					batch.Unreadable++
					continue
				}
				flags := parsed.AutomationFlags()
				if flags&BulkMask != 0 {
					batch.Bulk++
				}
				if flags != 0 {
					batch.Automated++
				}
				if opts.Apply {
					result, writeErr := q.ExecContext(ctx, `UPDATE messages SET automation_flags = ?, automation_rule_version = ? WHERE id = ?
 AND (automation_flags IS NULL OR automation_rule_version IS NULL OR automation_rule_version <> ?)`, flags, AutomationRuleVersion, id, AutomationRuleVersion)
					if writeErr != nil {
						return writeErr
					}
					n, countErr := result.RowsAffected()
					if countErr != nil {
						return countErr
					}
					batch.Written += n
				} else {
					batch.Written++
				}
			}
			return nil
		}
		if opts.Apply {
			err = s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
				if err := s.lockListIDRepairSQLiteWriter(ctx, tx); err != nil {
					return err
				}
				if s.IsPostgreSQL() {
					for _, id := range ids {
						if err := s.lockMeetingEvidenceWith(ctx, tx, id); err != nil {
							return err
						}
					}
				}
				if err := process(tx); err != nil {
					return err
				}
				if batch.Written > 0 {
					return setArchiveMetadataValueTx(ctx, tx, pendingDerivedEmailRevisionKey, "1")
				}
				return nil
			})
		} else {
			err = process(s.db)
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
		if progress != nil {
			progress(summary)
		}
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages m WHERE `+listIDRepairEmailMessagePredicate+`
 AND (m.automation_flags IS NULL OR m.automation_rule_version IS NULL OR m.automation_rule_version <> ?)`, AutomationRuleVersion).Scan(&summary.Remaining)
	return summary, err
}
