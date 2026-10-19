package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/conversationpolicy"
)

const (
	conversationPolicySettingsKey   = "conversation_policy_settings"
	conversationPolicyCleanupKey    = "conversation_policy_cleanup_revision"
	conversationPolicyCleanedKey    = "conversation_policy_cleaned_revision"
	conversationPolicyRoomBatchSize = 256
)

type ConversationPolicyRefresh struct {
	Revision               int64
	ChangedConversationIDs []int64
}

// ConversationPolicySettingsContext reads the persisted configuration without
// changing policy state. Each call returns independently decoded maps and slices.
func (s *Store) ConversationPolicySettingsContext(ctx context.Context) (conversationpolicy.Settings, error) {
	settings, _, err := s.conversationPolicySettingsWith(ctx, s.db)
	return settings, err
}

func (s *Store) conversationPolicySettingsTx(ctx context.Context, tx *loggedTx) (conversationpolicy.Settings, string, error) {
	return s.conversationPolicySettingsWith(ctx, tx)
}

func (s *Store) conversationPolicySettingsWith(ctx context.Context, q contextStatementQuerier) (conversationpolicy.Settings, string, error) {
	var raw string
	err := q.QueryRowContext(ctx, s.Rebind(`SELECT value FROM archive_metadata WHERE key=?`), conversationPolicySettingsKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return conversationpolicy.DefaultSettings(), "", nil
	}
	if err != nil {
		return conversationpolicy.Settings{}, "", err
	}
	var settings conversationpolicy.Settings
	if err = json.Unmarshal([]byte(raw), &settings); err != nil {
		return settings, raw, err
	}
	return settings, raw, settings.Validate()
}

func (s *Store) setConversationPolicyMetadataTx(ctx context.Context, tx *loggedTx, key, value string) error {
	_, err := tx.ExecContext(ctx, s.Rebind(`INSERT INTO archive_metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value WHERE archive_metadata.value<>excluded.value`), key, value)
	return err
}

func (s *Store) ConfigureConversationPolicy(ctx context.Context, settings conversationpolicy.Settings, now time.Time) (ConversationPolicyRefresh, error) {
	var result ConversationPolicyRefresh
	if err := settings.Validate(); err != nil {
		return result, err
	}
	if now.IsZero() {
		return result, errors.New("policy time must be set")
	}
	// Override order is not policy state. Copy before sorting because callers
	// retain ownership of the slice and its backing array.
	settings.Overrides = slices.Clone(settings.Overrides)
	slices.SortFunc(settings.Overrides, func(a, b conversationpolicy.Override) int {
		return cmp.Or(cmp.Compare(a.SourceType, b.SourceType), cmp.Compare(a.SourceIdentifier, b.SourceIdentifier), cmp.Compare(a.ConversationID, b.ConversationID))
	})
	raw, err := json.Marshal(settings, json.Deterministic(true))
	if err != nil {
		return result, err
	}
	err = s.withConversationPolicyWrite(ctx, func(tx *loggedTx) error {
		return s.configureConversationPolicyTx(ctx, tx, settings, string(raw), now, &result, false)
	})
	return result, err
}

