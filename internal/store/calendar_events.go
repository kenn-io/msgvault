package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"time"
)

const messageTypeCalendarEvent = "calendar_event"

// CalendarProjection preserves unknown values in sparse provider tombstones.
type CalendarProjection struct {
	Status   *string `json:"status"`
	Sequence *int64  `json:"sequence"`
	Start    *string `json:"start"`
	End      *string `json:"end"`
	AllDay   *bool   `json:"all_day"`
	TimeZone *string `json:"time_zone"`
	ICalUID  *string `json:"ical_uid"`
}

// ParseCalendarProjection reads the current calendar state from archive metadata.
func ParseCalendarProjection(metadata string) *CalendarProjection {
	projection := &CalendarProjection{}
	if err := json.Unmarshal([]byte(metadata), projection); err != nil {
		return &CalendarProjection{}
	}
	if projection.Start == nil || *projection.Start == "" {
		projection.Start = nil
		projection.AllDay = nil
	}
	return projection
}

// PersistCalendarEventContext atomically stores a complete calendar snapshot or
// merges a sparse cancellation, and classifies its transition under the writer fence.
func (s *Store) PersistCalendarEventContext(ctx context.Context, data *MessagePersistData, changedAt time.Time, cancelled bool) (id int64, inserted bool, err error) {
	if data == nil || data.Message == nil {
		return 0, false, errors.New("persist calendar event requires a message")
	}
	if err = s.requireSyncSource(data.Message.SourceID); err != nil {
		return id, inserted, err
	}
	err = s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{data.Message.SourceID}}, func(tx *loggedTx) error {
		if s.dialect.DriverName() != postgresDriverName {
			if _, e := tx.ExecContext(ctx, `UPDATE embedding_change_clock SET sequence=sequence WHERE singleton=1`); e != nil {
				return e
			}
		}
		var prior sql.NullString
		lookup := `SELECT id, metadata FROM messages WHERE source_id = ? AND source_message_id = ?` + s.dialect.SelectForUpdate()
		e := tx.QueryRowContext(ctx, lookup, data.Message.SourceID, data.Message.SourceMessageID).Scan(&id, &prior)
		found := e == nil
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return fmt.Errorf("read prior calendar state: %w", e)
		}
		old := ParseCalendarProjection(prior.String)
		metadata := "{}"
		if data.Metadata != nil && data.Metadata.Valid {
			metadata = data.Metadata.String
		}
		if cancelled && found {
			// Provider cancellation deltas must not erase the stored scheduled state.
			merged := map[string]any{}
			_ = json.Unmarshal([]byte(prior.String), &merged)
			if merged == nil {
				merged = map[string]any{}
			}
			merged["status"] = "cancelled"
			raw, e := json.Marshal(merged)
			if e != nil {
				return e
			}
			metadata = string(raw)
			if e := setMessageMetadataWith(boundQuerier{ctx: ctx, q: tx}, s.dialect, id, sql.NullString{String: metadata, Valid: true}); e != nil {
				return e
			}
		} else {
			snapshot := *data
			// Canonical archive readiness must not depend on a derived index.
			snapshot.FTS = nil
			message := *data.Message
			message.MessageType = messageTypeCalendarEvent
			snapshot.Message = &message
			if cancelled {
				merged := map[string]any{}
				_ = json.Unmarshal([]byte(metadata), &merged)
				if merged == nil {
					merged = map[string]any{}
				}
				merged["status"] = "cancelled"
				raw, e := json.Marshal(merged)
				if e != nil {
					return e
				}
				metadata = string(raw)
			}
			snapshot.Metadata = &sql.NullString{String: metadata, Valid: true}
			id, e = s.persistMessageWith(ctx, tx, &snapshot, &inserted)
			if e != nil {
				return e
			}
			if data.FTS != nil && s.fts5Available {
				if e := s.indexCalendarEventTx(ctx, tx, id, *data.FTS); e != nil {
					return e
				}
			}
		}
		next := ParseCalendarProjection(metadata)
		// Older snapshots can omit the default-zero iCalendar sequence.
		priorSequence := int64(0)
		if old.Sequence != nil {
			priorSequence = *old.Sequence
		}
		sequenceIncreased := next.Sequence != nil && *next.Sequence > priorSequence
		kind := ""
		nextCancelled := next.Status != nil && *next.Status == "cancelled"
		oldCancelled := old.Status != nil && *old.Status == "cancelled"
		switch {
		case !found && nextCancelled:
			kind = "cancelled"
		case !found:
			kind = "created"
		case nextCancelled && !oldCancelled:
			kind = "cancelled"
		case !reflect.DeepEqual(old.Status, next.Status) || !reflect.DeepEqual(old.Start, next.Start) || !reflect.DeepEqual(old.End, next.End) || !reflect.DeepEqual(old.AllDay, next.AllDay) || !reflect.DeepEqual(old.TimeZone, next.TimeZone):
			kind = "updated"
		case sequenceIncreased:
			kind = "updated"
		}
		if kind == "" || !s.captureMCPEnabled() || s.ingestContext().Mode != IngestLive {
			return nil
		}
		if changedAt.IsZero() {
			changedAt = s.ingestContext().ObservedAt
		}
		if changedAt.IsZero() {
			changedAt = time.Now().UTC()
		}
		var conversation, source int64
		var fromMe bool
		if e := tx.QueryRowContext(ctx, `SELECT conversation_id,source_id,is_from_me FROM messages WHERE id=?`, id).Scan(&conversation, &source, &fromMe); e != nil {
			return e
		}
		raw, e := json.Marshal(map[string]any{"kind": kind, "message_id": strconv.FormatInt(id, 10), "conversation_id": strconv.FormatInt(conversation, 10), "source_id": strconv.FormatInt(source, 10), "from_me": fromMe, "ical_uid": next.ICalUID, "sequence": next.Sequence, "starts_at": next.Start, "all_day": next.AllDay, "changed_at": changedAt.UTC().Format(time.RFC3339Nano)})
		if e != nil {
			return e
		}
		return s.appendMCPEventTx(ctx, tx, MCPEvent{Family: "msgvault.calendar_event_changed", Kind: kind, ScopeKind: "source", ScopeID: source, SourceID: source, ConversationID: conversation, MessageID: id, FromMe: fromMe, OccurredAt: changedAt, Data: raw})
	})
	return id, inserted, err
}

// indexCalendarEventTx preserves the calendar syncer's best-effort index write.
// A PostgreSQL SQL error must be rolled back before the archive transaction can
// append its occurrence and commit the ready canonical snapshot.
func (s *Store) indexCalendarEventTx(ctx context.Context, tx *loggedTx, id int64, doc FTSDoc) error {
	const savepoint = "calendar_event_fts"
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
		return fmt.Errorf("create calendar index savepoint: %w", err)
	}
	doc.MessageID = id
	indexErr := s.dialect.FTSUpsert(boundQuerier{ctx: ctx, q: tx}, doc)
	if indexErr != nil {
		if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); err != nil {
			return fmt.Errorf("rollback calendar index: index: %w; rollback: %w", indexErr, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+savepoint); err != nil {
		return fmt.Errorf("release calendar index savepoint: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if indexErr != nil {
		slog.Warn("upsert calendar event fts failed", "message_id", id, "reason", "index_write_failed")
	}
	return nil
}
