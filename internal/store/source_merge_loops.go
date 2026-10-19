package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"
)

func (s *Store) transferMergedLoopState(ctx context.Context, tx *loggedTx, conversations []mergeConversation, messageSurvivors map[int64]int64) error {
	conversationIDs := make(map[int64]int64, len(conversations))
	for _, conversation := range conversations {
		conversationIDs[conversation.id] = conversation.destination
	}
	for _, conversation := range conversations {
		if conversation.id == conversation.destination {
			continue
		}
		if err := s.transferMergedConversationLoopState(ctx, tx, conversation.id, conversation.destination, conversationIDs, messageSurvivors); err != nil {
			return fmt.Errorf("transfer loop state from conversation %d to %d: %w", conversation.id, conversation.destination, err)
		}
	}
	return nil
}

func (s *Store) transferMergedConversationLoopState(ctx context.Context, tx *loggedTx, from, into int64, conversationIDs, messageSurvivors map[int64]int64) error {
	loops, err := listLoopsQuery(ctx, tx, "SELECT "+openLoopColumns+" FROM open_loops l WHERE l.conversation_id=? ORDER BY l.id", from)
	if err != nil {
		return err
	}
	for _, loop := range loops {
		reason, by := "", ""
		if loop.Status != "closed" {
			collision, found, err := findMergedLoopCollision(ctx, tx, loop, into)
			if err != nil {
				return err
			}
			if found {
				reason, by = "withdrawn", "system"
				if collision.ConversationID == into {
					if err := mergeLoopOwnerState(ctx, tx, loop, collision, s.dialect.Now()); err != nil {
						return err
					}
				}
			}
		}
		if reason != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE open_loops SET status='closed',closed_at=`+s.dialect.Now()+`,
closed_reason=?,closed_by=?,closed_message_id=NULL,updated_at=`+s.dialect.Now()+`
WHERE id=? AND status<>'closed'`, reason, by, loop.ID); err != nil {
				return err
			}
		}
		anchor := remapMergedMessageID(loop.AnchorMessageID, messageSurvivors)
		lastActivity := remapMergedMessageID(loop.LastActivityMessageID, messageSurvivors)
		closedMessage := remapMergedMessageID(loop.ClosedMessageID, messageSurvivors)
		evidence, err := remapMergedLoopEvidence(loop.EvidenceJSON, conversationIDs, messageSurvivors)
		if err != nil {
			return fmt.Errorf("remap evidence for loop %d: %w", loop.ID, err)
		}
		turnKey, cycle, err := remappedMergedTurnKey(ctx, tx, loop, into, anchor)
		if err != nil {
			return fmt.Errorf("remap turn key for loop %d: %w", loop.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE open_loops SET turn_key=?,cycle=?,conversation_id=?,anchor_message_id=?,last_activity_message_id=?,closed_message_id=?,evidence_json=`+s.dialect.JSONBindExpr()+` WHERE id=?`, turnKey, cycle, into, anchor, lastActivity, closedMessage, string(evidence), loop.ID); err != nil {
			return err
		}
	}
	return nil
}

func remapMergedMessageID(id *int64, survivors map[int64]int64) *int64 {
	if id == nil {
		return nil
	}
	if survivor, ok := survivors[*id]; ok {
		return &survivor
	}
	return id
}

func remapMergedLoopEvidence(raw jsontext.Value, conversations, survivors map[int64]int64) (jsontext.Value, error) {
	var evidence []map[string]jsontext.Value
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return nil, err
	}
	changed := false
	for _, entry := range evidence {
		for _, fieldMapping := range []struct {
			field   string
			mapping map[int64]int64
		}{{"message_id", survivors}, {"conversation_id", conversations}} {
			encodedID, ok := entry[fieldMapping.field]
			if !ok {
				continue
			}
			var id int64
			if err := json.Unmarshal(encodedID, &id); err != nil {
				return nil, fmt.Errorf("decode %s: %w", fieldMapping.field, err)
			}
			if replacement, ok := fieldMapping.mapping[id]; ok {
				entry[fieldMapping.field] = jsontext.Value(fmt.Sprint(replacement))
				changed = true
			}
		}
	}
	if !changed {
		return raw, nil
	}
	encoded, err := json.Marshal(evidence)
	return jsontext.Value(encoded), err
}

func remappedMergedTurnKey(ctx context.Context, tx *loggedTx, loop OpenLoop, conversationID int64, anchor *int64) (string, int64, error) {
	turnKey := loop.TurnKey
	if loop.Detector == "turn_state" && anchor != nil {
		turnKey = fmt.Sprintf("turn:%d:%s:%d", conversationID, loop.Kind, *anchor)
	}
	if turnKey == loop.TurnKey {
		var cycleTaken bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM open_loops WHERE turn_key=? AND cycle=? AND id<>?)`, turnKey, loop.Cycle, loop.ID).Scan(&cycleTaken); err != nil {
			return "", 0, err
		}
		if !cycleTaken {
			return turnKey, loop.Cycle, nil
		}
	}
	var cycle int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(cycle),0)+1 FROM open_loops WHERE turn_key=?`, turnKey).Scan(&cycle); err != nil {
		return "", 0, err
	}
	return turnKey, cycle, nil
}

func findMergedLoopCollision(ctx context.Context, tx *loggedTx, source OpenLoop, destinationID int64) (OpenLoop, bool, error) {
	predicate := "l.turn_key=?"
	args := []any{source.ID, source.TurnKey}
	if source.Detector == "turn_state" {
		predicate += " OR (l.conversation_id=? AND l.kind=? AND l.detector='turn_state')"
		args = append(args, destinationID, source.Kind)
	}
	query := "SELECT " + openLoopColumns + " FROM open_loops l WHERE l.id<>? AND l.status<>'closed' AND (" + predicate + ") ORDER BY CASE WHEN l.conversation_id=? THEN 0 ELSE 1 END,l.id LIMIT 1"
	args = append(args, destinationID)
	loop, err := scanOpenLoop(tx.QueryRowContext(ctx, query, args...))
	if errors.Is(err, ErrLoopNotFound) {
		return OpenLoop{}, false, nil
	}
	if err != nil {
		return OpenLoop{}, false, err
	}
	return loop, true, nil
}

func mergeLoopOwnerState(ctx context.Context, tx *loggedTx, source, destination OpenLoop, nowExpr string) error {
	snoozedUntil := laterLoopTime(source.SnoozedUntil, destination.SnoozedUntil)
	var agendaState string
	switch {
	case source.AgendaState == "created" || destination.AgendaState == "created":
		agendaState = "created"
	case source.AgendaState == "dismissed" || destination.AgendaState == "dismissed":
		agendaState = "dismissed"
	case source.AgendaState == "suggested" || destination.AgendaState == "suggested":
		agendaState = "suggested"
	default:
		agendaState = "none"
	}
	fulfilmentSuggestedAt := laterLoopTime(source.FulfilmentSuggestedAt, destination.FulfilmentSuggestedAt)
	_, err := tx.ExecContext(ctx, `UPDATE open_loops SET snoozed_until=?,agenda_state=?,fulfilment_suggested_at=?,updated_at=`+nowExpr+`
WHERE id=? AND status<>'closed'`, nullableLoopTime(snoozedUntil), agendaState, nullableLoopTime(fulfilmentSuggestedAt), destination.ID)
	return err
}

func laterLoopTime(left, right *time.Time) *time.Time {
	if left == nil {
		return right
	}
	if right == nil || left.After(*right) {
		return left
	}
	return right
}

func nullableLoopTime(value *time.Time) sql.NullTime {
	if value == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *value, Valid: true}
}
