package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strconv"

	"go.kenn.io/msgvault/internal/mime"
)

// Match the MIME type without allocating a lowercase copy of the email.
func hasCalendarToken(raw []byte) bool {
	for _, token := range []string{"text/calendar", "application/ics", "application/octet-stream"} {
		for i := 0; i+len(token) <= len(raw); i++ {
			if (raw[i] == token[0] || raw[i] == token[0]-'a'+'A') && bytes.EqualFold(raw[i:i+len(token)], []byte(token)) {
				return true
			}
		}
	}
	return false
}

func (s *Store) deriveEmailCalendarAfterCommit(ctx context.Context, id int64, raw []byte) {
	parsed, err := mime.Parse(raw)
	if err == nil {
		err = s.refreshCommittedEmailCalendar(ctx, id, raw, parsed)
	}
	if err != nil {
		slog.Warn("derive email calendar after commit failed", "message_id", id, "error", err)
	}
}

func (s *Store) refreshCommittedEmailCalendar(ctx context.Context, id int64, raw []byte, parsed *mime.Message) error {
	var sourceID int64
	if err := s.db.QueryRowContext(ctx, `SELECT source_id FROM messages WHERE id=?`, id).Scan(&sourceID); err != nil {
		return err
	}
	item, err := s.ScanArchivedEmailContext(ctx, sourceID, id-1, id)
	if err != nil {
		return err
	}
	if item == nil {
		return nil
	}
	if item.RawDecodeError != nil {
		return item.RawDecodeError
	}
	if !bytes.Equal(item.RawData, raw) {
		return nil
	}
	return s.refreshAndSettleEmailCalendar(ctx, sourceID, item, parsed)
}

// refreshAndSettleEmailCalendar derives the invitation and then releases the
// repair hold the message's write took.
func (s *Store) refreshAndSettleEmailCalendar(ctx context.Context, sourceID int64, item *ArchivedRawMessage, parsed *mime.Message) error {
	if _, err := s.RefreshArchivedEmailCalendarContext(ctx, sourceID, item, parsed); err != nil {
		return err
	}
	return s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{sourceID}}, func(tx *loggedTx) error {
		if err := s.lockArchivedEmailTextContext(ctx, tx, item); err != nil {
			return err
		}
		return settleEmailRepair(boundQuerier{ctx: ctx, q: tx}, item.MessageID)
	})
}

// upsertEmailRaw stores raw data and, for MIME that may hold an invitation,
// reopens the email repair pass in the same transaction.
func upsertEmailRaw(q querier, postgres bool, messageID int64, rawData []byte, format string) error {
	if err := upsertMessageRawWithFormat(q, messageID, rawData, format); err != nil || format != emailLinkageRawFormat || !hasCalendarToken(rawData) {
		return err
	}
	return holdEmailRepair(q, postgres, messageID)
}

// EmailRepairVersion is the ledger version suffix of the email repair pass
// (rederive.LedgerKey), which the store's reopen markers extend.
const EmailRepairVersion = "email-v3"

// emailHoldSuffix extends the pass key to name one message's hold marker.
const emailHoldSuffix = "#o"

// emailRepairSourceSQL yields the source's ledger prefix, up to the version,
// of the message bound to its single placeholder.
const emailRepairSourceSQL = `SELECT 'rederive:' || source_type || ':' || identifier || ':' AS prefix
	FROM sources WHERE id = (SELECT source_id FROM messages WHERE id = ?)`

// holdEmailRepair reopens the repair pass and records, in the same transaction,
// one marker per message whose invitation derive is still owed. The markers
// extend the pass key with a colon-free suffix, so a restarted walk (which
// covers every message) and a source merge or removal clear them with its rows.
func holdEmailRepair(q querier, postgres bool, messageID int64) error {
	if err := serializeEmailRepairHold(q, postgres, messageID); err != nil {
		return err
	}
	_, err := q.Exec(`INSERT INTO applied_migrations (name) SELECT prefix || '`+EmailRepairVersion+emailHoldSuffix+`' || ?
		FROM (`+emailRepairSourceSQL+`) repair_source WHERE true ON CONFLICT (name) DO NOTHING`,
		strconv.FormatInt(messageID, 16), messageID)
	return err
}

