package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
)

const calendarEventMessageType = "calendar_event"

// ReconcileCalendarEventSourceMessageIDContext adopts recurring exceptions
// that were archived under event.id before their original start could be
// resolved. If the canonical recurrence key already exists, its newer
// complete snapshot replaces the legacy row's content while the archive
// message ID is retained. Incomplete rows and sparse cancellation tombstones
// do not replace that content.
func (s *Store) ReconcileCalendarEventSourceMessageIDContext(
	ctx context.Context,
	sourceID int64,
	legacySourceMessageID string,
	newSourceMessageID string,
) error {
	if legacySourceMessageID == "" || newSourceMessageID == "" || legacySourceMessageID == newSourceMessageID {
		return nil
	}
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	return s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{sourceID}}, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		legacyID, legacyType, err := calendarSourceMessageTx(ctx, tx, sourceID, legacySourceMessageID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find legacy calendar event: %w", err)
		}
		if legacyType != calendarEventMessageType {
			return nil
		}
		if err := s.requireSyncMessageSourceTx(q, legacyID); err != nil {
			return err
		}

		duplicateID, duplicateType, err := calendarSourceMessageTx(ctx, tx, sourceID, newSourceMessageID)
		if errors.Is(err, sql.ErrNoRows) {
			_, err := tx.ExecContext(ctx, `UPDATE messages
				SET source_message_id = ?, deleted_from_source_at = NULL
				WHERE id = ? AND source_id = ? AND source_message_id = ?`,
				newSourceMessageID, legacyID, sourceID, legacySourceMessageID)
			if err != nil {
				return fmt.Errorf("adopt legacy calendar event identity: %w", err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("find canonical calendar event: %w", err)
		}
		if duplicateType != calendarEventMessageType {
			return fmt.Errorf("new calendar event identity %q belongs to message type %q", newSourceMessageID, duplicateType)
		}
		if err := s.requireSyncMessageSourceTx(q, duplicateID); err != nil {
			return err
		}

		lockedIDs := []int64{legacyID, duplicateID}
		slices.Sort(lockedIDs)
		for _, id := range lockedIDs {
			if err := s.lockMeetingEvidenceWith(ctx, tx, id); err != nil {
				return fmt.Errorf("lock calendar event %d snapshot: %w", id, err)
			}
		}
		completeSnapshot, err := completeCalendarEventSnapshotTx(ctx, tx, duplicateID)
		if err != nil {
			return err
		}
		preserveLegacySnapshot := !completeSnapshot

		if s.fts5Available {
			if err := s.dialect.InvalidateFTSForMessage(q, legacyID); err != nil {
				return fmt.Errorf("invalidate legacy calendar event search document: %w", err)
			}
			if err := s.dialect.InvalidateFTSForMessage(q, duplicateID); err != nil {
				return fmt.Errorf("invalidate duplicate calendar event search document: %w", err)
			}
		}
		if !preserveLegacySnapshot {
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET
				conversation_id = (SELECT conversation_id FROM messages WHERE id = ?),
				rfc822_message_id = (SELECT rfc822_message_id FROM messages WHERE id = ?),
				list_id = (SELECT list_id FROM messages WHERE id = ?),
				message_type = (SELECT message_type FROM messages WHERE id = ?),
				sent_at = (SELECT sent_at FROM messages WHERE id = ?),
				received_at = (SELECT received_at FROM messages WHERE id = ?),
				internal_date = (SELECT internal_date FROM messages WHERE id = ?),
				sender_id = (SELECT sender_id FROM messages WHERE id = ?),
				reply_to_message_id = (SELECT reply_to_message_id FROM messages WHERE id = ?),
				is_from_me = (SELECT is_from_me FROM messages WHERE id = ?),
				source_is_from_me = (SELECT source_is_from_me FROM messages WHERE id = ?),
				identity_is_from_me = (SELECT identity_is_from_me FROM messages WHERE id = ?),
				subject = (SELECT subject FROM messages WHERE id = ?),
				snippet = (SELECT snippet FROM messages WHERE id = ?),
				size_estimate = (SELECT size_estimate FROM messages WHERE id = ?),
				has_attachments = (SELECT has_attachments FROM messages WHERE id = ?),
				attachment_count = (SELECT attachment_count FROM messages WHERE id = ?),
				metadata = (SELECT metadata FROM messages WHERE id = ?),
				account_address = NULL,
				account_path = NULL,
				embed_gen = NULL
			WHERE id = ?`,
				duplicateID, duplicateID, duplicateID, duplicateID, duplicateID, duplicateID,
				duplicateID, duplicateID, duplicateID, duplicateID, duplicateID, duplicateID,
				duplicateID, duplicateID, duplicateID, duplicateID, duplicateID, duplicateID,
				legacyID); err != nil {
				return fmt.Errorf("copy newer calendar event fields: %w", err)
			}
			if err := copyCalendarEventBodyTx(ctx, tx, legacyID, duplicateID); err != nil {
				return err
			}
			if err := copyCalendarEventRawTx(ctx, tx, legacyID, duplicateID); err != nil {
				return err
			}
			if err := copyCalendarEventRecipientsTx(ctx, tx, legacyID, duplicateID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_labels(message_id, label_id)
			SELECT ?, label_id FROM message_labels WHERE message_id = ?
			ON CONFLICT (message_id, label_id) DO NOTHING`, legacyID, duplicateID); err != nil {
			return fmt.Errorf("preserve duplicate calendar event labels: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO reactions(message_id, participant_id, reaction_type, reaction_value, created_at, removed_at)
			SELECT ?, participant_id, reaction_type, reaction_value, created_at, removed_at FROM reactions WHERE message_id = ?
			ON CONFLICT (message_id, participant_id, reaction_type, reaction_value) DO NOTHING`, legacyID, duplicateID); err != nil {
			return fmt.Errorf("preserve duplicate calendar event reactions: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET reply_to_message_id = ?
			WHERE id <> ? AND reply_to_message_id = ?`, legacyID, legacyID, duplicateID); err != nil {
			return fmt.Errorf("repoint duplicate calendar event replies: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ? AND source_id = ? AND source_message_id = ?`,
			duplicateID, sourceID, newSourceMessageID); err != nil {
			return fmt.Errorf("delete duplicate calendar event: %w", err)
		}
		result, err := tx.ExecContext(ctx, `UPDATE messages
			SET source_message_id = ?, deleted_from_source_at = NULL
			WHERE id = ? AND source_id = ? AND source_message_id = ?`,
			newSourceMessageID, legacyID, sourceID, legacySourceMessageID)
		if err != nil {
			return fmt.Errorf("adopt canonical calendar event identity: %w", err)
		}
		if affected, err := result.RowsAffected(); err != nil {
			return fmt.Errorf("read adopted calendar event count: %w", err)
		} else if affected != 1 {
			return fmt.Errorf("adopt canonical calendar event identity: retained message %d changed concurrently", legacyID)
		}
		if err := refreshMessageAttributionWith(q, legacyID); err != nil {
			return err
		}
		if _, err := s.refreshAccountAttributionTx(ctx, tx, legacyID, deliveryInput{}); err != nil {
			return fmt.Errorf("refresh calendar event account attribution: %w", err)
		}
		if err := s.refreshMeetingProjectionWith(ctx, tx, legacyID); err != nil {
			return err
		}
		if err := s.refreshCalendarEventProjectionWith(ctx, tx, legacyID); err != nil {
			return err
		}
		if s.fts5Available {
			doc, err := calendarEventFTSDocTx(ctx, tx, legacyID)
			if err != nil {
				return err
			}
			if err := s.bestEffortCalendarEventFTSUpsert(ctx, tx, q, doc); err != nil {
				return fmt.Errorf("index reconciled calendar event: %w", err)
			}
		}
		return nil
	})
}

// completeCalendarEventSnapshotTx checks the provider metadata, body row, and
// raw Calendar JSON that ingestEvent writes separately. Empty event bodies
// are valid, so the body row's presence is the check. The raw bounds must agree
// with metadata because a partial update can leave either snapshot stale.
func completeCalendarEventSnapshotTx(ctx context.Context, tx *loggedTx, messageID int64) (bool, error) {
	var metadata sql.NullString
	var hasBody bool
	var storedRaw []byte
	var compression sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT m.metadata,
		EXISTS(SELECT 1 FROM message_bodies WHERE message_id = m.id),
		r.raw_data, r.compression
		FROM messages m LEFT JOIN message_raw r ON r.message_id = m.id
			AND r.raw_format = ? AND length(r.raw_data) > 0
		WHERE m.id = ?`, gcalmodel.RawFormat, messageID).Scan(&metadata, &hasBody, &storedRaw, &compression)
	if err != nil {
		return false, fmt.Errorf("read duplicate calendar event snapshot completeness: %w", err)
	}
	if !metadata.Valid || metadata.String == "" || !hasBody || len(storedRaw) == 0 {
		return false, nil
	}
	raw, _, err := decodeMessageRawBounded(storedRaw, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
	if err != nil {
		return false, nil //nolint:nilerr // Unreadable raw data cannot replace retained event evidence.
	}
	return calendarEventSnapshotIsReplaceable(metadata.String, raw), nil
}

// calendarEventSnapshotIsReplaceable reports whether metadata and the decoded
// provider snapshot describe the same complete bounds. Cancellation status is
// deliberately ignored: it can be merged into metadata without replacing raw
// event content.
func calendarEventSnapshotIsReplaceable(metadata string, raw []byte) bool {
	var fields struct {
		AllDay bool   `json:"all_day"`
		Start  string `json:"start"`
		End    string `json:"end"`
	}
	if json.Unmarshal([]byte(metadata), &fields) != nil || fields.Start == "" || fields.End == "" {
		return false
	}
	event, err := gcalmodel.DecodeEvent(raw)
	if err != nil || event.ID == "" || !validCalendarEventBound(event.Start) || !validCalendarEventBound(event.End) {
		return false
	}
	if event.Start.IsAllDay() != event.End.IsAllDay() || fields.AllDay != event.Start.IsAllDay() {
		return false
	}
	return fields.Start == calendarEventBoundString(event.Start) && fields.End == calendarEventBoundString(event.End)
}

func validCalendarEventBound(bound gcalmodel.EventDateTime) bool {
	if bound.Date != "" {
		if !bound.DateTime.IsZero() {
			return false
		}
		_, err := time.Parse(time.DateOnly, bound.Date)
		return err == nil
	}
	return !bound.DateTime.IsZero()
}

func calendarEventBoundString(bound gcalmodel.EventDateTime) string {
	if bound.Date != "" {
		return bound.Date
	}
	if !bound.DateTime.IsZero() {
		return bound.DateTime.Format(time.RFC3339)
	}
	return ""
}

func calendarSourceMessageTx(ctx context.Context, tx *loggedTx, sourceID int64, sourceMessageID string) (int64, string, error) {
	var messageID int64
	var messageType string
	err := tx.QueryRowContext(ctx, `SELECT id, message_type FROM messages
		WHERE source_id = ? AND source_message_id = ?`, sourceID, sourceMessageID).Scan(&messageID, &messageType)
	return messageID, messageType, err
}

func copyCalendarEventBodyTx(ctx context.Context, tx *loggedTx, keepID, duplicateID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_bodies WHERE message_id = ?`, keepID); err != nil {
		return fmt.Errorf("replace legacy calendar event body: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO message_bodies(message_id, body_text, body_html)
		SELECT ?, body_text, body_html FROM message_bodies WHERE message_id = ?`, keepID, duplicateID); err != nil {
		return fmt.Errorf("copy duplicate calendar event body: %w", err)
	}
	return nil
}

func copyCalendarEventRawTx(ctx context.Context, tx *loggedTx, keepID, duplicateID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_raw WHERE message_id = ?`, keepID); err != nil {
		return fmt.Errorf("replace legacy calendar event raw data: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO message_raw(message_id, raw_data, raw_format, compression, encryption_version)
		SELECT ?, raw_data, raw_format, compression, encryption_version FROM message_raw WHERE message_id = ?`, keepID, duplicateID); err != nil {
		return fmt.Errorf("copy duplicate calendar event raw data: %w", err)
	}
	return nil
}

func copyCalendarEventRecipientsTx(ctx context.Context, tx *loggedTx, keepID, duplicateID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_recipients WHERE message_id = ?`, keepID); err != nil {
		return fmt.Errorf("replace legacy calendar event recipients: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO message_recipients(message_id, participant_id, recipient_type, display_name, email_address)
		SELECT ?, participant_id, recipient_type, display_name, email_address FROM message_recipients WHERE message_id = ?`, keepID, duplicateID); err != nil {
		return fmt.Errorf("copy duplicate calendar event recipients: %w", err)
	}
	return nil
}

func calendarEventFTSDocTx(ctx context.Context, tx *loggedTx, messageID int64) (FTSDoc, error) {
	var doc FTSDoc
	doc.MessageID = messageID
	var subject sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT subject FROM messages WHERE id = ?`, messageID).Scan(&subject); err != nil {
		return doc, fmt.Errorf("read reconciled calendar event subject: %w", err)
	}
	doc.Subject = subject.String
	var body sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT body_text FROM message_bodies WHERE message_id = ?`, messageID).Scan(&body)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return doc, fmt.Errorf("read reconciled calendar event body: %w", err)
	}
	doc.Body = body.String
	rows, err := tx.QueryContext(ctx, `SELECT recipient_type,
		COALESCE(NULLIF(email_address, ''), (SELECT email_address FROM participants WHERE id = message_recipients.participant_id), ''), id
		FROM message_recipients WHERE message_id = ? ORDER BY id`, messageID)
	if err != nil {
		return doc, fmt.Errorf("read reconciled calendar event recipients: %w", err)
	}
	var fromAddrs, toAddrs, ccAddrs []string
	for rows.Next() {
		var recipientType, email string
		var id int64
		if err := rows.Scan(&recipientType, &email, &id); err != nil {
			_ = rows.Close()
			return doc, fmt.Errorf("scan reconciled calendar event recipient: %w", err)
		}
		if email == "" {
			continue
		}
		switch recipientType {
		case meetingSenderRole:
			fromAddrs = append(fromAddrs, email)
		case "to":
			toAddrs = append(toAddrs, email)
		case "cc":
			ccAddrs = append(ccAddrs, email)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return doc, fmt.Errorf("iterate reconciled calendar event recipients: %w", err)
	}
	doc.FromAddr = strings.Join(fromAddrs, " ")
	doc.ToAddrs = strings.Join(toAddrs, " ")
	doc.CcAddrs = strings.Join(ccAddrs, " ")
	return doc, nil
}

func (s *Store) bestEffortCalendarEventFTSUpsert(ctx context.Context, tx *loggedTx, q querier, doc FTSDoc) error {
	const savepoint = "calendar_event_identity_fts"
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
		return err
	}
	if err := s.dialect.FTSUpsert(q, doc); err != nil {
		if _, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		if _, releaseErr := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+savepoint); releaseErr != nil {
			return errors.Join(err, releaseErr)
		}
		slog.Warn("calendar event FTS upsert failed after identity reconciliation", "message_id", doc.MessageID, "error", err)
		return nil
	}
	_, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+savepoint)
	return err
}
