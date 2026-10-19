package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *Store) applySourceMergePlan(ctx context.Context, tx *loggedTx, from, into int64, plan sourceMergePlan) error {
	conversations := map[int64]int64{}
	for _, c := range plan.conversations {
		if _, err := tx.ExecContext(ctx, `INSERT INTO source_merge_conversations(conversation_id, original_source_id, original_conversation_id, destination_conversation_id) VALUES (?, ?, ?, ?) ON CONFLICT (conversation_id) DO UPDATE SET destination_conversation_id = excluded.destination_conversation_id`, c.id, from, c.providerID, c.destination); err != nil {
			return err
		}
		conversations[c.id] = c.destination
		if c.destination == c.id {
			if _, err := tx.ExecContext(ctx, `UPDATE conversations SET source_id = ?, source_conversation_id = ? WHERE id = ?`, into, c.archivalID, c.id); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_participants(conversation_id, participant_id, role, joined_at, left_at)
 SELECT ?, participant_id, role, joined_at, left_at FROM conversation_participants WHERE conversation_id = ?
 ON CONFLICT (conversation_id, participant_id) DO NOTHING`, c.destination, c.id); err != nil {
				return err
			}
		}
	}
	// Batch the ordinary row transfer and provenance writes. The values table
	// keeps provider IDs and hidden-survivor decisions in the same transaction.
	for start := 0; start < len(plan.messages); start += 100 {
		batch := plan.messages[start:min(start+100, len(plan.messages))]
		values := make([]string, 0, len(batch))
		args := make([]any, 0, len(batch)*6)
		for _, m := range batch {
			var survivor any
			basis := ""
			if m.survivor != 0 {
				survivor = m.survivor
				basis = "payload-fingerprint"
				if m.kind.String == MessageTypeEmail && m.rfcID.String != "" {
					basis = "rfc822-or-payload"
				}
			}
			values = append(values, "(CAST(? AS BIGINT), CAST(? AS BIGINT), ?, ?, CAST(? AS BIGINT), ?)")
			args = append(args, m.id, conversations[m.conversationID], m.archivalID, m.providerID, survivor, basis)
		}
		incoming := `WITH incoming(id, conversation_id, archival_id, provider_id, survivor_id, match_basis) AS (VALUES ` + strings.Join(values, ",") + `) `
		if _, err := tx.ExecContext(ctx, incoming+`UPDATE messages SET source_id = ?,
 conversation_id = (SELECT conversation_id FROM incoming WHERE incoming.id = messages.id),
 source_message_id = (SELECT archival_id FROM incoming WHERE incoming.id = messages.id), embed_gen = NULL,
 deleted_at = CASE WHEN deleted_from_source_at IS NULL AND
 (SELECT survivor_id FROM incoming WHERE incoming.id = messages.id) IS NOT NULL
 THEN COALESCE(deleted_at, `+s.dialect.Now()+`) ELSE deleted_at END
 WHERE id IN (SELECT id FROM incoming)`, append(args, into)...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, incoming+`INSERT INTO source_merge_messages(message_id, original_source_id, original_message_id, survivor_message_id, archive_only, match_basis)
 SELECT id, ?, provider_id, survivor_id, TRUE, match_basis FROM incoming WHERE TRUE
 ON CONFLICT (message_id) DO UPDATE SET survivor_message_id = COALESCE(excluded.survivor_message_id, source_merge_messages.survivor_message_id),
 archive_only = TRUE, match_basis = CASE WHEN excluded.match_basis != '' THEN excluded.match_basis ELSE source_merge_messages.match_basis END`, append(args, from)...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, incoming+`INSERT INTO source_merge_archive_only_messages(message_id, original_source_message_id)
 SELECT id, COALESCE(provider_id, '') FROM incoming WHERE TRUE
 ON CONFLICT (message_id) DO UPDATE SET original_source_message_id =
 CASE WHEN source_merge_archive_only_messages.original_source_message_id = '' THEN excluded.original_source_message_id
 ELSE source_merge_archive_only_messages.original_source_message_id END`, args...); err != nil {
			return err
		}
	}
	if err := s.transferSourceMergeLoopState(ctx, tx, into, plan.conversations, plan.messages); err != nil {
		return fmt.Errorf("transfer source merge loop state: %w", err)
	}
	// Gmail's canonical IDs also carry trusted roles. Preserve that evidence
	// before archive labels lose provider IDs; Drafts takes precedence, while
	// category roles keep loop bulk exclusions intact after source merges.
	if _, err := tx.ExecContext(ctx, `UPDATE labels SET system_role = CASE
		WHEN system_role = ? OR source_label_id = 'DRAFT' THEN ?
		WHEN source_label_id = 'SENT' THEN ?
		WHEN source_label_id = 'CATEGORY_PERSONAL' THEN ?
		WHEN source_label_id = 'CATEGORY_SOCIAL' THEN ?
		WHEN source_label_id = 'CATEGORY_PROMOTIONS' THEN ?
		WHEN source_label_id = 'CATEGORY_UPDATES' THEN ?
		WHEN source_label_id = 'CATEGORY_FORUMS' THEN ?
		END
		WHERE source_id IN (?, ?) AND source_label_id IN ('SENT', 'DRAFT', 'CATEGORY_PERSONAL', 'CATEGORY_SOCIAL', 'CATEGORY_PROMOTIONS', 'CATEGORY_UPDATES', 'CATEGORY_FORUMS')
		AND EXISTS (SELECT 1 FROM sources s WHERE s.id = labels.source_id AND s.source_type = 'gmail')`,
		LabelSystemRoleDrafts, LabelSystemRoleDrafts, LabelSystemRoleSent,
		labelSystemRoleGmailCategoryPersonal, labelSystemRoleGmailCategorySocial,
		labelSystemRoleGmailCategoryPromotions, labelSystemRoleGmailCategoryUpdates,
		labelSystemRoleGmailCategoryForums, from, into); err != nil {
		return err
	}
	// Incoming labels are archive organization, not destination provider label IDs.
	rows, err := tx.QueryContext(ctx, `SELECT l.id, l.name, COALESCE(l.system_role, ''), EXISTS(SELECT 1 FROM source_merge_preserved_labels p WHERE p.label_id = l.id) FROM labels l WHERE l.source_id = ? ORDER BY l.id`, from)
	if err != nil {
		return err
	}
	type label struct {
		id         int64
		name, role string
		preserved  bool
	}
	var labels []label
	for rows.Next() {
		var l label
		if err := rows.Scan(&l.id, &l.name, &l.role, &l.preserved); err != nil {
			_ = rows.Close()
			return err
		}
		labels = append(labels, l)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, l := range labels {
		var target int64
		var targetRole string
		err := tx.QueryRowContext(ctx, `SELECT id, COALESCE(system_role, '') FROM labels WHERE source_id = ? AND name = ?`, into, l.name).Scan(&target, &targetRole)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && (targetRole != l.role || l.preserved) {
			// Sharing a name does not prove the same provider role. Keep the
			// incoming role without changing the destination label's meaning.
			// Previously archived roles also stay separate across later merges.
			target = 0
			l.name = fmt.Sprintf("msgvault-archive:%d:label:%d", from, l.id)
			for {
				var used bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM labels WHERE source_id = ? AND name = ?)`, into, l.name).Scan(&used); err != nil {
					return err
				}
				if !used {
					break
				}
				l.name += ":archive"
			}
		}
		if target == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE labels SET source_id = ?, name = ?, source_label_id = NULL WHERE id = ?`, into, l.name, l.id); err != nil {
				return err
			}
			if l.role != "" {
				if _, err := tx.ExecContext(ctx, `INSERT INTO source_merge_preserved_labels(label_id) VALUES (?) ON CONFLICT (label_id) DO NOTHING`, l.id); err != nil {
					return err
				}
			}
		} else {
			if _, err := tx.ExecContext(ctx, `INSERT INTO message_labels(message_id, label_id) SELECT message_id, ? FROM message_labels WHERE label_id = ? ON CONFLICT (message_id, label_id) DO NOTHING`, target, l.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM message_labels WHERE label_id = ?`, l.id); err != nil {
				return err
			}
			// Retain the old definition for historical source inspection.
		}
	}
	for _, m := range plan.messages {
		if m.survivor == 0 {
			continue
		}
		if err := copyMergedMessageEvidence(ctx, tx, m.id, m.survivor, from); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET reply_to_message_id = ? WHERE reply_to_message_id = ?`, m.survivor, m.id); err != nil {
			return err
		}
	}
	if err := s.mergeSourceIdentityEvidence(ctx, tx, from, into); err != nil {
		return err
	}
	if err := refreshSourceMessageAttributionContext(ctx, tx, into); err != nil {
		return err
	}
	// Moved mail uses a new mailbox, and incoming aliases or a Sent folder can
	// change attribution for existing destination mail too. Mark these rows in
	// the merge transaction; derive them in pages after it commits.
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET account_address = NULL, account_path = NULL
 WHERE source_id = ? AND COALESCE(message_type, '') IN ('', 'email', 'calendar_event')`, into); err != nil {
		return fmt.Errorf("mark merged account attribution pending: %w", err)
	}
	// Source-local receipts, sync runs, cursors, re-anchor markers, import receipts
	// and Beeper media endpoint records remain on the retired source for audit.
	for _, statement := range []string{
		`INSERT INTO collection_sources(collection_id, source_id) SELECT collection_id, ? FROM collection_sources WHERE source_id = ? ON CONFLICT (collection_id, source_id) DO NOTHING`,
		`UPDATE document_occurrences SET source_id = ? WHERE source_id = ?`,
		`UPDATE activity_events SET source_id = ? WHERE source_id = ?`,
		`UPDATE activity_events SET owner_source_id = ? WHERE owner_source_id = ?`,
		`UPDATE person_contact_state SET last_contact_source_id = ? WHERE last_contact_source_id = ?`,
		`UPDATE open_loops SET source_id = ? WHERE source_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, into, from); err != nil {
			return err
		}
	}
	messageSurvivors := make(map[int64]int64)
	for _, message := range plan.messages {
		if message.survivor != 0 {
			messageSurvivors[message.id] = message.survivor
		}
	}
	if err := s.transferMergedLoopState(ctx, tx, plan.conversations, messageSurvivors); err != nil {
		return err
	}
	// Observation rows and accepted-match endpoints retain their immutable IDs.
	// A duplicate current observation becomes historical rather than being deleted.
	if _, err := tx.ExecContext(ctx, `UPDATE participant_contact_observations SET superseded_at = `+s.dialect.Now()+`
 WHERE source_id = ? AND active_until IS NULL AND superseded_at IS NULL
 AND EXISTS (SELECT 1 FROM participant_contact_observations b WHERE b.source_id = ?
 AND b.participant_id = participant_contact_observations.participant_id
 AND b.address_kind = participant_contact_observations.address_kind
 AND (b.service_id = participant_contact_observations.service_id OR
      (b.service_id IS NULL AND participant_contact_observations.service_id IS NULL))
 AND (b.scope_kind = participant_contact_observations.scope_kind OR
      (b.scope_kind IS NULL AND participant_contact_observations.scope_kind IS NULL))
 AND (b.scope_value = participant_contact_observations.scope_value OR
      (b.scope_value IS NULL AND participant_contact_observations.scope_value IS NULL))
 AND b.normalized_value = participant_contact_observations.normalized_value AND b.active_until IS NULL AND b.superseded_at IS NULL)`, from, into); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE participant_contact_observations SET source_id = ? WHERE source_id = ?`, into, from); err != nil {
		return err
	}
	for _, relation := range []struct{ table, key string }{{"identity_match_candidate_sources", "candidate_id"}, {"identity_match_evidence_sources", "evidence_id"}} {
		statement := fmt.Sprintf(`INSERT INTO %s(%s, source_id, is_conservative) SELECT %s, ?, is_conservative FROM %s WHERE source_id = ?
 ON CONFLICT (%s, source_id) DO UPDATE SET is_conservative = %s.is_conservative OR excluded.is_conservative`, relation.table, relation.key, relation.key, relation.table, relation.key, relation.table)
		if _, err := tx.ExecContext(ctx, statement, into, from); err != nil {
			return err
		}
	}
	if err := s.reconcileCurrentObservationIdentityMatchesTxContext(ctx, tx); err != nil {
		return fmt.Errorf("reconcile merged contact observation identity matches: %w", err)
	}
	// Queue triggers cover changed source/conversation/deletion inputs. Counts
	// and previews must ignore the retained hidden payloads immediately.
	for _, c := range plan.conversations {
		for _, id := range []int64{c.id, c.destination} {
			if err := s.recomputeConversationStatsWith(boundQuerier{ctx: ctx, q: tx}, "id = ?", id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE conversations SET message_count = (SELECT COUNT(*) FROM messages WHERE conversation_id = conversations.id AND deleted_at IS NULL AND deleted_from_source_at IS NULL) WHERE id = ?`, id); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE document_index_state SET revision = revision + 1, updated_at = `+s.dialect.Now()+` WHERE singleton = 1`); err != nil {
		return err
	}
	if _, err := s.bumpIdentityRevisionContext(ctx, tx); err != nil {
		return err
	}
	if err := s.bumpAccountIdentityRevisionContext(ctx, tx); err != nil {
		return err
	}
	return s.bumpDerivedDataRevision(tx)
}

func (s *Store) transferSourceMergeLoopState(
	ctx context.Context,
	tx *loggedTx,
	destinationSourceID int64,
	conversations []mergeConversation,
	messages []mergeMessage,
) error {
	survivors := make(map[int64]int64)
	survivorConversationIDs, err := sourceMergeSurvivorConversationIDs(ctx, tx, messages)
	if err != nil {
		return fmt.Errorf("load surviving conversation IDs: %w", err)
	}
	destinationsByConversation := make(map[int64][]int64, len(conversations))
	seenDestinations := make(map[int64]map[int64]struct{}, len(conversations))
	activeUniqueMessages := make(map[int64]bool, len(conversations))
	addDestination := func(sourceConversationID, destinationConversationID int64) {
		if destinationConversationID <= 0 {
			return
		}
		seen := seenDestinations[sourceConversationID]
		if seen == nil {
			seen = make(map[int64]struct{})
			seenDestinations[sourceConversationID] = seen
		}
		if _, ok := seen[destinationConversationID]; ok {
			return
		}
		seen[destinationConversationID] = struct{}{}
		destinationsByConversation[sourceConversationID] = append(destinationsByConversation[sourceConversationID], destinationConversationID)
	}
	for _, message := range messages {
		if message.survivor != 0 {
			survivors[message.id] = message.survivor
			destinationConversationID, ok := survivorConversationIDs[message.survivor]
			if !ok {
				return fmt.Errorf("surviving message %d has no conversation", message.survivor)
			}
			addDestination(message.conversationID, destinationConversationID)
		} else if !message.archiveOnly && !message.deleted.Valid && !message.removed.Valid {
			activeUniqueMessages[message.conversationID] = true
		}
	}
	for _, conversation := range conversations {
		if conversation.destination != conversation.id || activeUniqueMessages[conversation.id] || len(destinationsByConversation[conversation.id]) == 0 {
			addDestination(conversation.id, conversation.destination)
		}
	}
	for _, conversation := range conversations {
		if err := s.transferSourceMergeConversationSuppression(ctx, tx, conversation.id, destinationsByConversation[conversation.id]); err != nil {
			return fmt.Errorf("transfer conversation mute from %d: %w", conversation.id, err)
		}
		if err := s.transferSourceMergeLoopReferences(ctx, tx, conversation.id, conversation.destination, destinationSourceID, survivors); err != nil {
			return fmt.Errorf("transfer loops for conversation %d to %d: %w", conversation.id, conversation.destination, err)
		}
	}
	closedDestinations := make(map[int64]struct{})
	for _, conversation := range conversations {
		for _, destinationConversationID := range destinationsByConversation[conversation.id] {
			if _, seen := closedDestinations[destinationConversationID]; seen {
				continue
			}
			closedDestinations[destinationConversationID] = struct{}{}
			if err := s.closeMutedSourceMergeDestinationLoops(ctx, tx, destinationConversationID); err != nil {
				return fmt.Errorf("close loops in muted source-merge destination %d: %w", destinationConversationID, err)
			}
		}
	}
	return nil
}

func (s *Store) closeMutedSourceMergeDestinationLoops(ctx context.Context, tx *loggedTx, conversationID int64) error {
	var muted bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM loop_suppressions
		WHERE target_kind = 'conversation' AND conversation_id = ?)`, conversationID).Scan(&muted); err != nil {
		return err
	}
	if !muted {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE open_loops SET status = 'closed',
		closed_at = `+s.dialect.Now()+`, closed_reason = 'muted', closed_by = 'user',
		closed_message_id = NULL, updated_at = `+s.dialect.Now()+`
		WHERE conversation_id = ? AND status <> 'closed'`, conversationID)
	return err
}

