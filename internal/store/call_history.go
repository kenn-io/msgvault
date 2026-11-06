package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/callhistory"
)

// PutCallObservationContext replaces one validated source observation atomically.
// It never resolves a person by name or infers participants from a chat roster.
func (s *Store) PutCallObservationContext(ctx context.Context, sourceID int64, o callhistory.Observation) (int64, error) {
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
		return nil
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
	text := "Call " + o.Direction
	if o.DurationSeconds != nil {
		text += fmt.Sprintf(", %g seconds", *o.DurationSeconds)
	}
	return s.persistMessageWithParticipantsTransaction(ctx, attributionLock{Sources: []int64{sourceID}},
		before, nil, func([]int64) *MessagePersistData {
			return &MessagePersistData{
				Message: &Message{
					SourceID: sourceID, SourceMessageID: o.RecordID, MessageType: callhistory.MessageType, IsFromMe: o.Direction == "outgoing",
					SentAt:  sql.NullTime{Time: o.OccurredAt, Valid: true},
					Snippet: sql.NullString{String: text, Valid: true},
				},
				Conversation: &ConversationPersistData{
					SourceConversationID: o.RecordID, ConversationType: "call_history", Title: text,
				},
				BodyText: sql.NullString{String: text, Valid: true},
				FTS:      &FTSDoc{Subject: text, Body: text},
				Metadata: &sql.NullString{String: string(metadata), Valid: true},
				RawMIME:  o.Raw, RawFormat: "call_history_json", Recipients: sets,
				PreserveLabels: true,
			}
		}, nil, s.refreshCallConversationStatsWith)
}

// Imported calls keep their conversation's denormalized stats current.
func (s *Store) refreshCallConversationStatsWith(ctx context.Context, tx *loggedTx, _ *MessagePersistData, id int64) error {
	return s.recomputeConversationStatsWith(boundQuerier{ctx: ctx, q: tx}, "id = (SELECT conversation_id FROM messages WHERE id = ?)", id)
}

// Scalar projections share the message lock and transaction of their metadata.
// Original provider data remains in message_raw; participant IDs never appear
// in scalar metadata and follow the existing recipient lifecycle. Persistence
// clears the projection when it rewrites a call as another message type.
func (s *Store) refreshCallProjectionWith(ctx context.Context, tx *loggedTx, id int64, metadata string) error {
	var e callhistory.Evidence
	if err := json.Unmarshal([]byte(metadata), &e); err != nil {
		return fmt.Errorf("%w: scalar metadata", callhistory.ErrInvalid)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO call_details
		(message_id, outcome, owner_participation, occurred_at, evidence_version)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (message_id) DO UPDATE SET
			outcome = excluded.outcome,
			owner_participation = excluded.owner_participation,
			occurred_at = excluded.occurred_at,
			evidence_version = excluded.evidence_version`,
		id, e.Outcome, e.OwnerParticipation, e.OccurredAt.UTC(), e.Version)
	if err != nil {
		return fmt.Errorf("write call projection: %w", err)
	}
	return nil
}
