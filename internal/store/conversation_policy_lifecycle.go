package store

import (
	"context"
	"encoding/json/v2"
	"slices"
	"time"
)

// Source lifecycle work keeps the existing maintenance owner and its actual
// transaction. Execution leases are acquired by the caller before this fence.
func (s *Store) runConversationPolicyMaintenance(ctx context.Context, fn func(context.Context, *loggedTx) error) error {
	if !s.IsPostgreSQL() {
		release, err := s.AcquireSQLiteConversationPolicyFence(ctx, ConversationPolicyExclusive)
		if err != nil {
			return err
		}
		defer release()
	}
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if s.IsPostgreSQL() {
			if err := s.LockPostgresConversationPolicyTx(ctx, tx.Tx, ConversationPolicyExclusive); err != nil {
				return err
			}
		}
		return fn(ctx, tx)
	})
}

type conversationPolicyLifecycle struct {
	participants []int64
	revision     int64
	// A contact-only merge must not bootstrap unrelated chat policy. Its actual
	// self resolver may still change, so compare it again after the mutation.
	checkSelfOnly bool
	selfBefore    string
	activityRooms []int64
}

func (s *Store) captureSourceConversationPolicyTx(ctx context.Context, tx *loggedTx, sourceIDs ...int64) (conversationPolicyLifecycle, error) {
	var state conversationPolicyLifecycle
	marks, args := sortedIDPlaceholders(sourceIDs)
	rows, err := tx.QueryContext(ctx, `SELECT id FROM conversations WHERE source_id IN (`+marks+`) ORDER BY id`, args...)
	if err != nil {
		return state, err
	}
	var rooms []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			break
		}
		rooms = append(rooms, id)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return state, err
	}
	return s.captureConversationPolicyRoomsTx(ctx, tx, rooms)
}

func (s *Store) captureConversationPolicyRoomsTx(ctx context.Context, tx *loggedTx, rooms []int64) (conversationPolicyLifecycle, error) {
	var state conversationPolicyLifecycle
	var err error
	if len(rooms) > 0 {
		if err = queryInChunksContext(ctx, tx, rooms, nil,
			`WITH target_rooms AS (SELECT id FROM conversations WHERE id IN (%s))
			 SELECT cp.participant_id FROM conversation_participants cp
			 WHERE EXISTS (SELECT 1 FROM target_rooms WHERE id=cp.conversation_id)
			 UNION SELECT m.sender_id FROM messages m
			 WHERE m.sender_id IS NOT NULL AND EXISTS (SELECT 1 FROM target_rooms WHERE id=m.conversation_id)
			 UNION SELECT mr.participant_id FROM message_recipients mr
			 WHERE EXISTS (SELECT 1 FROM messages m WHERE m.id=mr.message_id
			               AND EXISTS (SELECT 1 FROM target_rooms WHERE id=m.conversation_id))
			 ORDER BY participant_id`,
			func(rows *loggedRows) error {
				var id int64
				if err := rows.Scan(&id); err != nil {
					return err
				}
				state.participants = append(state.participants, id)
				return nil
			}); err != nil {
			return state, err
		}
		state.activityRooms = slices.Clone(rooms)
	}
	state.participants = sortedInt64Set(mapInt64Set(state.participants))
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(CAST(value AS BIGINT)),0) FROM archive_metadata WHERE key='conversation_policy_revision'`).Scan(&state.revision)
	return state, err
}

// Policy and main-store invalidations commit with the provenance mutation.
// Physical native cleanup belongs to the existing post-commit coordinator.
func (s *Store) finishConversationPolicyLifecycleTx(ctx context.Context, tx *loggedTx, state conversationPolicyLifecycle) error {
	if err := markConversationPolicyActivitiesDirtyWith(boundQuerier{ctx: ctx, q: tx}, state.activityRooms); err != nil {
		return err
	}
	var err error
	refresh := !state.checkSelfOnly
	if state.checkSelfOnly {
		current, err := s.conversationPolicySelfFingerprintTx(ctx, tx)
		if err != nil {
			return err
		}
		refresh = state.selfBefore != current
	}
	refreshed := ConversationPolicyRefresh{Revision: state.revision}
	if refresh {
		settings, _, err := s.conversationPolicySettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(settings, json.Deterministic(true))
		if err != nil {
			return err
		}
		if err = s.configureConversationPolicyTx(ctx, tx, settings, string(raw), time.Now().UTC(), &refreshed, true); err != nil {
			return err
		}
	}
	// Full provenance can enter an already-Cold destination without any tier
	// delta. Its old work and cached/native publications still need revocation.
	if refreshed.Revision == state.revision {
		if err = s.bumpConversationPolicyRevision(ctx, tx); err != nil {
			return err
		}
		if err = s.bumpDerivedDataRevisionContext(ctx, tx); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, `SELECT CAST(value AS BIGINT) FROM archive_metadata WHERE key='conversation_policy_revision'`).Scan(&refreshed.Revision); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, advanceConversationPolicyCleanupSQL); err != nil {
		return err
	}
	return s.invalidateContextualParticipantMessagesTx(ctx, tx, state.participants)
}

func mapInt64Set(values []int64) map[int64]struct{} {
	set := make(map[int64]struct{}, len(values))
	for _, value := range values {
		if value > 0 {
			set[value] = struct{}{}
		}
	}
	return set
}

// Contextual blocks include historical senders even after they leave the roster.
// Their supported names can change when a different conversation becomes Cold.
func (s *Store) invalidateContextualParticipantMessagesTx(ctx context.Context, tx *loggedTx, participantIDs []int64) error {
	participantIDs = sortedInt64Set(mapInt64Set(participantIDs))
	return execInChunksContext(ctx, tx, participantIDs, nil, `
		WITH changed_participants AS (SELECT id FROM participants WHERE id IN (%s))
		UPDATE messages SET embed_gen=NULL, embedding_revision=embedding_revision+1
		WHERE message_type='beeper'
		  AND conversation_id IN (
			SELECT conversation_id FROM conversation_participants
			WHERE participant_id IN (SELECT id FROM changed_participants)
			UNION
			SELECT conversation_id FROM messages
			WHERE sender_id IN (SELECT id FROM changed_participants)
		  )
		  AND `+FullMessageWhere("messages"))
}
