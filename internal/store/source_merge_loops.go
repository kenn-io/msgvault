package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

func (s *Store) transferMergedLoopState(
	ctx context.Context,
	tx *loggedTx,
	conversations []mergeConversation,
	messageSurvivors, messageSurvivorConversations map[int64]int64,
	survivorSourceConversations map[int64]bool,
) error {
	conversationIDs := make(map[int64]int64, len(conversations))
	for _, conversation := range conversations {
		conversationIDs[conversation.id] = conversation.destination
	}
	for _, conversation := range conversations {
		if conversation.id == conversation.destination && !survivorSourceConversations[conversation.id] {
			continue
		}
		if err := s.transferMergedConversationLoopState(
			ctx, tx, conversation.id, conversation.destination, conversationIDs,
			messageSurvivors, messageSurvivorConversations,
		); err != nil {
			return fmt.Errorf("transfer loop state from conversation %d to %d: %w", conversation.id, conversation.destination, err)
		}
	}
	return nil
}

func (s *Store) transferMergedConversationLoopState(
	ctx context.Context,
	tx *loggedTx,
	from, into int64,
	conversationIDs, messageSurvivors, messageSurvivorConversations map[int64]int64,
) error {
	if from != into {
		if err := transferConversationLoopSuppression(ctx, tx, from, into); err != nil {
			return err
		}
	}
	var intoMuted bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM loop_suppressions
WHERE target_kind='conversation' AND conversation_id=?)`, into).Scan(&intoMuted); err != nil {
		return err
	}
	if intoMuted {
		if _, err := tx.ExecContext(ctx, `UPDATE open_loops SET status='closed',closed_at=`+s.dialect.Now()+`,
closed_reason='muted',closed_by='user',closed_message_id=NULL,updated_at=`+s.dialect.Now()+`
WHERE conversation_id=? AND status<>'closed'`, into); err != nil {
			return err
		}
	}
	loops, err := listLoopsQuery(ctx, tx, "SELECT "+openLoopColumns+" FROM open_loops l WHERE l.conversation_id=? ORDER BY l.id", from)
	if err != nil {
		return err
	}
	for _, loop := range loops {
		targetConversation, messageTarget := mergedLoopTargetConversation(loop, conversationIDs, messageSurvivorConversations)
		if from == into && !messageTarget {
			continue
		}
		if !messageTarget {
			targetConversation = into
		}
		var muted bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM loop_suppressions
WHERE target_kind='conversation' AND conversation_id=?)`, targetConversation).Scan(&muted); err != nil {
			return err
		}
		if muted {
			if _, err := tx.ExecContext(ctx, `UPDATE open_loops SET status='closed',closed_at=`+s.dialect.Now()+`,
closed_reason='muted',closed_by='user',closed_message_id=NULL,updated_at=`+s.dialect.Now()+`
WHERE conversation_id=? AND status<>'closed'`, targetConversation); err != nil {
				return err
			}
		}
		anchor := remapMergedMessageID(loop.AnchorMessageID, messageSurvivors)
		lastActivity := remapMergedMessageID(loop.LastActivityMessageID, messageSurvivors)
		closedMessage := remapMergedMessageID(loop.ClosedMessageID, messageSurvivors)
		evidence, err := remapMergedLoopEvidence(loop.EvidenceJSON, conversationIDs, messageSurvivors, messageSurvivorConversations)
		if err != nil {
			return fmt.Errorf("remap evidence for loop %d: %w", loop.ID, err)
		}
		reason, by := "", ""
		if loop.Status != "closed" {
			if muted {
				reason, by = "muted", "user"
			} else {
				collision, found, err := findMergedLoopCollision(ctx, tx, loop, targetConversation)
				if err != nil {
					return err
				}
				if found {
					reason, by = "withdrawn", "system"
					if collision.ConversationID == targetConversation {
						if err := mergeLoopOwnerState(ctx, tx, loop, collision, s.dialect.Now()); err != nil {
							return err
						}
					}
				}
			}
		}
		turnKey, cycle, err := remappedMergedTurnKey(ctx, tx, loop, targetConversation, anchor)
		if err != nil {
			return fmt.Errorf("remap turn key for loop %d: %w", loop.ID, err)
		}
		var terminalHistory bool
		if loop.Status != "closed" {
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM open_loops
WHERE turn_key=? AND id<>? AND status='closed' AND closed_reason IN ('done','fulfilled','expired'))`, turnKey, loop.ID).Scan(&terminalHistory); err != nil {
				return err
			}
			if terminalHistory && reason == "" {
				reason, by = "withdrawn", "system"
			}
		}
		if reason != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE open_loops SET status='closed',closed_at=`+s.dialect.Now()+`,
closed_reason=?,closed_by=?,closed_message_id=NULL,updated_at=`+s.dialect.Now()+`
WHERE id=? AND status<>'closed'`, reason, by, loop.ID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE open_loops SET turn_key=?,cycle=?,conversation_id=?,anchor_message_id=?,last_activity_message_id=?,closed_message_id=?,evidence_json=`+s.dialect.JSONBindExpr()+` WHERE id=?`, turnKey, cycle, targetConversation, anchor, lastActivity, closedMessage, string(evidence), loop.ID); err != nil {
			return err
		}
		if loopTerminal(loop) || terminalHistory {
			if _, err := tx.ExecContext(ctx, `UPDATE open_loops SET status='closed',closed_at=`+s.dialect.Now()+`,
