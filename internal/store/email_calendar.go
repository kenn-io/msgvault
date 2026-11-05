package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"go.kenn.io/msgvault/internal/mime"
)

const rawFormatMIME = "mime"

// Match the MIME type without allocating a lowercase copy of the email.
func hasCalendarToken(raw []byte) bool {
	const token = "text/calendar"
	for i := 0; i+len(token) <= len(raw); i++ {
		if (raw[i] == 't' || raw[i] == 'T') && bytes.EqualFold(raw[i:i+len(token)], []byte(token)) {
			return true
		}
	}
	return false
}

func (s *Store) lockEmailCalendarParticipantsWith(
	ctx context.Context, tx *loggedTx, raw []byte, format string,
) error {
	if len(raw) == 0 || (format != "" && format != rawFormatMIME) {
		return nil
	}
	if !hasCalendarToken(raw) {
		return nil
	}
	if err := s.lockParticipantDirectoryMutationTxContext(ctx, tx); err != nil {
		return fmt.Errorf("lock email calendar participant directory: %w", err)
	}
	return nil
}

func (s *Store) refreshEmailCalendarEventsWith(ctx context.Context, tx *loggedTx, id int64, raw []byte) error {
	if !hasCalendarToken(raw) {
		return nil
	}
	q := boundQuerier{ctx: ctx, q: tx}
	var kind string
	var sourceID int64
	if err := q.QueryRow(`SELECT COALESCE(message_type,''),source_id FROM messages WHERE id=?`, id).Scan(&kind, &sourceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if !IsEmailMessageType(kind) {
		return nil
	}
	parsed, err := mime.Parse(raw)
	if err != nil {
		slog.Warn("email MIME could not be decoded for calendar invites", "message_id", id, "error", err)
		return nil
	}
	_, err = s.persistEmailCalendarPartsWith(ctx, tx, id, sourceID, parsed)
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
	prepared, diagnostics, err := s.prepareEmailCalendarPartsWith(ctx, tx, sourceID, parsed.CalendarParts)
	if err != nil {
		return summary, err
	}
	if err := s.persistICalendarWith(ctx, tx, sourceID, prepared, &summary); err != nil {
		return summary, err
	}
	diagnostics = append(diagnostics, summary.Diagnostics...)
	for _, diagnostic := range diagnostics {
		slog.Warn("calendar invite ignored", "message_id", id, "error", diagnostic)
	}
	return summary, nil
}

func (s *Store) prepareEmailCalendarPartsWith(ctx context.Context, tx *loggedTx, sourceID int64, parts []mime.CalendarPart) ([]preparedICS, []string, error) {
	var prepared []preparedICS
	var diagnostics []string
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		items, err := s.prepareICalendarWith(ctx, tx, sourceID, part.Content, part.Method, true)
		if err != nil {
			// Interpretation failures preserve the email; database failures abort archival.
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
