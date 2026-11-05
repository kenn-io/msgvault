package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/mime"
)

const rawFormatMIME = "mime"

const emailCalendarRepairableWhere = `NOT EXISTS (
	SELECT 1 FROM source_merge_archive_only_messages marker WHERE marker.message_id = messages.id
)`

// EmailCalendarRepairSummary reports archived email interpretation coverage.
type EmailCalendarRepairSummary struct{ Scanned, Changed, Skipped int }

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

func (s *Store) refreshEmailCalendarEventsWith(ctx context.Context, tx *loggedTx, id int64, inMemoryRaw *[]byte) (ICalendarImportSummary, int, error) {
	var summary ICalendarImportSummary
	if inMemoryRaw != nil && !hasCalendarToken(*inMemoryRaw) {
		return summary, 0, nil
	}
	q := boundQuerier{ctx: ctx, q: tx}
	var kind string
	var sourceID int64
	if err := q.QueryRow(`SELECT COALESCE(message_type,''),source_id FROM messages WHERE id=? AND `+emailCalendarRepairableWhere, id).Scan(&kind, &sourceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return summary, 0, nil
		}
		return summary, 0, err
	}
	if !IsEmailMessageType(kind) {
		return summary, 0, nil
	}
	raw, err := readEmailCalendarRaw(q, id, inMemoryRaw)
	if err != nil || !hasCalendarToken(raw) {
		return summary, 0, err
	}
	var diagnostics []string
	var prepared []preparedICS
	parsed, err := mime.Parse(raw)
	if err != nil {
		diagnostics = append(diagnostics, "email MIME could not be decoded")
	} else {
		prepared, diagnostics, err = s.prepareEmailCalendarPartsWith(ctx, tx, sourceID, parsed.CalendarParts)
		if err != nil {
			return summary, 0, err
		}
	}
	if err := s.persistICalendarWith(ctx, tx, sourceID, prepared, &summary); err != nil {
		return summary, 0, err
	}
	diagnostics = append(diagnostics, summary.Diagnostics...)
	if err := s.setEmailCalendarDiagnosticsWith(ctx, tx, id, diagnostics); err != nil {
		return summary, 0, err
	}
	return summary, len(diagnostics), nil
}

func (s *Store) setEmailCalendarDiagnosticsWith(ctx context.Context, tx *loggedTx, id int64, diagnostics []string) error {
	q := boundQuerier{ctx: ctx, q: tx}
	var stored sql.NullString
	if err := q.QueryRow(`SELECT metadata FROM messages WHERE id=?`, id).Scan(&stored); err != nil {
		return err
	}
	var meta map[string]jsontext.Value
	if stored.Valid {
		if err := json.Unmarshal([]byte(stored.String), &meta); err != nil {
			//nolint:nilerr // Preserve opaque legacy metadata instead of failing email archival.
			return nil
		}
	}
	if len(diagnostics) == 0 {
		if _, ok := meta["calendar_invite_errors"]; !ok {
			return nil
		}
		delete(meta, "calendar_invite_errors")
	} else {
		if meta == nil {
			meta = make(map[string]jsontext.Value)
		}
		encoded, err := json.Marshal(diagnostics)
		if err != nil {
			return err
		}
		meta["calendar_invite_errors"] = encoded
	}
	raw, err := json.Marshal(meta, json.Deterministic(true))
	if err != nil {
		return err
	}
	if stored.String == string(raw) {
		return nil
	}
	return setMessageMetadataWith(q, s.dialect, id, sql.NullString{String: string(raw), Valid: true})
}

// RepairEmailCalendarEventsContext extracts invites from stored MIME, committing
// each page before reporting progress so interrupted repairs can resume.
func (s *Store) RepairEmailCalendarEventsContext(ctx context.Context, sourceID int64, progress func(string)) (EmailCalendarRepairSummary, error) {
	var summary EmailCalendarRepairSummary
	var after int64
	haveCursor := false
	for {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		var page EmailCalendarRepairSummary
		var ids []int64
		err := s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{sourceID}}, func(tx *loggedTx) error {
			page, ids = EmailCalendarRepairSummary{}, nil
			if err := s.requireSyncSource(sourceID); err != nil {
				return err
			}
			// Earlier emails can write diagnostics before a later invitation
			// resolves participants. Keep the directory ahead of both row locks.
			if err := s.lockParticipantDirectoryMutationTxContext(ctx, tx); err != nil {
				return fmt.Errorf("lock email calendar repair participant directory: %w", err)
			}
			query := `SELECT id FROM messages WHERE source_id=? AND COALESCE(message_type,'') IN ('','email') AND ` + emailCalendarRepairableWhere
			args := []any{sourceID}
			if haveCursor {
				query += ` AND id>?`
				args = append(args, after)
			}
			query += ` ORDER BY id LIMIT 100`
			rows, err := tx.QueryContext(ctx, query, args...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					_ = rows.Close()
					return err
				}
				ids = append(ids, id)
			}
			err = rows.Err()
			closeErr := rows.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			for _, id := range ids {
				sum, skipped, err := s.refreshEmailCalendarEventsWith(ctx, tx, id, nil)
				if err != nil {
					return err
				}
				page.Scanned++
				page.Changed += sum.Changed
				page.Skipped += skipped
			}
			return nil
		})
		if err != nil {
			return summary, err
		}
		if len(ids) == 0 {
			return summary, nil
		}
		summary.Scanned += page.Scanned
		summary.Changed += page.Changed
		summary.Skipped += page.Skipped
		after, haveCursor = ids[len(ids)-1], true
		if progress != nil {
			progress(fmt.Sprintf("calendar invites: %d emails scanned", summary.Scanned))
		}
	}
}

func readEmailCalendarRaw(q querier, id int64, inMemoryRaw *[]byte) ([]byte, error) {
	if inMemoryRaw != nil {
		return *inMemoryRaw, nil
	}
	var stored []byte
	var format, compression sql.NullString
	err := q.QueryRow(`SELECT raw_data,raw_format,compression FROM message_raw WHERE message_id=?`, id).Scan(&stored, &format, &compression)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && format.String != rawFormatMIME) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	raw, _, err := decodeMessageRawBounded(stored, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
	return raw, err
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
