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
	people       []int64
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
		state.people, err = s.conversationPolicyPeopleForRoomsTx(ctx, tx, rooms)
		if err != nil {
			return state, err
		}
		if err = queryInChunksContext(ctx, tx, rooms, nil,
			`WITH target_rooms AS (SELECT id FROM conversations WHERE id IN (%s))
			 SELECT cp.participant_id FROM conversation_participants cp
			 WHERE EXISTS (SELECT 1 FROM target_rooms WHERE id=cp.conversation_id)
			 UNION SELECT m.sender_id FROM messages m
			 WHERE m.sender_id IS NOT NULL AND EXISTS (SELECT 1 FROM target_rooms WHERE id=m.conversation_id)
			 UNION SELECT mr.participant_id FROM message_recipients mr
			 WHERE EXISTS (SELECT 1 FROM messages m WHERE m.id=mr.message_id
			               AND EXISTS (SELECT 1 FROM target_rooms WHERE id=m.conversation_id))
			 UNION SELECT o.participant_id FROM participant_contact_observations o
			 WHERE EXISTS (SELECT 1 FROM participant_contact_observation_conversations edge
			               WHERE edge.observation_id=o.id
			                 AND EXISTS (SELECT 1 FROM target_rooms WHERE id=edge.conversation_id))
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

// Select only the merged identities' supporting rooms. In particular, a merge
// into an unchanged self ID can alter activity without changing its fingerprint.
func (s *Store) captureParticipantConversationPolicyTx(ctx context.Context, tx *loggedTx, oldID, newID int64) (conversationPolicyLifecycle, error) {
	var state conversationPolicyLifecycle
	rows, err := tx.QueryContext(ctx, `SELECT c.id FROM conversations c JOIN sources src ON src.id=c.source_id
 WHERE src.source_type IN ('beeper','slack','slackdump','discord','teams') AND c.id IN (
 SELECT conversation_id FROM messages WHERE sender_id IN (?,?) AND conversation_id IS NOT NULL
 UNION SELECT m.conversation_id FROM message_recipients mr JOIN messages m ON m.id=mr.message_id WHERE mr.participant_id IN (?,?) AND m.conversation_id IS NOT NULL
 UNION SELECT conversation_id FROM conversation_participants WHERE participant_id IN (?,?)
 UNION SELECT edge.conversation_id FROM participant_contact_observations o JOIN participant_contact_observation_conversations edge ON edge.observation_id=o.id WHERE o.participant_id IN (?,?))`, oldID, newID, oldID, newID, oldID, newID, oldID, newID)
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
	state, err = s.captureConversationPolicyRoomsTx(ctx, tx, rooms)
	if err != nil {
		return state, err
	}
	if len(rooms) == 0 {
		state.checkSelfOnly = true
		state.selfBefore, err = s.conversationPolicySelfFingerprintTx(ctx, tx)
		if err != nil {
			return state, err
		}
	}
	state.activityRooms = rooms
	state.participants = sortedInt64Set(mapInt64Set(append(state.participants, oldID, newID)))
	return state, nil
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) SELECT 'conversation_policy_cleanup_revision',value FROM archive_metadata WHERE key='conversation_policy_revision' ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		return err
	}
	if err = s.invalidateContextualParticipantMessagesTx(ctx, tx, state.participants); err != nil {
		return err
	}
	return s.invalidateConversationPolicyPersonsTx(ctx, tx, state.people, refreshed.Revision)
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
