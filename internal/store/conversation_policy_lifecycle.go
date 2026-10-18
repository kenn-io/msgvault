package store

import (
	"context"
	"encoding/json/v2"
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
	people   []int64
	revision int64
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
	}
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
	return s.invalidateConversationPolicyPersonsTx(ctx, tx, state.people, refreshed.Revision)
}
