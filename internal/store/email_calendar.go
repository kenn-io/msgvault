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
	if err == nil && len(parsed.CalendarParts) == 0 {
		return
	}
	if err == nil {
		var sourceID int64
		err = s.db.QueryRowContext(ctx, `SELECT source_id FROM messages WHERE id=?`, id).Scan(&sourceID)
		if err == nil {
			var item *ArchivedRawMessage
			item, err = s.ScanArchivedEmailContext(ctx, sourceID, id-1, id)
			if err == nil && item != nil && item.RawDecodeError != nil {
				err = item.RawDecodeError
			}
			if err == nil && item != nil && bytes.Equal(item.RawData, raw) {
				_, err = s.RefreshArchivedEmailCalendarContext(ctx, sourceID, item, parsed)
			}
		}
	}
	if err != nil {
		slog.Warn("derive email calendar after commit failed", "message_id", id, "error", err)
	}
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
