package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/conversationpolicy"
)

const (
	conversationPolicySettingsKey       = "conversation_policy_settings"
	conversationPolicyActivityExpiryKey = "conversation_policy_activity_expiry_checked_at"
	conversationPolicyRoomBatchSize     = 256
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
	settings, raw, err := normalizedConversationPolicySettings(settings)
	if err != nil {
		return result, err
	}
	err = s.withConversationPolicyWrite(ctx, func(tx *loggedTx) error {
		return s.configureConversationPolicyTx(ctx, tx, settings, raw, now, &result, false)
	})
	return result, err
}

// normalizedConversationPolicySettings returns the settings and the exact JSON
// that configuration stores. Override order is not policy state. Copy before
// sorting because callers retain ownership of the slice and its backing array.
func normalizedConversationPolicySettings(settings conversationpolicy.Settings) (conversationpolicy.Settings, string, error) {
	settings.Overrides = slices.Clone(settings.Overrides)
	slices.SortFunc(settings.Overrides, func(a, b conversationpolicy.Override) int {
		return cmp.Or(cmp.Compare(a.SourceType, b.SourceType), cmp.Compare(a.SourceIdentifier, b.SourceIdentifier), cmp.Compare(a.ConversationID, b.ConversationID))
	})
	raw, err := json.Marshal(settings, json.Deterministic(true))
	return settings, string(raw), err
}

// ConversationPolicyRefreshDue reports whether configuring and expiring with
// settings at now could change any policy state. Both steps take the exclusive
// policy lock, which stalls every writer that holds it shared, so the daemon's
// minute loop skips them when nothing is due. The check is conservative: a
// false positive only costs one refresh.
func (s *Store) ConversationPolicyRefreshDue(ctx context.Context, settings conversationpolicy.Settings, now time.Time) (bool, error) {
	if err := settings.Validate(); err != nil {
		return false, err
	}
	if now.IsZero() {
		return false, errors.New("policy time must be set")
	}
	_, raw, err := normalizedConversationPolicySettings(settings)
	if err != nil {
		return false, err
	}
	due := false
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		due, err = s.conversationPolicyRefreshDueTx(ctx, tx, settings, raw, now)
		return err
	})
	return due, err
}

