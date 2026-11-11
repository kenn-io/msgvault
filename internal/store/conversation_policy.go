package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/conversationpolicy"
)

const conversationPolicyRevisionKey = "conversation_policy_revision"

// ConversationPolicy is a source-qualified archived room and its live policy.
// EffectiveTier is the current authoritative eligibility tier.
type ConversationPolicy struct {
	ConversationID   int64                   `json:"conversation_id"`
	SourceID         int64                   `json:"source_id"`
	SourceType       string                  `json:"source_type"`
	SourceIdentifier string                  `json:"source_identifier"`
	ProviderID       string                  `json:"provider_id"`
	Participants     int                     `json:"participants"`
	KnownSize        bool                    `json:"known_size"`
	EffectiveTier    conversationpolicy.Tier `json:"effective_tier"`
	Override         conversationpolicy.Tier `json:"override,omitempty"`
	LastActivity     time.Time               `json:"last_activity,omitzero"`
}

// ListConversationPolicies uses the same roster decoder and participant-count
// resolution as media policy, including Discord's observed lower bound.
func (s *Store) ListConversationPolicies(ctx context.Context) ([]ConversationPolicy, error) {
	scope, args := sourceScopeClause(ctx, "c.source_id")
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT c.id,c.source_id,src.source_type,src.identifier,
 COALESCE(c.source_conversation_id,''),COALESCE(c.participant_count,0),
 (SELECT COUNT(DISTINCT cp.participant_id) FROM conversation_participants cp WHERE cp.conversation_id=c.id),
 COALESCE(CAST(c.metadata AS TEXT),''),COALESCE(p.effective_tier,CASE WHEN src.source_type IN ('beeper','slack','slackdump','discord','teams') THEN 'cold' ELSE 'full' END),COALESCE(p.override_tier,''),p.last_activity_at
 FROM conversations c JOIN sources src ON src.id=c.source_id
 LEFT JOIN conversation_policy p ON p.conversation_id=c.id WHERE 1=1`+scope+` ORDER BY c.id`), args...)
	if err != nil {
		return nil, fmt.Errorf("list conversation policies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []ConversationPolicy{}
	for rows.Next() {
		var row ConversationPolicy
		var observed int
		var metadata string
		var activity nullableTimestamp
		if err := rows.Scan(&row.ConversationID, &row.SourceID, &row.SourceType, &row.SourceIdentifier, &row.ProviderID, &row.Participants, &observed, &metadata, &row.EffectiveTier, &row.Override, &activity); err != nil {
			return nil, fmt.Errorf("scan conversation policy: %w", err)
		}
		record := decodeMembershipRecord(metadata)
		row.KnownSize = record.counted && !record.unknown && !record.repairPending && record.memberCount >= 0
		row.Participants = attachmentPolicyParticipantCount(row.SourceType, max(row.Participants, observed), metadata)
		// Corrupt negative metadata is unresolved, never a small-room admission.
		row.Participants = max(0, row.Participants)
		if activity.Valid {
			row.LastActivity = activity.Time
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// ConversationPolicyRevision changes atomically with saved policy settings.
func (s *Store) ConversationPolicyRevision(ctx context.Context) (int64, error) {
	var revision int64
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT CAST(value AS BIGINT) FROM archive_metadata WHERE key=?`), conversationPolicyRevisionKey).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return revision, err
}

// InvalidateConversationPolicyWorkTx advances the policy revision inside the
// caller's transaction. Cleanup calls it in the transaction that resets
// discovery progress: a worker that captured work before then would
// otherwise pass the revision check and write its older progress over the
// reset, skipping newly eligible messages until a backstop.
func (s *Store) InvalidateConversationPolicyWorkTx(ctx context.Context, tx ConversationPolicyTx) error {
	return s.bumpConversationPolicyRevision(ctx, tx)
}

func (s *Store) bumpConversationPolicyRevision(ctx context.Context, tx contextStatementQuerier) error {
	_, err := tx.ExecContext(ctx, s.Rebind(`INSERT INTO archive_metadata(key,value) VALUES (?,'1')
 ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(archive_metadata.value AS BIGINT)+1 AS TEXT)`), conversationPolicyRevisionKey)
	return err
}

// SetConversationPolicyOverride saves full/cold or resets with the empty tier.
// Stub/skip are rejected until explicit retention and purge support is present.
// Effective state, invalidations and cleanup intent commit with the override.
func (s *Store) SetConversationPolicyOverride(ctx context.Context, sourceID int64, providerID string, tier conversationpolicy.Tier) error {
	if tier != "" {
		if _, err := conversationpolicy.ParseTier(string(tier)); err != nil {
			return err
		}
		if tier == conversationpolicy.Stub || tier == conversationpolicy.Skip {
			return fmt.Errorf("tier %s requires retention/purge support, which is not enabled", tier)
		}
	}
	return s.withConversationPolicyWrite(ctx, func(tx *loggedTx) error {
		scope, scopeArgs := sourceScopeClause(ctx, "source_id")
		var id int64
		err := tx.QueryRowContext(ctx, s.Rebind(`SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=?`+scope), append([]any{sourceID, providerID}, scopeArgs...)...).Scan(&id)
		if err != nil {
			return fmt.Errorf("find policy conversation: %w", err)
		}
		var current sql.NullString
		err = tx.QueryRowContext(ctx, s.Rebind(`SELECT override_tier FROM conversation_policy WHERE conversation_id=?`), id).Scan(&current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if current.String == string(tier) {
			return nil
		}
		var value any
		if tier != "" {
			value = string(tier)
		}
		_, err = tx.ExecContext(ctx, s.Rebind(`INSERT INTO conversation_policy(conversation_id,override_tier) VALUES(?,?)
   ON CONFLICT(conversation_id) DO UPDATE SET override_tier=excluded.override_tier`), id, value)
		if err != nil {
			return fmt.Errorf("save conversation override: %w", err)
		}
		settings, _, err := s.conversationPolicySettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		_, err = s.refreshConversationPolicyTx(ctx, tx, settings, []int64{id}, time.Now(), true)
		return err
	})
}
