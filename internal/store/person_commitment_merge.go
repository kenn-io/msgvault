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
	source, err := commitmentAuthoredSourceTx(ctx, tx, messageID)
	if err != nil {
		return err
	}
	if located, live := locateCommitmentQuote(source, from); live {
		from = located
	} else if from.AnchorMessageID != messageID {
		from.State, from.SnoozedUntil, from.ReopenedAt = LoopStateOpen, nil, nil
	}
	promise := peoplesweep.CommitmentPromise{Direction: from.Direction, Start: from.Start, End: from.End}
	current, ambiguous, err := s.matchCommitmentLocator(ctx, tx, personID, messageID, promise, source, false)
	if err != nil {
		return err
	}
	if ambiguous {
		return nil // Retain the source decision for undo; no unique survivor owns it.
	}
	// No live survivor matched. A stale row at the same unique locator is
	// a replacement, so it receives only the revalidated incoming decision.
	if current.ID == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO person_commitments
			(person_id, anchor_message_id, current_claim_id, target_key, direction,
			quote, quote_start, quote_end, summary, due_on, promised_at, expires_at,
			owner_state, snoozed_until, reopened_at, fulfilment_evidence_id, fulfilment_confirmed)
			SELECT ?, ?, NULL, target_key, direction,
			quote, ?, ?, summary, due_on, promised_at, expires_at,
			?, ?, ?, NULL, 0
			FROM person_commitments WHERE id = ?
			ON CONFLICT (person_id, anchor_message_id, direction, quote_start, quote_end) DO UPDATE SET
				current_claim_id = NULL, target_key = excluded.target_key, quote = excluded.quote,
				summary = excluded.summary, due_on = excluded.due_on,
				promised_at = excluded.promised_at, expires_at = excluded.expires_at,
				owner_state = excluded.owner_state, snoozed_until = excluded.snoozed_until,
				reopened_at = excluded.reopened_at, fulfilment_evidence_id = NULL, fulfilment_confirmed = 0`, personID, messageID, from.Start, from.End, from.State,
			s.commitmentTimestamp(from.SnoozedUntil), s.commitmentTimestamp(from.ReopenedAt), from.ID)
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
	_, err = tx.ExecContext(ctx, `DELETE FROM person_commitment_frontiers WHERE person_id = ?`, personID)
	return err
}

// A duplicate may render the same promise at different original-body offsets.
// Only an exact authored quote can carry its incoming owner decision.
func commitmentAuthoredSourceTx(ctx context.Context, tx *loggedTx, messageID int64) (peoplesweep.CommitmentSource, error) {
	var body string
	var kind sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(body_text, ''),
		(SELECT conversation_type FROM conversations WHERE id = (SELECT conversation_id FROM messages WHERE id = ?))
		FROM message_bodies WHERE message_id = ?`, messageID, messageID).Scan(&body, &kind)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return peoplesweep.CommitmentSource{}, err
	}
	if kind.String == "email_thread" {
		body = string(commitmentEmailRunes(body))
	}
	return peoplesweep.CommitmentSource{Text: body}, nil
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

// Only unchanged authored words at their current range or a unique moved
// location retain a promise's owner decisions and fulfilment.
func locateCommitmentQuote(source peoplesweep.CommitmentSource, c PersonCommitment) (PersonCommitment, bool) {
	if c.ID == 0 || c.Quote == "" {
		return c, false
	}
	if source.MatchesQuote(c.Quote, c.Start, c.End) {
		return c, true
	}
	at := strings.Index(source.Text, c.Quote)
	if at < 0 || strings.Contains(source.Text[at+1:], c.Quote) {
		return c, false
	}
	start := source.Start + int64(utf8.RuneCountInString(source.Text[:at]))
	c.Start, c.End = start, start+int64(utf8.RuneCountInString(c.Quote))
	return c, source.MatchesQuote(c.Quote, c.Start, c.End)
}