func (s *Store) conversationPolicyRefreshDueTx(ctx context.Context, tx *loggedTx, settings conversationpolicy.Settings, raw string, now time.Time) (bool, error) {
	_, stored, err := s.conversationPolicySettingsTx(ctx, tx)
	if err != nil || stored != raw {
		return true, err
	}
	// Configuration recomputes self attribution when either identity revision
	// moved since the last pass, or when no fingerprint was recorded yet.
	var attribution, recorded string
	var fingerprinted bool
	err = tx.QueryRowContext(ctx, `SELECT
		COALESCE(MAX(CASE WHEN key='identity_revision' THEN value END),'0') || ':' || COALESCE(MAX(CASE WHEN key='account_identity_revision' THEN value END),'0'),
		COALESCE(MAX(CASE WHEN key='conversation_policy_attribution_revision' THEN value END),''),
		COUNT(CASE WHEN key='conversation_policy_self_attribution_fingerprint' THEN 1 END) > 0
		FROM archive_metadata`).Scan(&attribution, &recorded, &fingerprinted)
	if err != nil || attribution != recorded || !fingerprinted {
		return true, err
	}
	var pending bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM archive_metadata WHERE key LIKE 'conversation_policy_activity_dirty:%')
		OR EXISTS(SELECT 1 FROM conversations c JOIN sources src ON src.id=c.source_id
			WHERE src.source_type IN ('beeper','slack','slackdump','discord','teams')
			  AND NOT EXISTS (SELECT 1 FROM conversation_policy p WHERE p.conversation_id=c.id))
		OR NOT EXISTS(SELECT 1 FROM archive_metadata WHERE key='`+conversationPolicyActivityExpiryKey+`')`).Scan(&pending)
	if err != nil || pending {
		return true, err
	}
	// A configured override applies to any source, including ones without
	// automatic classification, whose rooms may be created without a policy
	// row. Refresh until each override's room carries its tier. A saved CLI
	// override takes precedence, so its room needs no config-driven refresh.
	if len(settings.Overrides) > 0 {
		var clauses []string
		var args []any
		for _, override := range settings.Overrides {
			clauses = append(clauses, `(src.source_type=? AND src.identifier=? AND c.source_conversation_id=? AND NOT EXISTS (
				SELECT 1 FROM conversation_policy p WHERE p.conversation_id=c.id
				  AND (p.effective_tier=? OR COALESCE(p.override_tier,'')<>'')))`)
			args = append(args, override.SourceType, override.SourceIdentifier, override.ConversationID, string(override.Tier))
		}
		err = tx.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(SELECT 1 FROM conversations c JOIN sources src ON src.id=c.source_id WHERE `+strings.Join(clauses, " OR ")+`)`), args...).Scan(&pending)
		if err != nil || pending {
			return true, err
		}
	}
	window := time.Duration(settings.ActivityWindowDays) * 24 * time.Hour
	since, err := s.conversationPolicyActivityExpiryStartTx(ctx, tx, now, window)
	if err != nil {
		return true, err
	}
	// A room stops counting as active once its last activity falls behind the
	// window cutoff. Widen the bounds by a second so timestamp precision can
	// only add a refresh, never skip one.
	lower, upper := since.Add(-window).Add(-time.Second).UTC(), now.Add(-window).Add(time.Second).UTC()
	crossed := `SELECT EXISTS(SELECT 1 FROM conversation_policy p WHERE p.last_activity_at >= ? AND p.last_activity_at <= ?)`
	if !s.IsPostgreSQL() {
		crossed = `SELECT EXISTS(SELECT 1 FROM conversation_policy p WHERE julianday(p.last_activity_at) >= julianday(?) AND julianday(p.last_activity_at) <= julianday(?))`
	}
	if err = tx.QueryRowContext(ctx, s.Rebind(crossed), lower, upper).Scan(&pending); err != nil || pending {
		return true, err
	}
	// Expiry also rescans rooms whose message timestamps entered the present.
	ids, err := s.conversationPolicyActivityIDsInIntervalTx(ctx, tx, since, now)
	return len(ids) > 0, err
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
	changed := old != raw || selfChanged
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
	if err = s.setConversationPolicyMetadataTx(ctx, tx, "conversation_policy_attribution_revision", attribution); err != nil {
		return err
	}
	if selfChanged {
		return s.setConversationPolicyMetadataTx(ctx, tx, conversationPolicyActivityExpiryKey, now.UTC().Format(time.RFC3339Nano))
	}
	return nil
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
			window := time.Duration(settings.ActivityWindowDays) * 24 * time.Hour
			since, checkpointErr := s.conversationPolicyActivityExpiryStartTx(ctx, tx, now, window)
			if checkpointErr != nil {
				return checkpointErr
			}
			intervalIDs, intervalErr := s.conversationPolicyActivityIDsInIntervalTx(ctx, tx, since, now)
			if intervalErr != nil {
				return intervalErr
			}
			dirtyActivityIDs = append(dirtyActivityIDs, intervalIDs...)
			slices.Sort(dirtyActivityIDs)
			dirtyActivityIDs = slices.Compact(dirtyActivityIDs)
			if len(dirtyActivityIDs) > 0 {
				if err = s.refreshConversationActivityTx(ctx, tx, dirtyActivityIDs, now); err != nil {
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
		if err != nil {
			return err
		}
		if ids == nil {
			return s.setConversationPolicyMetadataTx(ctx, tx, conversationPolicyActivityExpiryKey, now.UTC().Format(time.RFC3339Nano))
		}
		return nil
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
		if err := s.enqueueConversationActivityProjectionTx(ctx, tx, c.row.ConversationID); err != nil {
			return result, fmt.Errorf("queue activity for policy conversation %d: %w", c.row.ConversationID, err)
		}
		result.ChangedConversationIDs = append(result.ChangedConversationIDs, c.row.ConversationID)
	}
	if settingsChanged || len(result.ChangedConversationIDs) > 0 {
		if err = s.bumpConversationPolicyRevision(ctx, tx); err != nil {
			return result, err
		}
		if err = s.bumpDerivedDataRevisionContext(ctx, tx); err != nil {
			return result, err
		}
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE person_contact_state SET dirty_at=COALESCE(dirty_at,%s) WHERE dirty_at IS NULL`, s.dialect.Now())); err != nil {
			return result, err
		}
		// Rows that were already dirty are not rewritten, so their update
		// trigger does not queue a Directory refresh. Queue it directly: the
		// Directory's eligible-contact fallback must see every demotion.
		if s.withoutSyncScope().directoryProjectionReady {
			if _, err = tx.ExecContext(ctx, `INSERT INTO directory_projection_dirty(person_id)
				SELECT person_id FROM person_contact_state WHERE dirty_at IS NOT NULL
				ON CONFLICT(person_id) DO NOTHING`); err != nil {
				return result, err
			}
		}
	}
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(CAST(value AS BIGINT)),0) FROM archive_metadata WHERE key='conversation_policy_revision'`).Scan(&result.Revision)
	return result, err
}
