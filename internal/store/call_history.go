package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/callhistory"
)

var ErrCallObservationWithdrawn = errors.New("call observation was withdrawn")

// PutCallObservationContext replaces one source observation atomically. It
// never resolves a person by name or infers participants from a chat roster.
func (s *Store) PutCallObservationContext(ctx context.Context, sourceID int64, o callhistory.Observation) (int64, error) {
	if err := o.Validate(); err != nil {
		return 0, err
	}
	metadata, err := json.Marshal(o.Evidence)
	if err != nil {
		return 0, fmt.Errorf("encode call evidence: %w", err)
	}
	sets := []RecipientSet{{Type: "call_joined"}, {Type: "call_invited"}, {Type: "call_unknown"}}
	for _, p := range o.Participants {
		for i := range sets {
			if sets[i].Type == "call_"+p.Participation {
				sets[i].ParticipantIDs = append(sets[i].ParticipantIDs, p.ID)
			}
		}
	}
	// Source locking serializes absent-row creation as well as later updates.
	// The identity lock held by persistence also orders participant merges.
	before := func(ctx context.Context, tx *loggedTx) error {
		var sourceType, identifier string
		err := tx.QueryRowContext(ctx, `SELECT source_type, identifier FROM sources WHERE id = ?`, sourceID).Scan(&sourceType, &identifier)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSourceNotFound
		}
		if err != nil {
			return err
		}
		if sourceType != callhistory.SourceType || identifier != o.SourceIdentifier() {
			return fmt.Errorf("%w: source provenance mismatch", callhistory.ErrInvalid)
		}
		if err := requireSourceWritableWith(boundQuerier{ctx: ctx, q: tx}, sourceID); err != nil {
			return err
		}
		for _, p := range o.Participants {
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM participants WHERE id = ?)`, p.ID).Scan(&exists); err != nil {
				return fmt.Errorf("check call participant: %w", err)
			}
			if !exists {
				return fmt.Errorf("%w: participant does not exist", callhistory.ErrInvalid)
			}
		}
		var withdrawn bool
		err = tx.QueryRowContext(ctx, `
			SELECT c.withdrawn FROM messages m JOIN call_details c ON c.message_id = m.id
			WHERE m.source_id = ? AND m.source_message_id = ?`, sourceID, o.RecordID).Scan(&withdrawn)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if withdrawn && !o.Withdrawn {
			return ErrCallObservationWithdrawn
		}
		return nil
	}
	return s.persistMessageWithParticipantsTransaction(ctx, attributionLock{Sources: []int64{sourceID}},
		before, nil, func([]int64) *MessagePersistData {
			return &MessagePersistData{
				Message: &Message{
					SourceID: sourceID, SourceMessageID: o.RecordID, MessageType: callhistory.MessageType,
					SentAt:  sql.NullTime{Time: o.OccurredAt, Valid: true},
					Subject: sql.NullString{String: "Call evidence", Valid: true},
				},
				Conversation: &ConversationPersistData{
					SourceConversationID: o.RecordID, ConversationType: "call_history", Title: "Call evidence",
				},
				Metadata: &sql.NullString{String: string(metadata), Valid: true},
				RawMIME:  o.Raw, RawFormat: "call_history_json", Recipients: sets,
			}
		}, nil, nil)
}

// Scalar projections share the message lock and transaction of their metadata.
// Original provider data remains in message_raw; participant IDs never appear
// in scalar metadata and follow the existing recipient lifecycle.
func (s *Store) refreshCallProjectionWith(ctx context.Context, tx *loggedTx, id int64, messageType, metadata string) error {
	if messageType != callhistory.MessageType {
		_, err := tx.ExecContext(ctx, `DELETE FROM call_details WHERE message_id = ?`, id)
		return err
	}
	var e callhistory.Evidence
	if err := json.Unmarshal([]byte(metadata), &e); err != nil {
		return fmt.Errorf("%w: scalar metadata", callhistory.ErrInvalid)
	}
	if err := e.Validate(); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO call_details
		(message_id, projection_version, canonical_key, kind, direction, outcome,
		 owner_participation, occurred_at, duration_seconds, withdrawn)
		VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (message_id) DO UPDATE SET
			projection_version = excluded.projection_version,
			canonical_key = excluded.canonical_key, kind = excluded.kind,
			direction = excluded.direction, outcome = excluded.outcome,
			owner_participation = excluded.owner_participation,
			occurred_at = excluded.occurred_at, duration_seconds = excluded.duration_seconds,
			withdrawn = excluded.withdrawn`,
		id, e.CanonicalKey(), e.Kind, e.Direction, e.Outcome, e.OwnerParticipation,
		e.OccurredAt.UTC(), e.DurationSeconds, e.Withdrawn)
	if err != nil {
		return fmt.Errorf("write call projection: %w", err)
	}
	return nil
}
