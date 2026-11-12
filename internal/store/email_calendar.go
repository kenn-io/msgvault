package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"

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
	_, err = s.RefreshArchivedEmailCalendarContext(ctx, sourceID, item, parsed)
	return err
}

// upsertEmailRaw records an invitation derive in the raw write transaction.
func upsertEmailRaw(q querier, messageID int64, rawData []byte, format string) error {
	if err := upsertMessageRawWithFormat(q, messageID, rawData, format); err != nil || format != emailLinkageRawFormat || !hasCalendarToken(rawData) {
		return err
	}
	_, err := q.Exec(`INSERT INTO email_calendar_pending (message_id) VALUES (?) ON CONFLICT (message_id) DO NOTHING`, messageID)
	return err
}

// RetryPendingEmailCalendarsContext retries invitation derives left by raw writes.
func (s *Store) RetryPendingEmailCalendarsContext(ctx context.Context, sourceID int64) error {
	var after int64
	for {
		var id int64
		err := s.db.QueryRowContext(ctx, `SELECT p.message_id FROM email_calendar_pending p
			JOIN messages m ON m.id = p.message_id WHERE m.source_id = ? AND p.message_id > ?
			ORDER BY p.message_id LIMIT 1`, sourceID, after).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		after = id
		item, err := s.ScanArchivedEmailContext(ctx, sourceID, id-1, id)
		if err != nil {
			return err
		}
		if item == nil {
			// A concurrent MIME write must not lose its pending flag.
			err = s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{sourceID}}, func(tx *loggedTx) error {
				if err := s.lockMeetingEvidenceWith(ctx, tx, id); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, `DELETE FROM email_calendar_pending WHERE message_id = ?
					AND EXISTS (SELECT 1 FROM messages m JOIN message_raw r ON r.message_id = m.id
					WHERE m.id = ? AND m.source_id = ? AND r.raw_format <> 'mime')`, id, id, sourceID)
				return err
			})
			if err != nil {
				return err
			}
			continue
		}
		parsed, parseErr := mime.Parse(item.RawData)
		if item.RawDecodeError != nil {
			parseErr = item.RawDecodeError
		}
		if parseErr != nil {
			slog.Warn("cannot parse pending email calendar", "message_id", id, "error", parseErr)
			parsed = &mime.Message{}
		}
		if _, err := s.RefreshArchivedEmailCalendarContext(ctx, sourceID, item, parsed); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("retry email calendar failed", "message_id", id, "error", err)
		}
	}
	return s.PublishPendingDerivedEmailRevisionContext(ctx)
}

// RefreshArchivedEmailCalendarContext derives invites from the MIME already
// parsed by email re-derivation, rejecting evidence changed since its scan.
func (s *Store) RefreshArchivedEmailCalendarContext(ctx context.Context, sourceID int64, item *ArchivedRawMessage, parsed *mime.Message) (bool, error) {
	if len(parsed.CalendarParts) == 0 {
		var pending bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM email_calendar_pending WHERE message_id = ?)`, item.MessageID).Scan(&pending); err != nil || !pending {
			return false, err
		}
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
		if IsEmailMessageType(kind) && len(parsed.CalendarParts) > 0 {
			var err error
			summary, err = s.persistEmailCalendarPartsWith(ctx, tx, item.MessageID, sourceID, parsed)
			if err != nil {
				return err
			}
			if summary.Changed > 0 {
				if err := setArchiveMetadataValueTx(ctx, tx, pendingDerivedEmailRevisionKey, "1"); err != nil {
					return err
				}
			}
		}
		_, err := q.Exec(`DELETE FROM email_calendar_pending WHERE message_id = ?`, item.MessageID)
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
	for i := range prepared {
		prepared[i].sender = parsed.GetFirstFrom().Email
	}
	if err := s.persistICalendarWith(ctx, tx, sourceID, prepared, &summary); err != nil {
		return summary, err
	}
	for _, diagnostic := range diagnostics {
		slog.Warn("calendar invite ignored", "message_id", id, "error", diagnostic)
	}
	return summary, nil
}

func prepareEmailCalendarParts(ctx context.Context, parts []mime.CalendarPart) ([]PreparedICS, []string, error) {
	var prepared []PreparedICS
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
