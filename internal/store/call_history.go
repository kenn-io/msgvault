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
		if o.Withdrawn {
			// Conversation upserts preserve existing titles when given empty text.
			if _, err := tx.ExecContext(ctx, `UPDATE conversations SET title = '' WHERE source_id = ? AND source_conversation_id = ?`, sourceID, o.RecordID); err != nil {
				return fmt.Errorf("clear withdrawn call title: %w", err)
			}
		}
		return nil
	}
	prepare := func(ctx context.Context, tx *loggedTx, data *MessagePersistData) (*MessagePersistData, error) {
		if !o.Sparse {
			return data, nil
		}
		var metadata string
		err := tx.QueryRowContext(ctx, `
			SELECT m.metadata, m.is_from_me FROM messages m JOIN call_details c ON c.message_id = m.id
			WHERE m.source_id = ? AND m.source_message_id = ? AND c.kind = 'scheduled_meeting'`, sourceID, o.RecordID).Scan(&metadata, &data.Message.IsFromMe)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: calendar event to cancel does not exist", callhistory.ErrInvalid)
		}
		if err != nil {
			return nil, fmt.Errorf("find calendar event to cancel: %w", err)
		}
		var evidence callhistory.Evidence
		if err := json.Unmarshal([]byte(metadata), &evidence); err != nil {
			return nil, fmt.Errorf("decode calendar evidence: %w", err)
		}
		evidence.Outcome = "cancelled"
		encoded, err := json.Marshal(evidence)
		if err != nil {
			return nil, fmt.Errorf("encode calendar evidence: %w", err)
		}
		data.Message.SentAt = sql.NullTime{Time: evidence.OccurredAt, Valid: true}
		data.Metadata = &sql.NullString{String: string(encoded), Valid: true}
		// A delta has no replacement recipients or raw. Persistence keeps the
		// stored rows when these fields are absent, inside this transaction.
		data.Recipients, data.RawMIME = nil, nil
		return data, nil
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
	if o.Kind == "scheduled_meeting" {
		text = "Scheduled meeting"
	}
	if o.DurationSeconds != nil {
		text += fmt.Sprintf(", %g seconds", *o.DurationSeconds)
	}
	if o.Withdrawn {
		text = ""
	}
	return s.persistMessageWithParticipantsTransaction(ctx, attributionLock{Sources: []int64{sourceID}},
		before, nil, func([]int64) *MessagePersistData {
			return &MessagePersistData{
				Message: &Message{
					SourceID: sourceID, SourceMessageID: o.RecordID, MessageType: callhistory.MessageType, IsFromMe: o.Direction == "outgoing",
					SentAt:  sql.NullTime{Time: o.OccurredAt, Valid: true},
					Subject: sql.NullString{String: text, Valid: true},
					Snippet: sql.NullString{String: text, Valid: true},
				},
				Conversation: &ConversationPersistData{
					SourceConversationID: o.RecordID, ConversationType: "call_history", Title: text,
				},
				BodyText: sql.NullString{String: text, Valid: true},
				FTS:      callFTSDoc(o, text),
				Metadata: &sql.NullString{String: string(metadata), Valid: true},
				RawMIME:  o.Raw, RawFormat: "call_history_json", Recipients: sets,
				PreserveLabels: true,
			}
		}, prepare, s.refreshCallConversationStatsWith)
}

// Withdrawn calls persist an empty search document, so the same upsert that
// indexes a call leaves it unsearchable on both dialects.
func callFTSDoc(o callhistory.Observation, text string) *FTSDoc {
	if o.Withdrawn {
		return &FTSDoc{}
	}
	return &FTSDoc{Subject: text, Body: text}
}

// Imported calls keep their conversation's denormalized stats current.
func (s *Store) refreshCallConversationStatsWith(ctx context.Context, tx *loggedTx, _ *MessagePersistData, id int64) error {
	return s.recomputeConversationStatsWith(boundQuerier{ctx: ctx, q: tx}, "id = (SELECT conversation_id FROM messages WHERE id = ?)", id)
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
		(message_id, canonical_key, kind, direction, outcome,
		 owner_participation, occurred_at, duration_seconds, withdrawn)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (message_id) DO UPDATE SET
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
