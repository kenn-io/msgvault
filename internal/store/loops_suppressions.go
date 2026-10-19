package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const loopSuppressionColumns = "id,target_kind,participant_id," + personMergePersonIDColumn + ",conversation_id,note,created_at"

func scanLoopSuppression(row scanner) (LoopSuppression, error) {
	var suppression LoopSuppression
	err := row.Scan(&suppression.ID, &suppression.TargetKind, &suppression.ParticipantID, &suppression.PersonID, &suppression.ConversationID, &suppression.Note, &suppression.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return suppression, ErrLoopNotFound
	}
	return suppression, err
}
func (s *Store) LoadLoopSuppressionsContext(ctx context.Context) ([]LoopSuppression, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+loopSuppressionColumns+" FROM loop_suppressions ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []LoopSuppression{}
	for rows.Next() {
		suppression, err := scanLoopSuppression(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, suppression)
	}
	return out, rows.Err()
}
func validateLoopSuppression(request LoopSuppressionRequest) error {
	count := 0
	for _, id := range []*int64{request.ParticipantID, request.PersonID, request.ConversationID} {
		if id != nil {
			if *id <= 0 {
				return fmt.Errorf("%w: suppression target must be positive", ErrInvalidLoop)
			}
			count++
		}
	}
	valid := request.TargetKind == "participant" && request.ParticipantID != nil || request.TargetKind == string(AttributeObjectPerson) && request.PersonID != nil || request.TargetKind == "conversation" && request.ConversationID != nil
	if count != 1 || !valid {
		return fmt.Errorf("%w: exactly one matching suppression target required", ErrInvalidLoop)
	}
	return nil
}
func loopSuppressionPredicate(request LoopSuppressionRequest) (string, []any) {
	switch request.TargetKind {
	case "participant":
		return "counterparty_participant_id=?", []any{request.ParticipantID}
	case string(AttributeObjectPerson):
		// Current participant ownership supersedes the saved person ID after a split.
		return "COALESCE((SELECT pp.person_id FROM person_participants pp WHERE pp.participant_id=open_loops.counterparty_participant_id),open_loops.counterparty_person_id)=?", []any{request.PersonID}
	default:
		return "conversation_id=?", []any{request.ConversationID}
	}
}

