package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/meetingcontent"
)

const meetingProjectionVersion = 1
const maxMeetingRawBytes = 64 << 20

var errMeetingRawTooLarge = errors.New("meeting raw exceeds decode limit")

// lockMeetingEvidenceWith orders public raw writes and backfill like persistence:
// reserve SQLite's writer before reading, or lock the PostgreSQL message before raw.
func (s *Store) lockMeetingEvidenceWith(ctx context.Context, tx *loggedTx, messageID int64) error {
	q := boundQuerier{ctx: ctx, q: tx}
	if s.dialect.DriverName() != postgresDriverName {
		_, err := q.Exec(`UPDATE embedding_change_clock SET sequence = sequence WHERE singleton = 1`)
		return err
	}
	var id int64
	err := q.QueryRow(`SELECT id FROM messages WHERE id = ? FOR UPDATE`, messageID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

// refreshMeetingProjectionWith reads the persisted snapshot under the caller's
// message lock. Decode failures are source coverage; storage failures abort the
// caller's transaction, including any raw, metadata, body or recipient changes.
func (s *Store) refreshMeetingProjectionWith(ctx context.Context, tx *loggedTx, messageID int64) error {
	q := boundQuerier{ctx: ctx, q: tx}
	var messageType, metadata sql.NullString
	err := q.QueryRow(`SELECT message_type, metadata FROM messages WHERE id = ?`, messageID).Scan(&messageType, &metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read meeting projection message: %w", err)
	}
	if messageType.String != "meeting_transcript" {
		// The common non-meeting path does no derived writes unless a prior meeting
		// was reclassified. Check both tables through the details primary key.
		var exists int
		err := q.QueryRow(`SELECT 1 FROM meeting_details WHERE message_id = ?`, messageID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read prior meeting projection: %w", err)
		}
		if _, err := q.Exec(`DELETE FROM meeting_action_items WHERE message_id = ?`, messageID); err != nil {
			return fmt.Errorf("clear meeting actions: %w", err)
		}
		if _, err := q.Exec(`DELETE FROM meeting_details WHERE message_id = ?`, messageID); err != nil {
			return fmt.Errorf("clear meeting details: %w", err)
		}
		return nil
	}
	var stored []byte
	var format, compression sql.NullString
	err = q.QueryRow(`SELECT raw_data, raw_format, compression FROM message_raw WHERE message_id = ?`, messageID).Scan(&stored, &format, &compression)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read meeting projection raw: %w", err)
	}
	raw, _, decodeErr := decodeMessageRawBounded(stored, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
	content := meetingcontent.Decode(format.String, raw, []byte(metadata.String))
	if decodeErr != nil {
		reason := "invalid_raw"
		if errors.Is(decodeErr, errMeetingRawTooLarge) {
			reason = "raw_too_large"
		}
		content = meetingcontent.Content{
			Summary:        meetingcontent.Section{State: meetingcontent.StateUnavailable, Reason: reason},
			Notes:          meetingcontent.Section{State: meetingcontent.StateUnavailable, Reason: reason},
			Transcript:     meetingcontent.Transcript{State: meetingcontent.StateUnavailable, Reason: reason},
			Actions:        []meetingcontent.Action{},
			ActionCoverage: meetingcontent.CoverageUnavailable,
			ActionReason:   reason,
		}
	}
	full, err := json.Marshal(content)
	if err != nil {
		return fmt.Errorf("marshal meeting content hash: %w", err)
	}
	sum := sha256.Sum256(full)
	hash := hex.EncodeToString(sum[:])
	var oldVersion int
	var oldHash string
	err = q.QueryRow(`SELECT projection_version, content_hash FROM meeting_details WHERE message_id = ?`, messageID).Scan(&oldVersion, &oldHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read meeting projection version: %w", err)
	}
	if err == nil && oldVersion == meetingProjectionVersion && oldHash == hash {
		return nil
	}
	compact, err := json.Marshal(meetingcontent.ProjectionContent(content))
	if err != nil {
		return fmt.Errorf("marshal meeting projection: %w", err)
	}
	_, err = q.Exec(`
		INSERT INTO meeting_details (
			message_id, projection_version, content_hash, content_json,
			duration_seconds, duration_basis, action_coverage, transcript_state
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (message_id) DO UPDATE SET
			projection_version = excluded.projection_version,
			content_hash = excluded.content_hash,
			content_json = excluded.content_json,
			duration_seconds = excluded.duration_seconds,
			duration_basis = excluded.duration_basis,
			action_coverage = excluded.action_coverage,
			transcript_state = excluded.transcript_state`,
		messageID, meetingProjectionVersion, hash, string(compact),
		content.DurationSeconds, string(content.DurationBasis),
		string(content.ActionCoverage), string(content.Transcript.State),
	)
	if err != nil {
		return fmt.Errorf("write meeting projection: %w", err)
	}
	if _, err := q.Exec(`DELETE FROM meeting_action_items WHERE message_id = ?`, messageID); err != nil {
		return fmt.Errorf("replace meeting actions: %w", err)
	}
	for _, action := range content.Actions {
		_, err := q.Exec(`
			INSERT INTO meeting_action_items (
				message_id, ordinal, source_id, title, description,
				assignee_name, assignee_email, status, source_status, due_date, origin, locator
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			messageID, action.Ordinal, action.SourceID, action.Title, action.Description,
			action.AssigneeName, action.AssigneeEmail, string(action.Status),
			action.SourceStatus, action.DueDate, action.Origin, action.Locator,
		)
		if err != nil {
			return fmt.Errorf("write meeting action %d: %w", action.Ordinal, err)
		}
	}
	return nil
}