closed_reason='withdrawn',closed_by='system',closed_message_id=NULL,updated_at=`+s.dialect.Now()+`
WHERE turn_key=? AND id<>? AND status<>'closed'`, turnKey, loop.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func transferConversationLoopSuppression(ctx context.Context, tx *loggedTx, from, into int64) error {
	var sourceID, destinationID int64
	var sourceNote, destinationNote string
	sourceErr := tx.QueryRowContext(ctx, `SELECT id,note FROM loop_suppressions WHERE target_kind='conversation' AND conversation_id=?`, from).Scan(&sourceID, &sourceNote)
	if errors.Is(sourceErr, sql.ErrNoRows) {
		return nil
	}
	if sourceErr != nil {
		return sourceErr
	}
	destinationErr := tx.QueryRowContext(ctx, `SELECT id,note FROM loop_suppressions WHERE target_kind='conversation' AND conversation_id=?`, into).Scan(&destinationID, &destinationNote)
	if errors.Is(destinationErr, sql.ErrNoRows) {
		_, err := tx.ExecContext(ctx, `UPDATE loop_suppressions SET conversation_id=? WHERE id=?`, into, sourceID)
		return err
	}
	if destinationErr != nil {
		return destinationErr
	}
	note := combineLoopSuppressionNotes(destinationNote, sourceNote)
	if _, err := tx.ExecContext(ctx, `UPDATE loop_suppressions SET note=? WHERE id=?`, note, destinationID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM loop_suppressions WHERE id=?`, sourceID)
	return err
}

func combineLoopSuppressionNotes(destination, source string) string {
	if source == "" || source == destination {
		return destination
	}
	if destination == "" {
		return source
	}
	if slices.Contains(strings.Split(destination, "\n"), source) {
		return destination
	}
	return destination + "\n" + source
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

func remapMergedLoopEvidence(
	raw jsontext.Value,
	conversations, survivors, survivorConversations map[int64]int64,
) (jsontext.Value, error) {
	var evidence []map[string]jsontext.Value
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return nil, err
	}
	changed := false
	for _, entry := range evidence {
		var messageID int64
		if encodedID, ok := entry["message_id"]; ok {
			if err := json.Unmarshal(encodedID, &messageID); err != nil {
				return nil, fmt.Errorf("decode message_id: %w", err)
			}
			if replacement, ok := survivors[messageID]; ok && replacement != messageID {
				entry["message_id"] = jsontext.Value(strconv.FormatInt(replacement, 10))
				changed = true
			}
		}
		if encodedID, ok := entry["conversation_id"]; ok {
			var conversationID int64
			if err := json.Unmarshal(encodedID, &conversationID); err != nil {
				return nil, fmt.Errorf("decode conversation_id: %w", err)
			}
			replacement, ok := survivorConversations[messageID]
			if !ok {
				replacement, ok = conversations[conversationID]
			}
			if ok && replacement != conversationID {
				entry["conversation_id"] = jsontext.Value(strconv.FormatInt(replacement, 10))
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

func mergedLoopTargetConversation(
	loop OpenLoop,
	conversations, survivorConversations map[int64]int64,
) (int64, bool) {
	targetConversation, ok := conversations[loop.ConversationID]
	if !ok {
		targetConversation = loop.ConversationID
	}
	messageTarget := false
	for _, messageID := range []*int64{loop.AnchorMessageID, loop.LastActivityMessageID, loop.ClosedMessageID} {
		if messageID == nil {
			continue
		}
		if _, ok := survivorConversations[*messageID]; ok {
			messageTarget = true
			break
		}
	}
	if loop.AnchorMessageID != nil {
		if conversationID, ok := survivorConversations[*loop.AnchorMessageID]; ok {
			return conversationID, messageTarget
		}
		return targetConversation, messageTarget
	}
	for _, messageID := range []*int64{loop.LastActivityMessageID, loop.ClosedMessageID} {
		if messageID == nil {
			continue
		}
		if conversationID, ok := survivorConversations[*messageID]; ok {
			return conversationID, messageTarget
		}
	}
	return targetConversation, messageTarget
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