// suppressionConversationIDs includes native membership even before any loop has been derived.
func (s *Store) suppressionConversationIDs(ctx context.Context, tx *loggedTx, request LoopSuppressionRequest, lookback time.Duration, now time.Time) ([]int64, error) {
	membership := "c.id=?"
	args := []any{}
	switch request.TargetKind {
	case "conversation":
		args = append(args, request.ConversationID)
	case "participant", string(AttributeObjectPerson):
		participant := "p.id=?"
		target := any(request.ParticipantID)
		if request.TargetKind == string(AttributeObjectPerson) {
			participant = "EXISTS(SELECT 1 FROM person_participants pp WHERE pp.participant_id=p.id AND pp." + personMergePersonIDColumn + "=?)"
			target = request.PersonID
		}
		membership = `EXISTS(SELECT 1 FROM participants p WHERE ` + participant + ` AND (
   EXISTS(SELECT 1 FROM messages m WHERE m.conversation_id=c.id AND m.sender_id=p.id)
   OR EXISTS(SELECT 1 FROM message_recipients mr WHERE mr.participant_id=p.id AND EXISTS(SELECT 1 FROM messages m WHERE m.id=mr.message_id AND m.conversation_id=c.id))
   OR EXISTS(SELECT 1 FROM conversation_participants cp WHERE cp.conversation_id=c.id AND cp.participant_id=p.id AND cp.left_at IS NULL)))`
		args = append(args, target)
	}
	native := "(" + membership + ")"
	if lookback > 0 {
		native += ` AND (c.last_message_at>=? OR EXISTS(SELECT 1 FROM messages m WHERE m.conversation_id=c.id AND COALESCE(m.sent_at,m.received_at,m.internal_date)>=?))`
		cutoff := s.dialect.TimestampParam(now.Add(-lookback))
		args = append(args, cutoff, cutoff)
	}
	predicate, predicateArgs := loopSuppressionPredicate(request)
	query := "SELECT c.id FROM conversations c WHERE ((" + native + ") OR EXISTS(SELECT 1 FROM open_loops WHERE conversation_id=c.id AND status<>'closed' AND " + predicate + ")) ORDER BY c.id"
	args = append(args, predicateArgs...)
	if s.IsPostgreSQL() {
		query += " FOR UPDATE OF c"
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) CreateLoopSuppressionContext(ctx context.Context, request LoopSuppressionRequest, now time.Time) (LoopSuppressionResult, error) {
	var out LoopSuppressionResult
	if err := validateLoopSuppression(request); err != nil {
		return out, err
	}
	if now.IsZero() {
		return out, fmt.Errorf("%w: time required", ErrInvalidLoop)
	}
	if request.DryRun {
		predicate, predicateArgs := loopSuppressionPredicate(request)
		err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
			return tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM open_loops WHERE status<>'closed' AND "+predicate, predicateArgs...).Scan(&out.WouldClose)
		})
		return out, err
	}
	err := retryBusyWriteErr(ctx, s, "create loop suppression", func() error {
		out = LoopSuppressionResult{}
		return s.withTxContext(ctx, func(tx *loggedTx) error {
			if _, err := s.suppressionConversationIDs(ctx, tx, request, 0, now); err != nil {
				return err
			}
			predicate, predicateArgs := loopSuppressionPredicate(request)
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM open_loops WHERE status<>'closed' AND "+predicate, predicateArgs...).Scan(&out.WouldClose); err != nil {
				return err
			}
			// Repeated mutes are idempotent and retain the original owner note.
			column := "conversation_id"
			if request.TargetKind == "participant" {
				column = "participant_id"
			}
			if request.TargetKind == string(AttributeObjectPerson) {
				column = personMergePersonIDColumn
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO loop_suppressions(target_kind,participant_id,`+personMergePersonIDColumn+`,conversation_id,note,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`, request.TargetKind, request.ParticipantID, request.PersonID, request.ConversationID, request.Note, s.dialect.TimestampParam(now)); err != nil {
				return err
			}
			suppression, err := scanLoopSuppression(tx.QueryRowContext(ctx, "SELECT "+loopSuppressionColumns+" FROM loop_suppressions WHERE "+column+"=?", predicateArgs[0]))
			if err != nil {
				return err
			}
			out.Suppression = &suppression
			args := append([]any{s.dialect.TimestampParam(now), s.dialect.TimestampParam(now)}, predicateArgs...)
			_, err = tx.ExecContext(ctx, `UPDATE open_loops SET status='closed',closed_at=?,closed_reason='muted',closed_by='user',closed_message_id=NULL,updated_at=? WHERE status<>'closed' AND `+predicate, args...)
			return err
		})
	})
	if err != nil {
		return LoopSuppressionResult{}, err
	}
	return out, nil
}

func (s *Store) DeleteLoopSuppressionContext(ctx context.Context, id int64, lookback time.Duration, now time.Time) error {
	if id <= 0 || lookback < 0 || now.IsZero() {
		return fmt.Errorf("%w: suppression, lookback and time invalid", ErrInvalidLoop)
	}
	return retryBusyWriteErr(ctx, s, "delete loop suppression", func() error {
		return s.withTxContext(ctx, func(tx *loggedTx) error {
			suppression, err := scanLoopSuppression(tx.QueryRowContext(ctx, "SELECT "+loopSuppressionColumns+" FROM loop_suppressions WHERE id=?", id))
			if err != nil {
				return err
			}
			request := LoopSuppressionRequest{TargetKind: suppression.TargetKind, ParticipantID: suppression.ParticipantID, PersonID: suppression.PersonID, ConversationID: suppression.ConversationID}
			ids, err := s.suppressionConversationIDs(ctx, tx, request, lookback, now)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM loop_suppressions WHERE id=?", id); err != nil {
				return err
			}
			for _, conversationID := range ids {
				_, err := tx.ExecContext(ctx, `INSERT INTO loop_conversation_queue(conversation_id,revision,feed_bound,reason,queued_at) VALUES(?,1,(SELECT feed_frontier FROM loop_derivation_state WHERE singleton=1),'unmute',?) `+loopQueueConflict, conversationID, s.dialect.TimestampParam(now))
				if err != nil {
					return err
				}
			}
			return nil
		})
	})
}

func (s *Store) mergeParticipantLoopStateTx(ctx context.Context, tx *loggedTx, absorbedID, survivorID int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE open_loops SET counterparty_participant_id = ? WHERE counterparty_participant_id = ?`, survivorID, absorbedID); err != nil {
		return fmt.Errorf("repoint loop counterpart: %w", err)
	}

	type suppression struct {
		id        int64
		note      string
		createdAt time.Time
	}
	var absorbed suppression
	err := tx.QueryRowContext(ctx, `SELECT id, note, created_at FROM loop_suppressions
		WHERE target_kind = 'participant' AND participant_id = ?`, absorbedID).
		Scan(&absorbed.id, &absorbed.note, &absorbed.createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load absorbed loop suppression: %w", err)
	}
	var survivor suppression
	err = tx.QueryRowContext(ctx, `SELECT id, note, created_at FROM loop_suppressions
		WHERE target_kind = 'participant' AND participant_id = ?`, survivorID).
		Scan(&survivor.id, &survivor.note, &survivor.createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `UPDATE loop_suppressions SET participant_id = ? WHERE id = ?`, survivorID, absorbed.id); err != nil {
			return fmt.Errorf("repoint absorbed loop suppression: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("load survivor loop suppression: %w", err)
	}
	createdAt := survivor.createdAt
	if absorbed.createdAt.Before(createdAt) {
		createdAt = absorbed.createdAt
	}
	if _, err := tx.ExecContext(ctx, `UPDATE loop_suppressions SET note = ?, created_at = ? WHERE id = ?`,
		mergeLoopSuppressionNotes(survivor.note, absorbed.note), s.dialect.TimestampParam(createdAt), survivor.id); err != nil {
		return fmt.Errorf("merge loop suppression notes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM loop_suppressions WHERE id = ?`, absorbed.id); err != nil {
		return fmt.Errorf("delete absorbed loop suppression: %w", err)
	}
	return nil
}