func (s *Store) configureConversationPolicyTx(ctx context.Context, tx *loggedTx, settings conversationpolicy.Settings, raw string, now time.Time, result *ConversationPolicyRefresh, forceSelfCheck bool) error {
	_, old, err := s.conversationPolicySettingsTx(ctx, tx)
	if err != nil {
		return err
	}
	// Attribution revisions share the existing identity/account revision sources.
	var attribution string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(CASE WHEN key='identity_revision' THEN value END),'0') || ':' || COALESCE(MAX(CASE WHEN key='account_identity_revision' THEN value END),'0') FROM archive_metadata`).Scan(&attribution)
	if err != nil {
		return err
	}
	var previous string
	err = tx.QueryRowContext(ctx, s.Rebind(`SELECT value FROM archive_metadata WHERE key=?`), "conversation_policy_attribution_revision").Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var previousFingerprint string
	err = tx.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key='conversation_policy_self_attribution_fingerprint'`).Scan(&previousFingerprint)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	fingerprint := previousFingerprint
	if forceSelfCheck || previous != attribution || previousFingerprint == "" {
		fingerprint, err = s.conversationPolicySelfFingerprintTx(ctx, tx)
		if err != nil {
			return err
		}
	}
	selfChanged := previousFingerprint != fingerprint
	var peopleBootstrapped bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM archive_metadata WHERE key='conversation_policy_people_bootstrapped')`).Scan(&peopleBootstrapped); err != nil {
		return err
	}
	changed := old != raw || selfChanged || !peopleBootstrapped
	dirty, err := s.drainConversationPolicyActivityDirtyTx(ctx, tx)
	if err != nil {
		return err
	}
	if selfChanged {
		if err = s.refreshConversationActivityTx(ctx, tx, nil, now); err != nil {
			return err
		}
	} else if len(dirty) > 0 {
		if err = s.refreshConversationActivityTx(ctx, tx, dirty, now); err != nil {
			return err
		}
	}
	if err = s.setConversationPolicyMetadataTx(ctx, tx, "conversation_policy_attribution_revision", attribution); err != nil {
		return err
	}
	if selfChanged {
		if err = s.setConversationPolicyMetadataTx(ctx, tx, "conversation_policy_self_attribution_fingerprint", fingerprint); err != nil {
			return err
		}
	}
	if !changed {
		if len(dirty) > 0 {
			*result, err = s.refreshConversationPolicyTx(ctx, tx, settings, dirty, now, false)
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(CAST(value AS BIGINT)),0) FROM archive_metadata WHERE key='conversation_policy_revision'`).Scan(&result.Revision)
	}
	if err = s.setConversationPolicyMetadataTx(ctx, tx, conversationPolicySettingsKey, raw); err != nil {
		return err
	}
	if selfChanged {
		if err = s.setConversationPolicyMetadataTx(ctx, tx, "conversation_policy_activity_dirty_revision", attribution); err != nil {
			return err
		}
	}
	if err = s.setConversationPolicyMetadataTx(ctx, tx, "conversation_policy_attribution_revision", attribution); err != nil {
		return err
	}
	*result, err = s.refreshConversationPolicyTx(ctx, tx, settings, nil, now, true)
	if err != nil {
		return err
	}
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(CASE WHEN key='identity_revision' THEN value END),'0') || ':' || COALESCE(MAX(CASE WHEN key='account_identity_revision' THEN value END),'0') FROM archive_metadata`).Scan(&attribution)
	if err != nil {
		return err
	}
	return s.setConversationPolicyMetadataTx(ctx, tx, "conversation_policy_attribution_revision", attribution)
}

func (s *Store) RefreshConversationPolicy(ctx context.Context, ids []int64, now time.Time) (ConversationPolicyRefresh, error) {
	if len(ids) == 0 {
		revision, err := s.ConversationPolicyRevision(ctx)
		return ConversationPolicyRefresh{Revision: revision}, err
	}
	return s.refreshConversationPolicy(ctx, ids, now)
}
func (s *Store) ExpireConversationPolicy(ctx context.Context, now time.Time) (ConversationPolicyRefresh, error) {
	return s.refreshConversationPolicy(ctx, nil, now)
}
func (s *Store) refreshConversationPolicy(ctx context.Context, ids []int64, now time.Time) (ConversationPolicyRefresh, error) {
	var result ConversationPolicyRefresh
	err := s.withConversationPolicyWrite(ctx, func(tx *loggedTx) error {
		settings, _, err := s.conversationPolicySettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		var dirtyActivityIDs []int64
		if ids == nil {
			dirty, drainErr := s.drainConversationPolicyActivityDirtyTx(ctx, tx)
			if drainErr != nil {
				return drainErr
			}
			dirtyActivityIDs = dirty
			if len(dirty) > 0 {
				if err = s.refreshConversationActivityTx(ctx, tx, dirty, now); err != nil {
					return err
				}
			}
		} else {
			if err = s.clearConversationPolicyActivityDirtyTx(ctx, tx, ids); err != nil {
				return err
			}
			if err = s.refreshConversationActivityTx(ctx, tx, ids, now); err != nil {
				return err
			}
		}
		result, err = s.refreshConversationPolicyTx(ctx, tx, settings, ids, now, false, dirtyActivityIDs...)
		return err
	})
	return result, err
}

// refreshConversationPolicyTx always queries the unscoped archive; ids narrows
// explicit refresh work only. Nil is private whole-archive maintenance.
func (s *Store) refreshConversationPolicyTx(ctx context.Context, tx *loggedTx, settings conversationpolicy.Settings, ids []int64, now time.Time, settingsChanged bool, dirtyActivityIDs ...int64) (ConversationPolicyRefresh, error) {
	result := ConversationPolicyRefresh{}
	if now.IsZero() {
		return result, errors.New("policy time must be set")
	}
	type candidate struct {
		row ConversationPolicy
		old string
	}
	var candidates []candidate
	var err error
	selected := map[int64]bool{}
	for _, id := range ids {
		selected[id] = true
	}
	// Expiry may initialize an interrupted Persist's dirty room, while quiet
	// missing rows still initialize cold without a historical scan.
	for _, id := range dirtyActivityIDs {
		selected[id] = true
	}
	queryCandidates := func(roomIDs []int64) error {
		var filter string
		var args []any
		if ids != nil {
			marks, bindIDs := sortedIDPlaceholders(roomIDs)
			if len(bindIDs) == 0 {
				return nil
			}
			filter = " WHERE c.id IN (" + marks + ")"
			args = bindIDs
		} else {
			// The periodic expiry pass needs only automatic chat-policy rooms
			// and explicitly managed exceptions. Email threads do not carry
			// policy state and must not pay the correlated membership count.
			// Keep prior cold exceptions discoverable after config removal.
			var filterBuilder strings.Builder
			filterBuilder.WriteString(` WHERE (src.source_type IN ('beeper','slack','slackdump','discord','teams') OR COALESCE(p.override_tier,'')<>'' OR p.effective_tier='cold'`)
			for _, override := range settings.Overrides {
				filterBuilder.WriteString(` OR (src.source_type=? AND src.identifier=? AND c.source_conversation_id=?)`)
				args = append(args, override.SourceType, override.SourceIdentifier, override.ConversationID)
			}
			filterBuilder.WriteByte(')')
			filter = filterBuilder.String()
		}
		rows, err := tx.QueryContext(ctx, s.Rebind(`SELECT c.id,c.source_id,src.source_type,src.identifier,COALESCE(c.source_conversation_id,''),COALESCE(c.participant_count,0),(SELECT COUNT(DISTINCT cp.participant_id) FROM conversation_participants cp WHERE cp.conversation_id=c.id),COALESCE(CAST(c.metadata AS TEXT),''),COALESCE(p.effective_tier,''),COALESCE(p.override_tier,''),p.last_activity_at FROM conversations c JOIN sources src ON src.id=c.source_id LEFT JOIN conversation_policy p ON p.conversation_id=c.id`+filter+` ORDER BY c.id`), args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c candidate
			var observed int
			var metadata string
			var activity nullableTimestamp
			if err := rows.Scan(&c.row.ConversationID, &c.row.SourceID, &c.row.SourceType, &c.row.SourceIdentifier, &c.row.ProviderID, &c.row.Participants, &observed, &metadata, &c.old, &c.row.Override, &activity); err != nil {
				_ = rows.Close()
				return err
			}
			if ids != nil && !selected[c.row.ConversationID] {
				continue
			}
			record := decodeMembershipRecord(metadata)
			c.row.KnownSize = record.counted && !record.unknown && !record.repairPending && record.memberCount >= 0
			c.row.Participants = max(0, attachmentPolicyParticipantCount(c.row.SourceType, max(c.row.Participants, observed), metadata))
			if activity.Valid {
				c.row.LastActivity = activity.Time
			}
			candidates = append(candidates, c)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		return rows.Close()
	}
	if ids == nil {
		if err := queryCandidates(nil); err != nil {
			return result, err
		}
	} else {
		roomIDs := slices.Clone(ids)
		slices.Sort(roomIDs)
		roomIDs = slices.Compact(roomIDs)
		for start := 0; start < len(roomIDs); start += conversationPolicyRoomBatchSize {
			end := min(start+conversationPolicyRoomBatchSize, len(roomIDs))
			if err := queryCandidates(roomIDs[start:end]); err != nil {
				return result, err
			}
		}
	}
	for _, c := range candidates {
		if c.old == "" && conversationpolicy.Supported(c.row.SourceType) && (settingsChanged || selected[c.row.ConversationID]) {
			c.row.LastActivity, err = s.conversationLastActivityWith(ctx, tx, c.row.ConversationID, now)
			if err != nil {
				return result, err
			}
		}
		tier := conversationpolicy.Full
		override := c.row.Override
		if override == "" {
			for _, o := range settings.Overrides {
				if o.SourceType == c.row.SourceType && o.SourceIdentifier == c.row.SourceIdentifier && o.ConversationID == c.row.ProviderID {
					override = o.Tier
					break
				}
			}
		}
		if conversationpolicy.Supported(c.row.SourceType) {
			tier, err = conversationpolicy.Evaluate(conversationpolicy.Input{Participants: c.row.Participants, KnownSize: c.row.KnownSize, LastActivity: c.row.LastActivity, MaxParticipants: settings.MaxParticipants[c.row.SourceType], ActivityWindow: time.Duration(settings.ActivityWindowDays) * 24 * time.Hour, Now: now, Override: override})
			if err != nil {
				return result, err
			}
		} else if override != "" {
			tier = override
		}
		if c.old == string(tier) || (c.old == "" && !conversationpolicy.Supported(c.row.SourceType) && override == "") {
			continue
		}
		var activity any
		if !c.row.LastActivity.IsZero() {
			activity = c.row.LastActivity
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO conversation_policy(conversation_id,effective_tier,last_activity_at) VALUES(?,?,?) ON CONFLICT(conversation_id) DO UPDATE SET effective_tier=excluded.effective_tier`, c.row.ConversationID, tier, activity)
		if err != nil {
			return result, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET embed_gen=NULL, embedding_revision=embedding_revision+1 WHERE conversation_id=?`, c.row.ConversationID); err != nil {
			return result, err
		}
		if err := s.enqueueConversationActivityProjectionTx(ctx, tx, c.row.ConversationID); err != nil {
			return result, fmt.Errorf("queue activity for policy conversation %d: %w", c.row.ConversationID, err)
		}
		result.ChangedConversationIDs = append(result.ChangedConversationIDs, c.row.ConversationID)
		if tier == conversationpolicy.Full {
			if err := s.reconcilePromotedConversationConflictsTx(ctx, tx, c.row.ConversationID); err != nil {
				return result, err
			}
		}
	}
	if settingsChanged || len(result.ChangedConversationIDs) > 0 {
		if err = s.reconcileCurrentObservationIdentityMatchesTxContext(ctx, tx); err != nil {
			return result, err
		}
		if err = s.bumpConversationPolicyRevision(ctx, tx); err != nil {
			return result, err
		}
		if err = s.bumpDerivedDataRevisionContext(ctx, tx); err != nil {
			return result, err
		}
		var oldIdentity int64
		oldIdentity, err = s.currentIdentityRevisionTxContext(ctx, tx)
		if err != nil {
			return result, err
		}
		newIdentity, err := s.bumpIdentityRevisionContext(ctx, tx)
		if err != nil {
			return result, err
		}
		// Advance our own observation marker without hiding external identity drift.
		var account string
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(value),'0') FROM archive_metadata WHERE key='account_identity_revision'`).Scan(&account)
		if err != nil {
			return result, err
		}
		_, err = tx.ExecContext(ctx, s.Rebind(`UPDATE archive_metadata SET value=? WHERE key='conversation_policy_attribution_revision' AND value=?`), strconv.FormatInt(newIdentity, 10)+":"+account, strconv.FormatInt(oldIdentity, 10)+":"+account)
		if err != nil {
			return result, err
		}
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE person_contact_state SET dirty_at=COALESCE(dirty_at,%s) WHERE dirty_at IS NULL`, s.dialect.Now())); err != nil {
			return result, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) SELECT 'conversation_policy_cleanup_revision',value FROM archive_metadata WHERE key='conversation_policy_revision' ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
			return result, err
		}
	}
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(CAST(value AS BIGINT)),0) FROM archive_metadata WHERE key='conversation_policy_revision'`).Scan(&result.Revision)
	if err != nil {
		return result, err
	}
	if len(result.ChangedConversationIDs) > 0 {
		if err = s.invalidateConversationPolicyPeopleTx(ctx, tx, result.ChangedConversationIDs, result.Revision); err != nil {
			return result, err
		}
		lifecycle, captureErr := s.captureConversationPolicyRoomsTx(ctx, tx, result.ChangedConversationIDs)
		if captureErr != nil {
			return result, captureErr
		}
		if err = s.invalidateContextualParticipantMessagesTx(ctx, tx, lifecycle.participants); err != nil {
			return result, err
		}
	}
	if settingsChanged && ids == nil {
		var initialized bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM archive_metadata WHERE key='conversation_policy_people_bootstrapped')`).Scan(&initialized); err != nil {
			return result, err
		}
		if !initialized {
			if err = s.invalidateConversationPolicyPeopleTx(ctx, tx, nil, result.Revision); err != nil {
				return result, err
			}
			err = s.setConversationPolicyMetadataTx(ctx, tx, "conversation_policy_people_bootstrapped", "1")
		}
	}
	return result, err
}

// ConversationPolicyCleanupRevision returns the durable reconciliation target.
func (s *Store) ConversationPolicyCleanupRevision(ctx context.Context) (int64, error) {
	return readArchiveMetadataRevisionContext(ctx, s.db, conversationPolicyCleanupKey, "conversation-policy cleanup")
}

// AcknowledgeConversationPolicyCleanup acknowledges only the exact completed pass.
func (s *Store) AcknowledgeConversationPolicyCleanup(ctx context.Context, expected int64) (bool, error) {
	acknowledged := false
	err := s.withConversationPolicyWrite(ctx, func(tx *loggedTx) error {
		var current int64
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(CAST(value AS BIGINT)),0) FROM archive_metadata WHERE key='conversation_policy_cleanup_revision'`).Scan(&current)
		if err != nil {
			return err
		}
		if current != expected {
			return nil
		}
		if err = s.setConversationPolicyMetadataTx(ctx, tx, conversationPolicyCleanedKey, strconv.FormatInt(expected, 10)); err != nil {
			return err
		}
		acknowledged = true
		return nil
	})
	return acknowledged, err
}