func sourceMergeSurvivorConversationIDs(ctx context.Context, tx *loggedTx, messages []mergeMessage) (map[int64]int64, error) {
	var ids []int64
	seen := make(map[int64]struct{})
	for _, message := range messages {
		if message.survivor == 0 {
			continue
		}
		if _, ok := seen[message.survivor]; ok {
			continue
		}
		seen[message.survivor] = struct{}{}
		ids = append(ids, message.survivor)
	}
	conversationIDs := make(map[int64]int64, len(ids))
	for start := 0; start < len(ids); start += 100 {
		batch := ids[start:min(start+100, len(ids))]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, conversation_id FROM messages WHERE id IN (`+strings.Join(placeholders, ",")+")", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var messageID, conversationID int64
			if err := rows.Scan(&messageID, &conversationID); err != nil {
				_ = rows.Close()
				return nil, err
			}
			conversationIDs[messageID] = conversationID
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	return conversationIDs, nil
}

func (s *Store) transferSourceMergeConversationSuppression(
	ctx context.Context,
	tx *loggedTx,
	sourceConversationID int64,
	destinationConversationIDs []int64,
) error {
	var absorbed struct {
		id        int64
		note      string
		createdAt time.Time
	}
	err := tx.QueryRowContext(ctx, `SELECT id, note, created_at FROM loop_suppressions
		WHERE target_kind = 'conversation' AND conversation_id = ?`, sourceConversationID).
		Scan(&absorbed.id, &absorbed.note, &absorbed.createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(destinationConversationIDs) == 0 {
		return nil
	}
	sourceMuteIDAvailable := true
	for _, destinationConversationID := range destinationConversationIDs {
		var survivor struct {
			id        int64
			note      string
			createdAt time.Time
		}
		err = tx.QueryRowContext(ctx, `SELECT id, note, created_at FROM loop_suppressions
			WHERE target_kind = 'conversation' AND conversation_id = ?`, destinationConversationID).
			Scan(&survivor.id, &survivor.note, &survivor.createdAt)
		if errors.Is(err, sql.ErrNoRows) {
			if sourceMuteIDAvailable {
				_, err = tx.ExecContext(ctx, `UPDATE loop_suppressions SET conversation_id = ? WHERE id = ?`, destinationConversationID, absorbed.id)
				if err != nil {
					return err
				}
			} else {
				_, err = tx.ExecContext(ctx, `INSERT INTO loop_suppressions(target_kind, conversation_id, note, created_at)
					VALUES ('conversation', ?, ?, ?)`, destinationConversationID, absorbed.note, s.dialect.TimestampParam(absorbed.createdAt))
				if err != nil {
					return err
				}
			}
			sourceMuteIDAvailable = false
			continue
		}
		if err != nil {
			return err
		}
		if survivor.id == absorbed.id {
			sourceMuteIDAvailable = false
			continue
		}
		createdAt := survivor.createdAt
		if absorbed.createdAt.Before(createdAt) {
			createdAt = absorbed.createdAt
		}
		if _, err := tx.ExecContext(ctx, `UPDATE loop_suppressions SET note = ?, created_at = ? WHERE id = ?`,
			mergeLoopSuppressionNotes(survivor.note, absorbed.note), s.dialect.TimestampParam(createdAt), survivor.id); err != nil {
			return err
		}
		if sourceMuteIDAvailable {
			if _, err := tx.ExecContext(ctx, `DELETE FROM loop_suppressions WHERE id = ?`, absorbed.id); err != nil {
				return err
			}
			sourceMuteIDAvailable = false
		}
	}
	return nil
}

type sourceMergeLiveLoop struct {
	id                    int64
	kind                  string
	lastActivityAt        time.Time
	hasKata               bool
	snoozedUntil          sql.NullTime
	agendaState           string
	fulfilmentSuggestedAt sql.NullTime
	evidence              []byte
}

func (s *Store) resolveSourceMergeLiveLoopCollision(
	ctx context.Context,
	tx *loggedTx,
	sourceLoopID, destinationConversationID int64,
) error {
	var sourceLoop sourceMergeLiveLoop
	err := tx.QueryRowContext(ctx, `SELECT id, kind, last_activity_at,
		EXISTS (SELECT 1 FROM loop_kata_links k WHERE k.loop_id = open_loops.id),
		snoozed_until, agenda_state, fulfilment_suggested_at, evidence_json FROM open_loops
		WHERE id = ? AND detector = 'turn_state' AND status <> 'closed'`, sourceLoopID).
		Scan(&sourceLoop.id, &sourceLoop.kind, &sourceLoop.lastActivityAt, &sourceLoop.hasKata, &sourceLoop.snoozedUntil,
			&sourceLoop.agendaState, &sourceLoop.fulfilmentSuggestedAt, &sourceLoop.evidence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var destinationLoop sourceMergeLiveLoop
	err = tx.QueryRowContext(ctx, `SELECT id, kind, last_activity_at,
		EXISTS (SELECT 1 FROM loop_kata_links k WHERE k.loop_id = open_loops.id),
		snoozed_until, agenda_state, fulfilment_suggested_at, evidence_json FROM open_loops
		WHERE conversation_id = ? AND kind = ? AND detector = 'turn_state' AND status <> 'closed'
		ORDER BY last_activity_at DESC, id LIMIT 1`, destinationConversationID, sourceLoop.kind).
		Scan(&destinationLoop.id, &destinationLoop.kind, &destinationLoop.lastActivityAt, &destinationLoop.hasKata,
			&destinationLoop.snoozedUntil, &destinationLoop.agendaState,
			&destinationLoop.fulfilmentSuggestedAt, &destinationLoop.evidence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	winner, retired := &destinationLoop, &sourceLoop
	if sourceLoop.hasKata != destinationLoop.hasKata {
		if sourceLoop.hasKata {
			winner, retired = &sourceLoop, &destinationLoop
		}
	} else if sourceLoop.lastActivityAt.After(destinationLoop.lastActivityAt) {
		winner, retired = &sourceLoop, &destinationLoop
	}
	if err := s.transferSourceMergeLiveLoopOwnerState(ctx, tx, winner, retired); err != nil {
		return fmt.Errorf("transfer owner state from loop %d to %d: %w", retired.id, winner.id, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE open_loops SET status = 'closed',
		closed_at = `+s.dialect.Now()+`, closed_reason = 'withdrawn', closed_by = 'system',
		closed_message_id = NULL, updated_at = `+s.dialect.Now()+`
		WHERE id = ? AND status <> 'closed'`, retired.id); err != nil {
		return err
	}
	return nil
}

func (s *Store) transferSourceMergeLoopReferences(
	ctx context.Context,
	tx *loggedTx,
	sourceConversationID, destinationConversationID, destinationSourceID int64,
	messageSurvivors map[int64]int64,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, turn_key, cycle, kind, detector,
		anchor_message_id, last_activity_message_id, closed_message_id, evidence_json
		FROM open_loops WHERE conversation_id = ? ORDER BY id`, sourceConversationID)
	if err != nil {
		return err
	}
	type loopRow struct {
		id                    int64
		turnKey               string
		cycle                 int64
		kind, detector        string
		anchor, activity, end sql.NullInt64
		evidence              []byte
	}
	var loops []loopRow
	for rows.Next() {
		var loop loopRow
		if err := rows.Scan(&loop.id, &loop.turnKey, &loop.cycle, &loop.kind, &loop.detector,
			&loop.anchor, &loop.activity, &loop.end, &loop.evidence); err != nil {
			_ = rows.Close()
			return err
		}
		loops = append(loops, loop)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, loop := range loops {
		anchor := remapSourceMergeLoopMessageID(loop.anchor, messageSurvivors)
		activity := remapSourceMergeLoopMessageID(loop.activity, messageSurvivors)
		closedMessage := remapSourceMergeLoopMessageID(loop.end, messageSurvivors)
		loopConversationID := destinationConversationID
		if anchor.Valid {
			var anchorConversationID int64
			if err := tx.QueryRowContext(ctx, `SELECT conversation_id FROM messages WHERE id = ?`, anchor.Int64).Scan(&anchorConversationID); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("find loop %d surviving anchor conversation: %w", loop.id, err)
			} else if anchorConversationID != 0 {
				loopConversationID = anchorConversationID
			}
		}
		loopEvidence := loop.evidence
		if loopConversationID != sourceConversationID {
			if err := s.resolveSourceMergeLiveLoopCollision(ctx, tx, loop.id, loopConversationID); err != nil {
				return fmt.Errorf("resolve live loop %d collision in conversation %d: %w", loop.id, loopConversationID, err)
			}
			if err := tx.QueryRowContext(ctx, `SELECT evidence_json FROM open_loops WHERE id = ?`, loop.id).Scan(&loopEvidence); err != nil {
				return fmt.Errorf("reload loop %d evidence after collision: %w", loop.id, err)
			}
		}
		evidence, err := remapSourceMergeLoopEvidence(loopEvidence, sourceConversationID, loopConversationID, messageSurvivors)
		if err != nil {
			return fmt.Errorf("remap evidence for loop %d: %w", loop.id, err)
		}
		turnKey, cycle := loop.turnKey, loop.cycle
		if loop.detector == "turn_state" && loop.anchor.Valid &&
			loop.turnKey == fmt.Sprintf("turn:%d:%s:%d", sourceConversationID, loop.kind, loop.anchor.Int64) {
			turnKey = fmt.Sprintf("turn:%d:%s:%d", loopConversationID, loop.kind, anchor.Int64)
			if turnKey != loop.turnKey {
				var maxCycle int64
				if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(cycle), 0) FROM open_loops WHERE turn_key = ? AND id <> ?`, turnKey, loop.id).Scan(&maxCycle); err != nil {
					return err
				}
				if maxCycle >= cycle {
					cycle = maxCycle + 1
				}
			}
		}
		if loop.detector == "turn_state" && loopConversationID != sourceConversationID {
			collapsed, err := s.collapseSourceMergeLoopDuplicate(
				ctx, tx, loop.id, loopConversationID, destinationSourceID, sourceConversationID,
				turnKey, loop.cycle, anchor, activity, closedMessage, messageSurvivors,
			)
			if err != nil {
				return fmt.Errorf("collapse duplicate loop %d in conversation %d: %w", loop.id, loopConversationID, err)
			}
			if collapsed {
				continue
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE open_loops SET conversation_id = ?, source_id = ?,
			turn_key = ?, cycle = ?, anchor_message_id = ?, last_activity_message_id = ?,
			closed_message_id = ?, evidence_json = ? WHERE id = ?`,
			loopConversationID, destinationSourceID, turnKey, cycle, anchor, activity, closedMessage, string(evidence), loop.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) collapseSourceMergeLoopDuplicate(
	ctx context.Context,
	tx *loggedTx,
	sourceLoopID, destinationConversationID, destinationSourceID, sourceConversationID int64,
	turnKey string,
	sourceCycle int64,
	anchor, activity, closedMessage sql.NullInt64,
	messageSurvivors map[int64]int64,
) (bool, error) {
	source, err := scanOpenLoop(tx.QueryRowContext(ctx,
		"SELECT "+openLoopColumns+openLoopFrom+" WHERE l.id=?", sourceLoopID,
	))
	if err != nil {
		return false, err
	}
	destination, err := scanOpenLoop(tx.QueryRowContext(ctx,
		"SELECT "+openLoopColumns+openLoopFrom+` WHERE l.id<>? AND l.conversation_id=? AND l.turn_key=? AND l.cycle=? ORDER BY l.id LIMIT 1`,
		sourceLoopID, destinationConversationID, turnKey, sourceCycle,
	))
	if errors.Is(err, ErrLoopNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if source.Kind != destination.Kind || source.Detector != destination.Detector {
		return false, nil
	}
	winnerIsSource := sourceMergeLoopWins(source, destination)
	if source.Kata != nil && destination.Kata == nil {
		if _, err := tx.ExecContext(ctx, `UPDATE loop_kata_links SET loop_id=? WHERE loop_id=?`, destination.ID, source.ID); err != nil {
			return false, fmt.Errorf("retarget tracked loop link from %d to %d: %w", source.ID, destination.ID, err)
		}
	}
	sourceState := sourceMergeLoopState(source)
	destinationState := sourceMergeLoopState(destination)
	winnerState, retiredState := &destinationState, &sourceState
	if winnerIsSource {
		winnerState, retiredState = &sourceState, &destinationState
	}
	if err := s.transferSourceMergeLiveLoopOwnerState(ctx, tx, winnerState, retiredState); err != nil {
		return false, fmt.Errorf("transfer duplicate loop owner state: %w", err)
	}
	if winnerIsSource {
		winner, err := scanOpenLoop(tx.QueryRowContext(ctx,
			"SELECT "+openLoopColumns+openLoopFrom+" WHERE l.id=?", sourceLoopID,
		))
		if err != nil {
			return false, fmt.Errorf("reload winning source loop: %w", err)
		}
		evidence, err := remapSourceMergeLoopEvidence(
			[]byte(winner.EvidenceJSON), sourceConversationID, destinationConversationID, messageSurvivors,
		)
		if err != nil {
			return false, fmt.Errorf("remap winning loop evidence: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE open_loops SET source_id=?,status=?,channel=?,owner_address=?,
			counterparty_participant_id=?,counterparty_person_id=?,anchor_message_id=?,owed_since_at=?,
			last_activity_message_id=?,last_activity_at=?,opens_at=?,due_at=?,snoozed_until=?,summary=?,
			evidence_json=?,ruleset_version=?,fact_claim_id=?,agenda_state=?,fulfilment_suggested_at=?,
			closed_at=?,closed_reason=?,closed_by=?,closed_message_id=?,updated_at=`+s.dialect.Now()+` WHERE id=?`,
			destinationSourceID, winner.Status, winner.Channel, winner.OwnerAddress,
			winner.CounterpartyParticipantID, winner.CounterpartyPersonID, anchor,
			winner.OwedSinceAt, activity, winner.LastActivityAt, winner.OpensAt,
			winner.DueAt, winner.SnoozedUntil, winner.Summary, string(evidence), winner.RulesetVersion,
			winner.FactClaimID, winner.AgendaState, winner.FulfilmentSuggestedAt,
			winner.ClosedAt, winner.ClosedReason, winner.ClosedBy, closedMessage, destination.ID,
		); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM open_loops WHERE id=?`, sourceLoopID); err != nil {
		return false, fmt.Errorf("delete duplicate source loop: %w", err)
	}
	return true, nil
}

func sourceMergeLoopWins(source, destination OpenLoop) bool {
	sourceTerminal, destinationTerminal := loopTerminal(source), loopTerminal(destination)
	if sourceTerminal != destinationTerminal {
		return sourceTerminal
	}
	if (source.Kata != nil) != (destination.Kata != nil) {
		return source.Kata != nil
	}
	sourceUserClosed := source.Status == "closed" && source.ClosedBy != nil && *source.ClosedBy == "user"
	destinationUserClosed := destination.Status == "closed" && destination.ClosedBy != nil && *destination.ClosedBy == "user"
	if sourceUserClosed != destinationUserClosed {
		return sourceUserClosed
	}
	if source.Status == "closed" && destination.Status != "closed" {
		return sourceUserClosed
	}
	if source.Status != "closed" && destination.Status == "closed" {
		return !destinationUserClosed
	}
	if source.Status == "closed" && destination.Status == "closed" {
		return source.ClosedAt != nil && destination.ClosedAt != nil && source.ClosedAt.After(*destination.ClosedAt)
	}
	return source.LastActivityAt.After(destination.LastActivityAt)
}

func sourceMergeLoopState(loop OpenLoop) sourceMergeLiveLoop {
	state := sourceMergeLiveLoop{
		id: loop.ID, kind: loop.Kind, lastActivityAt: loop.LastActivityAt,
		agendaState: loop.AgendaState, evidence: []byte(loop.EvidenceJSON),
	}
	if loop.SnoozedUntil != nil {
		state.snoozedUntil = sql.NullTime{Time: *loop.SnoozedUntil, Valid: true}
	}
	if loop.FulfilmentSuggestedAt != nil {
		state.fulfilmentSuggestedAt = sql.NullTime{Time: *loop.FulfilmentSuggestedAt, Valid: true}
	}
	return state
}

func remapSourceMergeLoopMessageID(messageID sql.NullInt64, survivors map[int64]int64) sql.NullInt64 {
	if !messageID.Valid {
		return messageID
	}
	if survivor, ok := survivors[messageID.Int64]; ok {
		messageID.Int64 = survivor
	}
	return messageID
}

func remapSourceMergeLoopEvidence(raw []byte, sourceConversationID, destinationConversationID int64, survivors map[int64]int64) ([]byte, error) {
	var evidence []map[string]jsontext.Value
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return nil, fmt.Errorf("decode loop evidence: %w", err)
	}
	changed := false
	for _, item := range evidence {
		for key, remap := range map[string]func(int64) (int64, bool){
			"message_id": func(id int64) (int64, bool) {
				survivor, ok := survivors[id]
				return survivor, ok
			},
			"owner_participation_message_id": func(id int64) (int64, bool) {
				survivor, ok := survivors[id]
				return survivor, ok
			},
			"conversation_id": func(id int64) (int64, bool) {
				if id == sourceConversationID {
					return destinationConversationID, true
				}
				return id, false
			},
		} {
			value, ok := item[key]
			if !ok {
				continue
			}
			var id int64
			if err := json.Unmarshal(value, &id); err != nil {
				continue
			}
			mapped, ok := remap(id)
			if !ok || mapped == id {
				continue
			}
			encoded, err := json.Marshal(mapped)
			if err != nil {
				return nil, err
			}
			item[key] = jsontext.Value(encoded)
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	return json.Marshal(evidence)
}

func (s *Store) transferSourceMergeLiveLoopOwnerState(ctx context.Context, tx *loggedTx, winner, retired *sourceMergeLiveLoop) error {
	evidence, err := mergeSourceMergeLoopOwnerNote(winner.evidence, retired.evidence)
	if err != nil {
		return fmt.Errorf("merge loop owner notes: %w", err)
	}
	var snoozedUntil any
	if retired.snoozedUntil.Valid {
		snoozedUntil = s.dialect.TimestampParam(retired.snoozedUntil.Time)
	}
	var fulfilmentSuggestedAt any
	if retired.fulfilmentSuggestedAt.Valid {
		fulfilmentSuggestedAt = s.dialect.TimestampParam(retired.fulfilmentSuggestedAt.Time)
	}
	_, err = tx.ExecContext(ctx, `UPDATE open_loops SET
		snoozed_until = CASE WHEN ? AND ? > `+s.dialect.Now()+`
			AND (snoozed_until IS NULL OR snoozed_until <= `+s.dialect.Now()+` OR ? > snoozed_until)
			THEN ? ELSE snoozed_until END,
		agenda_state = CASE WHEN agenda_state = 'none' AND ? <> 'none' THEN ? ELSE agenda_state END,
		fulfilment_suggested_at = COALESCE(fulfilment_suggested_at, ?), evidence_json = ?
		WHERE id = ?`, retired.snoozedUntil.Valid, snoozedUntil, snoozedUntil, snoozedUntil,
		retired.agendaState, retired.agendaState, fulfilmentSuggestedAt, string(evidence), winner.id)
	return err
}

func mergeSourceMergeLoopOwnerNote(winnerEvidence, retiredEvidence []byte) ([]byte, error) {
	winnerNote, err := sourceMergeLoopOwnerNote(winnerEvidence)
	if err != nil {
		return nil, err
	}
	retiredNote, err := sourceMergeLoopOwnerNote(retiredEvidence)
	if err != nil {
		return nil, err
	}
	if retiredNote == "" || retiredNote == winnerNote {
		return winnerEvidence, nil
	}
	if winnerNote != "" {
		retiredNote = winnerNote + "\n" + retiredNote
	}
	return loopEvidenceMetadata(winnerEvidence, "owner_note", retiredNote)
}

func sourceMergeLoopOwnerNote(raw []byte) (string, error) {
	var evidence []map[string]jsontext.Value
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return "", fmt.Errorf("decode loop evidence: %w", err)
	}
	if len(evidence) == 0 || evidence[0] == nil {
		return "", nil
	}
	encoded, ok := evidence[0]["owner_note"]
	if !ok {
		return "", nil
	}
	var note string
	if err := json.Unmarshal(encoded, &note); err != nil {
		return "", fmt.Errorf("decode owner note: %w", err)
	}
	return note, nil
}

// Copy only occurrences not already represented on the survivor. Compare
// archived file identity and presentation, not source-local MIME/provider IDs.
// Counting matching occurrences preserves repeated files within one message.
const missingMergedAttachmentPredicate = `(
 SELECT COUNT(*) FROM attachments b WHERE b.message_id = ?
 AND b.content_hash = a.content_hash
 AND COALESCE(b.filename, '') = COALESCE(a.filename, '')
 AND COALESCE(b.mime_type, '') = COALESCE(a.mime_type, '')
 AND COALESCE(b.size, 0) = COALESCE(a.size, 0)
 AND COALESCE(b.attachment_role, '') = COALESCE(a.attachment_role, '')
 AND COALESCE(b.content_id, '') = COALESCE(a.content_id, '')
) < (
 SELECT COUNT(*) FROM attachments h WHERE h.message_id = a.message_id AND h.id <= a.id
 AND h.content_hash = a.content_hash
 AND COALESCE(h.filename, '') = COALESCE(a.filename, '')
 AND COALESCE(h.mime_type, '') = COALESCE(a.mime_type, '')
 AND COALESCE(h.size, 0) = COALESCE(a.size, 0)
 AND COALESCE(h.attachment_role, '') = COALESCE(a.attachment_role, '')
 AND COALESCE(h.content_id, '') = COALESCE(a.content_id, '')
) OR COALESCE(a.content_hash, '') = ''`

func copyMergedMessageEvidence(ctx context.Context, tx *loggedTx, from, into, originalSource int64) error {
	for _, statement := range []string{
		`INSERT INTO message_labels(message_id, label_id) SELECT ?, label_id FROM message_labels WHERE message_id = ? ON CONFLICT (message_id, label_id) DO NOTHING`,
		`INSERT INTO message_recipients(message_id, participant_id, recipient_type, display_name, email_address) SELECT ?, participant_id, recipient_type, display_name, email_address FROM message_recipients WHERE message_id = ? ON CONFLICT DO NOTHING`,
		`INSERT INTO reactions(message_id, participant_id, reaction_type, reaction_value, created_at, removed_at) SELECT ?, participant_id, reaction_type, reaction_value, created_at, removed_at FROM reactions WHERE message_id = ? ON CONFLICT (message_id, participant_id, reaction_type, reaction_value) DO NOTHING`,
	} {
		if _, err := tx.ExecContext(ctx, statement, into, from); err != nil {
			return err
		}
	}
	const attachmentColumns = `filename, mime_type, size, content_hash, storage_path, media_type, width, height, duration_ms,
 thumbnail_hash, thumbnail_path, source_attachment_id, attachment_metadata, attachment_state, attachment_skip_reason,
 attachment_role, role_source, source_part_key, content_id, encryption_version, created_at`

	rows, err := tx.QueryContext(ctx, `SELECT a.id FROM attachments a WHERE a.message_id = ? AND (`+missingMergedAttachmentPredicate+`) ORDER BY a.id`, from, into)
	if err != nil {
		return err
	}
	var attachmentIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		attachmentIDs = append(attachmentIDs, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range attachmentIDs {
		key := fmt.Sprintf("msgvault-archive:%d:attachment:%d", originalSource, id)
		selectColumns := strings.Replace(attachmentColumns, "source_part_key,", "CASE WHEN source_part_key IS NOT NULL THEN ? ELSE NULL END,", 1)
		var copied int64
		err := tx.QueryRowContext(ctx, `INSERT INTO attachments(message_id, `+attachmentColumns+`) SELECT ?, `+selectColumns+` FROM attachments WHERE id = ? ON CONFLICT DO NOTHING RETURNING id`, into, key, id).Scan(&copied)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO source_merge_attachments(attachment_id, original_attachment_id, original_source_id) VALUES (?, ?, ?)`, copied, id, originalSource); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO source_merge_preserved_attachments(attachment_id) VALUES (?) ON CONFLICT (attachment_id) DO NOTHING`, copied); err != nil {
			return err
		}
		// Only the copied occurrence loses provider eligibility. Original live
		// destination attachments retain their pending state and endpoint.
		if _, err := tx.ExecContext(ctx, `UPDATE attachments SET attachment_state = 'unavailable', attachment_skip_reason = 'historical-source' WHERE id = ? AND COALESCE(content_hash, '') = ''`, copied); err != nil {
			return err
		}
	}

	_, err = tx.ExecContext(ctx, `UPDATE messages SET embed_gen = NULL, attachment_count = (SELECT COUNT(*) FROM attachments WHERE message_id = messages.id),
 has_attachments = EXISTS (SELECT 1 FROM attachments WHERE message_id = messages.id) WHERE id = ?`, into)
	return err
}

func (s *Store) mergeSourceIdentityEvidence(ctx context.Context, tx *loggedTx, from, into int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT address, source_signal, confirmed_at FROM account_identities WHERE source_id = ? ORDER BY address`, from)
	if err != nil {
		return err
	}
	type identity struct {
		address, signals string
		confirmed        time.Time
	}
	// Scan a native timestamp to retain its precision on both backends.
	var identities []identity
	for rows.Next() {
		var i identity
		if err := rows.Scan(&i.address, &i.signals, &i.confirmed); err != nil {
			_ = rows.Close()
			return err
		}
		identities = append(identities, i)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, i := range identities {
		if _, err := s.mergeAccountIdentitySignalsTx(ctx, tx, into, i.address, strings.Split(i.signals, ","), newIdentifierMatch(i.address)); err != nil {
			return err
		}
		confirmed := s.dialect.TimestampParam(i.confirmed)
		if _, err := tx.ExecContext(ctx, `UPDATE account_identities SET confirmed_at = ? WHERE source_id = ? AND address_key = ? AND confirmed_at > ?`, confirmed, into, NormalizeIdentifierForCompare(i.address), confirmed); err != nil {
			return err
		}
	}
	return nil
}
