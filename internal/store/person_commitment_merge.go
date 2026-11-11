package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

// transferCommitmentDecisionsTx follows person and confirmed message survivors.
// Immutable source claims retain their original keys. The next consented sweep
// revalidates any detached promise against its survivor.
func (s *Store) transferCommitmentDecisionsTx(ctx context.Context, tx *loggedTx, from, into int64, personMerge bool) error {
	column := "anchor_message_id"
	if personMerge {
		column = personMergePersonIDColumn
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+commitmentColumns+` FROM person_commitments WHERE `+column+` = ? ORDER BY id`, from)
	if err != nil {
		return err
	}
	var decisions []PersonCommitment
	for rows.Next() {
		decision, err := scanPersonCommitment(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		decisions = append(decisions, decision)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, decision := range decisions {
		personID, messageID := decision.PersonID, into
		if personMerge {
			personID, messageID = into, decision.AnchorMessageID
		}
		if err := s.transferCommitmentDecisionTx(ctx, tx, decision, personID, messageID); err != nil {
			return fmt.Errorf("transfer commitment %d to survivor: %w", decision.ID, err)
		}
	}
	return nil
}

func (s *Store) transferCommitmentDecisionTx(ctx context.Context, tx *loggedTx, from PersonCommitment, personID, messageID int64) error {
	from, err := rebaseCommitmentDecision(ctx, tx, from, messageID)
	if err != nil {
		return err
	}
	current, err := scanPersonCommitment(tx.QueryRowContext(ctx, `SELECT `+commitmentColumns+` FROM person_commitments
		WHERE person_id = ? AND anchor_message_id = ? AND direction = ? AND quote_start = ? AND quote_end = ?`,
		personID, messageID, from.Direction, from.Start, from.End))
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `INSERT INTO person_commitments
			(person_id, anchor_message_id, current_claim_id, target_key, direction,
			quote, quote_start, quote_end, summary, due_on, promised_at, expires_at,
			owner_state, snoozed_until, reopened_at, fulfilment_evidence_id, fulfilment_confirmed)
			SELECT ?, ?, NULL, target_key, direction,
			quote, ?, ?, summary, due_on, promised_at, expires_at,
			?, ?, ?, NULL, 0
			FROM person_commitments WHERE id = ? RETURNING id`, personID, messageID, from.Start, from.End, from.State,
			s.commitmentTimestamp(from.SnoozedUntil), s.commitmentTimestamp(from.ReopenedAt), from.ID).Scan(&current.ID)
	} else if err == nil {
		state, snooze := current.State, laterCommitmentTime(current.SnoozedUntil, from.SnoozedUntil)
		if from.State == LoopStateDone || current.State == LoopStateDone {
			state, snooze = LoopStateDone, nil
		} else if snooze != nil {
			state = LoopStateSnoozed
		}
		_, err = tx.ExecContext(ctx, `UPDATE person_commitments SET owner_state = ?, snoozed_until = ?, reopened_at = ? WHERE id = ?`,
			state, s.commitmentTimestamp(snooze), s.commitmentTimestamp(laterCommitmentTime(current.ReopenedAt, from.ReopenedAt)), current.ID)
	}
	if err != nil {
		return err
	}
	if err := s.transferCommitmentAgendaTx(ctx, tx, from.ID, current.ID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM person_commitment_frontiers WHERE person_id = ?`, personID)
	return err
}

// A duplicate may render the same promise at different original-body offsets.
// Only an exact authored quote can carry its incoming owner decision.
func rebaseCommitmentDecision(ctx context.Context, tx *loggedTx, from PersonCommitment, messageID int64) (PersonCommitment, error) {
	if from.AnchorMessageID == messageID {
		return from, nil
	}
	var body string
	var kind sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(body_text, ''),
		(SELECT conversation_type FROM conversations WHERE id = (SELECT conversation_id FROM messages WHERE id = ?))
		FROM message_bodies WHERE message_id = ?`, messageID, messageID).Scan(&body, &kind)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return PersonCommitment{}, err
	}
	if kind.String == "email_thread" {
		body = string(commitmentEmailRunes(body))
	}
	source := peoplesweep.CommitmentSource{Text: body}
	if source.MatchesQuote(from.Quote, from.Start, from.End) {
		return from, nil
	}
	if at := strings.Index(body, from.Quote); from.Quote != "" && at >= 0 && !strings.Contains(body[at+1:], from.Quote) {
		start := int64(utf8.RuneCountInString(body[:at]))
		end := start + int64(utf8.RuneCountInString(from.Quote))
		if source.MatchesQuote(from.Quote, start, end) {
			from.Start, from.End = start, end
			return from, nil
		}
	}
	from.State, from.SnoozedUntil, from.ReopenedAt = LoopStateOpen, nil, nil
	return from, nil
}

func (s *Store) commitmentTimestamp(at *time.Time) any {
	if at == nil {
		return nil
	}
	return s.dialect.TimestampParam(*at)
}

func laterCommitmentTime(a, b *time.Time) *time.Time {
	if a == nil || b != nil && b.After(*a) {
		return b
	}
	return a
}
