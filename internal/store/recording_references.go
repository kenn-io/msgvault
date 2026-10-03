package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"time"

	"github.com/google/uuid"
)

type RecordingReferenceCursor struct {
	At       time.Time
	AfterID  int64
	AfterRow bool
	Policy   string
}

func (c RecordingReferenceCursor) FeedCursor() ChangedMessagesCursor {
	if c.AfterRow {
		return ChangedMessagesAfter(c.At, c.AfterID)
	}
	return ChangedMessagesFrom(c.At)
}

func (s *Store) LoadRecordingReferenceCursor(ctx context.Context, destination string) (RecordingReferenceCursor, error) {
	var encoded string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key = ?`, "recording-reference:"+destination).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return RecordingReferenceCursor{}, nil
	}
	if err != nil {
		return RecordingReferenceCursor{}, err
	}
	var cursor RecordingReferenceCursor
	err = json.Unmarshal([]byte(encoded), &cursor)
	return cursor, err
}

func (s *Store) AdvanceRecordingReferenceCursor(ctx context.Context, destination string, before, after RecordingReferenceCursor) (bool, error) {
	oldValue, err := json.Marshal(before)
	if err != nil {
		return false, err
	}
	newValue, err := json.Marshal(after)
	if err != nil {
		return false, err
	}
	var swapped bool
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		key := "recording-reference:" + destination
		if _, err := q.Exec(`INSERT INTO archive_metadata (key, value) VALUES (?, ?) ON CONFLICT (key) DO NOTHING`, key, string(oldValue)); err != nil {
			return err
		}
		result, err := q.Exec(`UPDATE archive_metadata SET value = ? WHERE key = ? AND value = ?`, string(newValue), key, string(oldValue))
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		swapped = count == 1
		return err
	})
	return swapped, err
}

type RecordingPointer struct{ ID, Path string }
type RecordingMessage struct {
	ID                                                                        int64
	ArchiveUID, SourceType, SourceIdentifier, SourceMessageID, Body, BodyHTML string
	SentAt                                                                    *time.Time
	Live                                                                      bool
	Pointers                                                                  []RecordingPointer
}

func (s *Store) ReadRecordingMessage(ctx context.Context, messageID int64) (RecordingMessage, bool, error) {
	m := RecordingMessage{ID: messageID}
	var sent nullableTimestamp
	err := s.db.QueryRowContext(ctx, `SELECT s.source_type, s.identifier, COALESCE(m.source_message_id, ''), m.sent_at,
		CASE WHEN `+LiveMessagesWhere("m", true)+` THEN TRUE ELSE FALSE END
		FROM messages m JOIN sources s ON s.id=m.source_id WHERE m.id=?`, messageID).
		Scan(&m.SourceType, &m.SourceIdentifier, &m.SourceMessageID, &sent, &m.Live)
	if errors.Is(err, sql.ErrNoRows) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	m.SentAt = optionalTimestamp(sent)
	m.ArchiveUID, err = s.ArchiveUIDContext(ctx)
	if err != nil {
		return m, false, err
	}
	var text, html sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT body_text, body_html FROM message_bodies WHERE message_id=?`, messageID).Scan(&text, &html)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return m, false, err
	}
	m.Body = embeddingBodyValue(text, html)
	m.BodyHTML = nullStringValue(html)
	rows, err := s.db.QueryContext(ctx, `SELECT source_attachment_id, storage_path FROM attachments WHERE message_id=? AND source_attachment_id LIKE 'teams:recording:%' ORDER BY id`, messageID)
	if err != nil {
		return m, false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p RecordingPointer
		if err := rows.Scan(&p.ID, &p.Path); err != nil {
			return m, false, err
		}
		m.Pointers = append(m.Pointers, p)
	}
	return m, true, rows.Err()
}

type RecordingReferenceInput struct {
	RouteKey, RefSHA256, OccurrenceJSON string
}

type RecordingReferenceClaim struct {
	RecordingReferenceInput

	DestinationKey                string
	MessageID                     int64
	OperationID, State, ErrorCode string
	NextActionAt                  time.Time
	LastSendAt                    *time.Time
	RetryCount                    int
}

type RecordingReferenceResult struct {
	State, ErrorCode, SourceID, OccurrenceID, Outcome, CoverageState string
	NextActionAt                                                     time.Time
	LastSendAt                                                       *time.Time
	RetryCount                                                       int
}

func (s *Store) ReconcileRecordingReferences(ctx context.Context, destination string, messageID int64, live bool, refs []RecordingReferenceInput) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		return s.reconcileRecordingReferences(ctx, tx, destination, messageID, live, refs)
	})
}