// serializeEmailRepairHold reopens the pass, then on PostgreSQL takes the
// pass's advisory lock, which the pass's completion also takes after recording
// itself, and reopens it again. A completion that committed while the lock was
// awaited is then visible to the second reopen, though its ledger row was not
// when the hold began. The lock follows the first reopen so the two never wait
// on each other's row and lock.
func serializeEmailRepairHold(q querier, postgres bool, messageID int64) error {
	if err := reopenEmailRepair(q, messageID); err != nil || !postgres {
		return err
	}
	if _, err := q.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(prefix || '`+EmailRepairVersion+`', 0))
		FROM (`+emailRepairSourceSQL+`) repair_source`, messageID); err != nil {
		return err
	}
	return reopenEmailRepair(q, messageID)
}

// settleEmailRepair drops the message's marker after its invitation derive
// succeeded and, when no other message still owes one, raises the versions
// reopenEmailRepair lowered. Callers hold the source's attribution lock, which
// serializes it with every writer that reopens the pass, so a marker another
// writer committed is always seen; a failed or unfinished derive keeps its
// marker and with it the reopened pass.
func settleEmailRepair(q querier, messageID int64) error {
	const markerPrefix = `prefix || '` + EmailRepairVersion + emailHoldSuffix + `'`
	if _, err := q.Exec(`DELETE FROM applied_migrations WHERE name IN (
		SELECT `+markerPrefix+` || ? FROM (`+emailRepairSourceSQL+`) repair_source)`,
		strconv.FormatInt(messageID, 16), messageID); err != nil {
		return err
	}
	_, err := q.Exec(`UPDATE applied_migrations SET version = 1 WHERE version = 0 AND EXISTS (
		SELECT 1 FROM (`+emailRepairSourceSQL+`) repair_source
		WHERE SUBSTR(name, 1, LENGTH(prefix) + 7) = prefix || 'email-v'
		  AND SUBSTR(name, LENGTH(prefix) + 8) NOT LIKE '%:%'
		  AND NOT EXISTS (SELECT 1 FROM applied_migrations marker
			WHERE SUBSTR(marker.name, 1, LENGTH(`+markerPrefix+`)) = `+markerPrefix+`))`, messageID)
	return err
}

// EmailRepairHeldContext reports whether any message still owes an invitation
// derive to the pass recorded under ledger key base, whether or not the
// reopened versions were raised since.
func (s *Store) EmailRepairHeldContext(ctx context.Context, base string) (bool, error) {
	return emailRepairHeld(ctx, s.db, base)
}

func emailRepairHeld(ctx context.Context, q contextStatementQuerier, base string) (bool, error) {
	var held bool
	prefix := base + emailHoldSuffix
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM applied_migrations
		WHERE SUBSTR(name, 1, LENGTH(?)) = ?)`, prefix, prefix).Scan(&held)
	return held, err
}

// markMigrationCompleteContext records a finished pass and then takes it back
// when a message still owes an invitation derive. The hold check follows the
// record and the pass's advisory lock, which holdEmailRepair also takes, so a
// writer that reopens the pass either committed its marker before the check or
// reopens the pass after this transaction, even when the pass had no ledger row;
// either way the pass is not left recorded over an owed derive.
func (s *Store) markMigrationCompleteContext(ctx context.Context, tx contextStatementQuerier, name string) error {
	if err := s.markMigrationAppliedContext(ctx, tx, name, 1); err != nil {
		return err
	}
	return s.reopenHeldEmailRepair(ctx, tx, name)
}

