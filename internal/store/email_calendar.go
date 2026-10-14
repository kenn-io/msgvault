package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/mime"
)

// EmailCalendarRepairSummary reports archived email interpretation coverage.
type EmailCalendarRepairSummary struct{ Scanned, Changed, Skipped int }

// Every calendar MIME subtype contains this unbroken ASCII token. Scanning
// only the subtype keeps folding or whitespace around the slash harmless.
// False positives are safe; avoid allocating a lowercase copy of the email.
func hasCalendarToken(raw []byte) bool {
	const token = "calendar"
	for i := 0; i+len(token) <= len(raw); i++ {
		if (raw[i] == 'c' || raw[i] == 'C') && bytes.EqualFold(raw[i:i+len(token)], []byte(token)) {
			return true
		}
	}
	return false
}

func (s *Store) lockEmailCalendarParticipantsWith(
	ctx context.Context, tx *loggedTx, data *MessagePersistData,
) error {
	if data == nil || data.Message == nil || !IsEmailMessageType(data.Message.MessageType) {
		return nil
	}
	var raw []byte
	if len(data.RawMIME) > 0 {
		format := data.RawFormat
		if format == "" {
			format = rawFormatMIME
		}
		if format != rawFormatMIME {
			return nil
		}
		raw = data.RawMIME
	} else {
		var stored []byte
		var format, compression sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT r.raw_data, r.raw_format, r.compression
			FROM messages m JOIN message_raw r ON r.message_id=m.id
			WHERE m.source_id=? AND m.source_message_id=?
		`, data.Message.SourceID, data.Message.SourceMessageID).Scan(&stored, &format, &compression)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && format.String != rawFormatMIME) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read stored email MIME before calendar persistence: %w", err)
		}
		var decodeErr error
		raw, _, decodeErr = decodeMessageRawBounded(stored, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
		if decodeErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		}
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
	q := boundQuerier{ctx: ctx, q: tx}
	var kind, parent string
	var sourceID int64
	if err := q.QueryRow(`SELECT COALESCE(message_type,''),source_id,source_message_id FROM messages WHERE id=?`, id).Scan(&kind, &sourceID, &parent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return summary, 0, nil
		}
		return summary, 0, err
	}
	if !IsEmailMessageType(kind) {
		return summary, 0, nil
	}
	var stored []byte
	var format, compression sql.NullString
	if inMemoryRaw == nil {
		err := q.QueryRow(`SELECT raw_data,raw_format,compression FROM message_raw WHERE message_id=?`, id).Scan(&stored, &format, &compression)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && format.String != rawFormatMIME) {
			return summary, 0, nil
		}
		if err != nil {
			return summary, 0, err
		}
	}
	if tx.attribution == nil || !tx.attribution.holds(sourceID) {
		return summary, 0, errAttributionLockMissing
	}
	var diagnostics []string
	var raw []byte
	var decodeErr error
	if inMemoryRaw != nil {
		raw, _, decodeErr = decodeMessageRawBounded(*inMemoryRaw, sql.NullString{}, maxMeetingRawBytes, errMeetingRawTooLarge)
	} else {
		raw, _, decodeErr = decodeMessageRawBounded(stored, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
	}
	var prepared []preparedICS
	decodedBytes, retainedBytes := 0, 0
	if decodeErr != nil {
		diagnostics = append(diagnostics, "email raw could not be decoded within the 64 MiB limit")
	} else if hasCalendarToken(raw) {
		parsed, err := mime.Parse(raw)
		if err != nil {
			diagnostics = append(diagnostics, "email MIME could not be decoded")
		} else {
			for _, part := range parsed.CalendarParts {
				if err := ctx.Err(); err != nil {
					return summary, 0, err
				}
				decodedBytes += len(part.Content)
				if decodedBytes > calevent.MaxICSRetainedBytes {
					diagnostics = append(diagnostics, "calendar decoded-input budget exceeds 64 MiB")
					prepared = nil
					break
				}
				if part.Error != "" {
					diagnostics = append(diagnostics, part.Error)
					continue
				}
				items, err := s.prepareICalendarWith(ctx, tx, sourceID, part.Content, part.Method, &id, parent, part.PartKey)
				if err != nil {
					// Parser/interpretation errors are coverage, but SQL and cancellation
					// errors must never turn a failed transaction into successful archival.
					if ctx.Err() != nil {
						return summary, 0, ctx.Err()
					}
					var invalid *icsInterpretationError
					if !errors.As(err, &invalid) {
						return summary, 0, err
					}
					var unsupported *calevent.UnsupportedMethodError
					if errors.As(err, &unsupported) && calevent.IsNonActionableMethod(unsupported.Method) {
						continue
					}
					diagnostics = append(diagnostics, invalid.Error())
					continue
				}
				retainedBytes += len(part.Content) * len(items)
				if retainedBytes > calevent.MaxICSRetainedBytes {
					diagnostics = append(diagnostics, "calendar retained-raw budget exceeds 64 MiB")
					prepared = nil
					break
				}
				prepared = append(prepared, items...)
			}
		}
	}
	summary.SourceID = sourceID
	if err := s.persistICalendarWith(ctx, tx, sourceID, prepared, &summary); err != nil {
		return summary, 0, err
	}
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
	var meta map[string]any
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
			meta = make(map[string]any)
		}
		meta["calendar_invite_errors"] = diagnostics
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

// RepairEmailCalendarEventsContext extracts invites from stored MIME without
// a provider connection. Per-email commits make interrupted repair resumable.
func (s *Store) RepairEmailCalendarEventsContext(ctx context.Context, sourceID int64) (EmailCalendarRepairSummary, error) {
	var summary EmailCalendarRepairSummary
	var after int64
	haveCursor := false
	for {
		query := `SELECT id FROM messages WHERE source_id=? AND COALESCE(message_type,'') IN ('','email')`
		args := []any{sourceID}
		if haveCursor {
			query += ` AND id>?`
			args = append(args, after)
		}
		query += ` ORDER BY id LIMIT 100`
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return summary, err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return summary, err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return summary, err
		}
		if closeErr != nil {
			return summary, closeErr
		}
		if len(ids) == 0 {
			return summary, ctx.Err()
		}
		for _, id := range ids {
			err := s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{sourceID}}, func(tx *loggedTx) error {
				if err := s.requireSyncSource(sourceID); err != nil {
					return err
				}
				var currentSource int64
				if err := tx.QueryRowContext(ctx, `SELECT source_id FROM messages WHERE id=?`, id).Scan(&currentSource); errors.Is(err, sql.ErrNoRows) {
					return nil
				} else if err != nil {
					return err
				}
				if currentSource != sourceID {
					return fmt.Errorf("email %d changed source during calendar repair", id)
				}
				sum, skipped, err := s.refreshEmailCalendarEventsWith(ctx, tx, id, nil)
				if err != nil {
					return err
				}
				summary.Scanned++
				summary.Changed += sum.Changed
				summary.Skipped += skipped
				return nil
			})
			if err != nil {
				return summary, err
			}
		}
		after, haveCursor = ids[len(ids)-1], true
	}
}