func (s *Store) reconcileRecordingReferences(ctx context.Context, tx *loggedTx, destination string, messageID int64, live bool, refs []RecordingReferenceInput) error {
	q := boundQuerier{ctx: ctx, q: tx}
	rows, err := tx.QueryContext(ctx, `SELECT route_key, ref_sha256, state, occurrence_id, occurrence_json FROM recording_references WHERE destination_key=? AND message_id=?`, destination, messageID)
	if err != nil {
		return err
	}
	type previous struct{ hash, state, receipt, occurrence string }
	old := make(map[string]previous)
	for rows.Next() {
		var key string
		var p previous
		if err := rows.Scan(&key, &p.hash, &p.state, &p.receipt, &p.occurrence); err != nil {
			_ = rows.Close()
			return err
		}
		old[key] = p
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	now := s.dialect.TimestampParam(time.Now().UTC())
	if live {
		for _, ref := range refs {
			p, exists := old[ref.RouteKey]
			unchanged := p.hash == ref.RefSHA256 && p.occurrence == ref.OccurrenceJSON
			delete(old, ref.RouteKey)
			if !exists {
				_, err = q.Exec(`INSERT INTO recording_references (destination_key,message_id,route_key,ref_sha256,operation_id,occurrence_json,state,next_action_at) VALUES (?,?,?,?,?,?,'pending',?)`, destination, messageID, ref.RouteKey, ref.RefSHA256, uuid.NewString(), ref.OccurrenceJSON, now)
			} else if !unchanged && p.state != "uncertain" {
				_, err = q.Exec(`UPDATE recording_references SET ref_sha256=?,operation_id=?,occurrence_json=?,state='pending',next_action_at=?,error_code='',source_id='',occurrence_id='',outcome='',coverage_state='',last_send_at=NULL,retry_count=0 WHERE destination_key=? AND message_id=? AND route_key=?`, ref.RefSHA256, uuid.NewString(), ref.OccurrenceJSON, now, destination, messageID, ref.RouteKey)
			} else if unchanged && p.state == "withdrawn" {
				state := "pending"
				if p.receipt != "" {
					state = "retained"
				}
				_, err = q.Exec(`UPDATE recording_references SET state=?,next_action_at=? WHERE destination_key=? AND message_id=? AND route_key=?`, state, now, destination, messageID, ref.RouteKey)
			}
			if err != nil {
				return err
			}
		}
	}
	for key, p := range old {
		// Uncertain sends need receipt recovery before their local withdrawal settles.
		if p.state == "uncertain" {
			continue
		}
		if _, err := q.Exec(`UPDATE recording_references SET state='withdrawn' WHERE destination_key=? AND message_id=? AND route_key=?`, destination, messageID, key); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ClaimRecordingReferences(ctx context.Context, destination string, now time.Time, limit int) ([]RecordingReferenceClaim, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT message_id,route_key,ref_sha256,operation_id,occurrence_json,state,next_action_at,error_code,last_send_at,retry_count FROM recording_references WHERE destination_key=? AND state IN ('pending','uncertain') AND next_action_at<=? ORDER BY next_action_at,message_id,route_key LIMIT ?`, destination, s.dialect.TimestampParam(now), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var claims []RecordingReferenceClaim
	for rows.Next() {
		c := RecordingReferenceClaim{DestinationKey: destination}
		var next requiredTimestamp
		var sent nullableTimestamp
		if err := rows.Scan(&c.MessageID, &c.RouteKey, &c.RefSHA256, &c.OperationID, &c.OccurrenceJSON, &c.State, &next, &c.ErrorCode, &sent, &c.RetryCount); err != nil {
			return nil, err
		}
		c.NextActionAt, c.LastSendAt = next.Time, optionalTimestamp(sent)
		claims = append(claims, c)
	}
	return claims, rows.Err()
}

// MarkRecordingReferenceSending commits recoverable state before network work.
func (s *Store) MarkRecordingReferenceSending(ctx context.Context, claim RecordingReferenceClaim, sentAt time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE recording_references SET state='uncertain',last_send_at=?,next_action_at=? WHERE destination_key=? AND message_id=? AND route_key=? AND operation_id=? AND state=?`, s.dialect.TimestampParam(sentAt), s.dialect.TimestampParam(sentAt.Add(5*time.Minute)), claim.DestinationKey, claim.MessageID, claim.RouteKey, claim.OperationID, claim.State)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) FinishRecordingReference(ctx context.Context, claim RecordingReferenceClaim, result RecordingReferenceResult, live bool, refs []RecordingReferenceInput) (bool, error) {
	var sent any
	if result.LastSendAt != nil {
		sent = s.dialect.TimestampParam(*result.LastSendAt)
	}
	var applied bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		res, err := tx.ExecContext(ctx, `UPDATE recording_references SET state=?,next_action_at=?,error_code=?,source_id=?,occurrence_id=?,outcome=?,coverage_state=?,last_send_at=?,retry_count=? WHERE destination_key=? AND message_id=? AND route_key=? AND operation_id=?`, result.State, s.dialect.TimestampParam(result.NextActionAt), result.ErrorCode, result.SourceID, result.OccurrenceID, result.Outcome, result.CoverageState, sent, result.RetryCount, claim.DestinationKey, claim.MessageID, claim.RouteKey, claim.OperationID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		applied = true
		if result.State != "uncertain" {
			return s.reconcileRecordingReferences(ctx, tx, claim.DestinationKey, claim.MessageID, live, refs)
		}
		return nil
	})
	return applied && err == nil, err
}

func (s *Store) ReconsiderBlockedRecordingReferences(ctx context.Context, destination string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE recording_references SET state='pending',next_action_at=?,retry_count=0 WHERE destination_key=? AND state='blocked'`, s.dialect.TimestampParam(time.Now().UTC()), destination)
	return err
}