func (s *Store) reopenHeldEmailRepair(ctx context.Context, tx contextStatementQuerier, name string) error {
	if s.IsPostgreSQL() {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`, name); err != nil {
			return err
		}
	}
	if held, err := emailRepairHeld(ctx, tx, name); err != nil || !held {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE applied_migrations SET version = 0 WHERE name = ?`, name)
	return err
}

// reopenEmailRepair marks the message's source's email repair pass incomplete
// in the transaction that stores the message, so a failed invitation write
// after the commit still leaves the pass stale for the next sync. It lowers the
// ledger version of the pass and its walk state, which restart the walk from
// the first message, instead of deleting rows. The pass raises them on completion.
func reopenEmailRepair(q querier, messageID int64) error {
	_, err := q.Exec(`UPDATE applied_migrations SET version = 0 WHERE EXISTS (
		SELECT 1 FROM (
			SELECT 'rederive:' || source_type || ':' || identifier || ':email-v' AS prefix
			FROM sources WHERE id = (SELECT source_id FROM messages WHERE id = ?)
		) repair_source
		WHERE SUBSTR(name, 1, LENGTH(prefix)) = prefix
		  AND SUBSTR(name, LENGTH(prefix) + 1) NOT LIKE '%:%'
	)`, messageID)
	return err
}

// RefreshArchivedEmailCalendarContext derives invites from the MIME already
// parsed by email re-derivation, rejecting evidence changed since its scan.
func (s *Store) RefreshArchivedEmailCalendarContext(ctx context.Context, sourceID int64, item *ArchivedRawMessage, parsed *mime.Message) (bool, error) {
	if len(parsed.CalendarParts) == 0 {
		return false, nil
	}
	var summary ICalendarImportSummary
	err := s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{sourceID}}, func(tx *loggedTx) error {
		if err := s.requireSyncSource(sourceID); err != nil {
			return err
		}
		if err := s.lockParticipantDirectoryMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := s.lockArchivedEmailTextContext(ctx, tx, item); err != nil {
			return err
		}
		var kind string
		q := boundQuerier{ctx: ctx, q: tx}
		if err := q.QueryRow(`SELECT COALESCE(message_type,'') FROM messages WHERE id=? AND source_id=?`, item.MessageID, sourceID).Scan(&kind); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if !IsEmailMessageType(kind) {
			return nil
		}
		var err error
		summary, err = s.persistEmailCalendarPartsWith(ctx, tx, item.MessageID, sourceID, parsed)
		if err == nil && summary.Changed > 0 {
			err = setArchiveMetadataValueTx(ctx, tx, pendingDerivedEmailRevisionKey, "1")
		}
		return err
	})
	return summary.Changed > 0, err
}

func (s *Store) persistEmailCalendarPartsWith(ctx context.Context, tx *loggedTx, id, sourceID int64, parsed *mime.Message) (ICalendarImportSummary, error) {
	var summary ICalendarImportSummary
	prepared, diagnostics, err := prepareEmailCalendarParts(ctx, parsed.CalendarParts)
	if err != nil {
		return summary, err
	}
	if err := s.persistICalendarWith(ctx, tx, sourceID, prepared, &summary); err != nil {
		return summary, err
	}
	for _, diagnostic := range diagnostics {
		slog.Warn("calendar invite ignored", "message_id", id, "error", diagnostic)
	}
	return summary, nil
}

func prepareEmailCalendarParts(ctx context.Context, parts []mime.CalendarPart) ([]preparedICS, []string, error) {
	var prepared []preparedICS
	var diagnostics []string
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		items, err := prepareICalendar(part.Content, part.Method, true)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			var invalid *icsInterpretationError
			if !errors.As(err, &invalid) {
				return nil, nil, err
			}
			diagnostics = append(diagnostics, invalid.Error())
			continue
		}
		prepared = append(prepared, items...)
	}
	return prepared, diagnostics, nil
}
