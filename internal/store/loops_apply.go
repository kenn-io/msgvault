package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"
)

func loopTerminal(l OpenLoop) bool {
	return l.ClosedReason != nil && slices.Contains([]string{"done", "fulfilled", "expired"}, *l.ClosedReason)
}

func (s *Store) lockLoopConversationContext(ctx context.Context, tx *loggedTx, conversationID int64) error {
	// Serialize every loop action for a conversation, including insertion when no loop exists.
	query := "SELECT id FROM conversations WHERE id=?"
	if s.IsPostgreSQL() {
		query += " FOR UPDATE"
	}
	var id int64
	err := tx.QueryRowContext(ctx, query, conversationID).Scan(&id)
	return err
}

func (s *Store) closeLoopTx(ctx context.Context, tx *loggedTx, id int64, reason, by string, at time.Time, messageID *int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE open_loops SET status='closed',closed_at=?,closed_reason=?,closed_by=?,closed_message_id=?,updated_at=? WHERE id=? AND status<>'closed'`, s.dialect.TimestampParam(at), reason, by, messageID, s.dialect.TimestampParam(at), id)
	return err
}

func (s *Store) ApplyLoopVerdictContext(ctx context.Context, apply LoopApply) error {
	if apply.ConversationID <= 0 || apply.ObservedRevision < 0 || apply.Now.IsZero() {
		return fmt.Errorf("%w: conversation, observed revision and time required", ErrInvalidLoop)
	}
	if open := apply.Verdict.Open; open != nil {
		if open.ConversationID != apply.ConversationID || open.SourceID <= 0 || open.TurnKey == "" || !slices.Contains([]string{"reply_owed", "awaiting_reply"}, open.Kind) || !slices.Contains([]string{"email", "chat"}, open.Channel) || open.OwedSinceAt.IsZero() || open.OpensAt.IsZero() || open.LastActivityAt.IsZero() || open.RulesetVersion < 1 || !json.Valid(open.EvidenceJSON) || utf8.RuneCountInString(open.Summary) > 200 {
			return fmt.Errorf("%w: invalid open decision", ErrInvalidLoop)
		}
	}
	for _, close := range apply.Verdict.Close {
		if !slices.Contains([]string{"turn_flip", "withdrawn", "muted", "fulfilled", "expired"}, close.Reason) || close.At.IsZero() {
			return fmt.Errorf("%w: invalid close decision", ErrInvalidLoop)
		}
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockLoopConversationContext(ctx, tx, apply.ConversationID); err != nil {
			return err
		}
		for _, close := range apply.Verdict.Close {
			// Constrain all detector writes to the conversation that was observed.
			var conversation int64
			if err := tx.QueryRowContext(ctx, "SELECT conversation_id FROM open_loops WHERE id=?", close.ID).Scan(&conversation); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("load loop %d to close: %w", close.ID, ErrLoopNotFound)
				}
				return fmt.Errorf("load loop %d to close: %w", close.ID, err)
			}
			if conversation != apply.ConversationID {
				return fmt.Errorf("%w: close belongs to another conversation", ErrInvalidLoop)
			}
			if err := s.closeLoopTx(ctx, tx, close.ID, close.Reason, "system", close.At, close.MessageID); err != nil {
				return err
			}
		}
		if apply.Verdict.Open != nil {
			if err := s.reconcileOpenLoopTx(ctx, tx, *apply.Verdict.Open, apply.Now); err != nil {
				return err
			}
		}
		for _, id := range apply.Verdict.CoverageGaps {
			loop, err := scanOpenLoop(tx.QueryRowContext(ctx, "SELECT "+openLoopColumns+" FROM open_loops l WHERE l.id=? AND l.conversation_id=? AND l.status<>'closed'", id, apply.ConversationID))
			if errors.Is(err, ErrLoopNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			evidence, err := loopEvidenceMetadata(loop.EvidenceJSON, "coverage_gap", true)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE open_loops SET evidence_json=?,updated_at=? WHERE id=? AND status<>'closed'", string(evidence), s.dialect.TimestampParam(apply.Now), id); err != nil {
				return err
			}
		}
		// An old generation cannot acknowledge a later enqueue or erase its bound.
		_, err := tx.ExecContext(ctx, `UPDATE loop_conversation_queue SET
    feed_bound=CASE WHEN revision<=? AND epoch=? THEN NULL ELSE feed_bound END,
    epoch=CASE WHEN revision<=? AND epoch=? THEN '' ELSE epoch END,
    processed_revision=CASE WHEN processed_revision>? THEN processed_revision WHEN revision<? THEN revision ELSE ? END
    WHERE conversation_id=? AND epoch=?`, apply.ObservedRevision, apply.Epoch, apply.ObservedRevision, apply.Epoch, apply.ObservedRevision, apply.ObservedRevision, apply.ObservedRevision, apply.ConversationID, apply.Epoch)
		return err
	})
}

func (s *Store) reconcileOpenLoopTx(ctx context.Context, tx *loggedTx, open LoopOpenDecision, now time.Time) error {
	history, err := listLoopsQuery(ctx, tx, "SELECT "+openLoopColumns+" FROM open_loops l WHERE l.conversation_id=? AND l.kind=? AND l.detector='turn_state' ORDER BY l.created_at DESC,l.id DESC", open.ConversationID, open.Kind)
	if err != nil {
		return err
	}
	var live, latest *OpenLoop
	terminalOutside := false
	for i := range history {
		row := &history[i]
		if row.Status != "closed" {
			live = row
		}
		sameTurn := row.AnchorMessageID != nil && (slices.Contains(open.TurnMessageIDs, *row.AnchorMessageID) || *row.AnchorMessageID == open.AnchorMessageID)
		if sameTurn && (latest == nil || loopTerminal(*row)) {
			latest = row
		}
		if !sameTurn && row.Status == "closed" && loopTerminal(*row) {
			terminalOutside = true
		}
	}
	// A done anchor that fell beyond the bounded same-side window stays authoritative.
	blocked := latest != nil && loopTerminal(*latest) || !open.PreviousTurnKnown && terminalOutside && live == nil
	var muted bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM loop_suppressions WHERE conversation_id=? OR participant_id=? OR person_id=? OR person_id IN (SELECT person_id FROM person_participants WHERE participant_id=?))`, open.ConversationID, open.CounterpartyParticipantID, open.CounterpartyPersonID, open.CounterpartyParticipantID).Scan(&muted); err != nil {
		return err
	}
	if blocked || muted {
		if live != nil {
			reason := "withdrawn"
			if muted {
				reason = "muted"
			}
			return s.closeLoopTx(ctx, tx, live.ID, reason, "system", now, nil)
		}
		return nil
	}
	status := "open"
	if now.Before(open.OpensAt) {
		status = "held"
	}
	if live != nil {
		// Only a late earlier message may replace an existing anchor. A newer nudge preserves the original clock.
		rewrite := live.AnchorMessageID == nil || (live.AnchorMessageID != nil && *live.AnchorMessageID != open.AnchorMessageID && (open.OwedSinceAt.Before(live.OwedSinceAt) || (open.OwedSinceAt.Equal(live.OwedSinceAt) && open.AnchorMessageID < *live.AnchorMessageID)))
		if rewrite {
			var cycle int64
			if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(cycle),0)+1 FROM open_loops WHERE turn_key=?", open.TurnKey).Scan(&cycle); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE open_loops SET turn_key=?,cycle=?,anchor_message_id=?,owed_since_at=?,opens_at=? WHERE id=? AND status<>'closed'", open.TurnKey, cycle, open.AnchorMessageID, s.dialect.TimestampParam(open.OwedSinceAt), s.dialect.TimestampParam(open.OpensAt), live.ID); err != nil {
				return err
			}
		} else {
			// Recompute thresholds from the preserved oldest unanswered anchor.
			open.OpensAt = live.OwedSinceAt.Add(open.OpensAt.Sub(open.OwedSinceAt))
			status = "open"
			if now.Before(open.OpensAt) {
				status = "held"
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE open_loops SET status=?,last_activity_message_id=?,last_activity_at=?,summary=?,counterparty_participant_id=?,counterparty_person_id=?,owner_address=?,opens_at=?,evidence_json=?,ruleset_version=?,updated_at=? WHERE id=? AND status<>'closed'`, status, open.LastActivityMessageID, s.dialect.TimestampParam(open.LastActivityAt), open.Summary, open.CounterpartyParticipantID, open.CounterpartyPersonID, open.OwnerAddress, s.dialect.TimestampParam(open.OpensAt), string(open.EvidenceJSON), open.RulesetVersion, s.dialect.TimestampParam(now), live.ID)
		return err
	}
	var cycle int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(cycle),0)+1 FROM open_loops WHERE turn_key=?", open.TurnKey).Scan(&cycle); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO open_loops(turn_key,cycle,conversation_id,source_id,kind,detector,status,channel,owner_address,counterparty_participant_id,counterparty_person_id,anchor_message_id,owed_since_at,last_activity_message_id,last_activity_at,opens_at,summary,evidence_json,ruleset_version,created_at,updated_at) VALUES(?,?,?,?,?,'turn_state',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, open.TurnKey, cycle, open.ConversationID, open.SourceID, open.Kind, status, open.Channel, open.OwnerAddress, open.CounterpartyParticipantID, open.CounterpartyPersonID, open.AnchorMessageID, s.dialect.TimestampParam(open.OwedSinceAt), open.LastActivityMessageID, s.dialect.TimestampParam(open.LastActivityAt), s.dialect.TimestampParam(open.OpensAt), open.Summary, string(open.EvidenceJSON), open.RulesetVersion, s.dialect.TimestampParam(now), s.dialect.TimestampParam(now))
	return err
}

func loopEvidenceMetadata(raw []byte, key string, value any) ([]byte, error) {
	var evidence []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return nil, fmt.Errorf("decode loop evidence: %w", err)
	}
	if len(evidence) == 0 {
		evidence = []map[string]json.RawMessage{{}}
	}
	if evidence[0] == nil {
		evidence[0] = map[string]json.RawMessage{}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	evidence[0][key] = encoded
	return json.Marshal(evidence)
}
