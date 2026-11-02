package store_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConversationPolicyBootstrapWholeArchive(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	a, err := s.GetOrCreateSource("beeper", "account-a")
	requirements.NoError(err)
	b, err := s.GetOrCreateSource("beeper", "account-b")
	requirements.NoError(err)
	scoped, err := s.EnsureConversation(a.ID, "room", "Scoped staged full")
	requirements.NoError(err)
	quiet, err := s.EnsureConversation(b.ID, "room", "Quiet staged full")
	requirements.NoError(err)
	missing, err := s.EnsureConversation(b.ID, "missing", "Quiet missing state")
	requirements.NoError(err)
	historyOnly := true
	sourceSettings, err := s.UpdateSourceSettingsContext(t.Context(), b.ID, store.SourceSettingsUpdate{HistoryOnly: &historyOnly})
	requirements.NoError(err)
	requirements.True(sourceSettings.HistoryOnly)
	_, err = s.DB().Exec(s.Rebind(`UPDATE sources SET last_sync_at=? WHERE id=?`), time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), b.ID)
	requirements.NoError(err)
	// Reproduce PR2's staged full state plus a legacy room with no policy row.
	_, err = s.DB().Exec(s.Rebind(`UPDATE conversation_policy SET effective_tier='full',last_activity_at=NULL WHERE conversation_id IN (?,?)`), scoped, quiet)
	requirements.NoError(err)
	_, err = s.DB().Exec(s.Rebind(`DELETE FROM conversation_policy WHERE conversation_id=?`), missing)
	requirements.NoError(err)
	var stagedCount, missingCount int
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT COUNT(*) FROM conversation_policy WHERE conversation_id IN (?,?) AND effective_tier='full' AND last_activity_at IS NULL`), scoped, quiet).Scan(&stagedCount))
	requirements.Equal(2, stagedCount)
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT COUNT(*) FROM conversation_policy WHERE conversation_id=?`), missing).Scan(&missingCount))
	requirements.Zero(missingCount)
	cfg := config.NewDefaultConfig()
	cfg.Beeper.Enabled = false
	cfg.Beeper.MaxParticipants = conversationpolicy.RecommendedMaxParticipants
	settings := cfg.ConversationPolicySettings()
	result, err := s.ConfigureConversationPolicy(sourcescope.WithSourceIDs(t.Context(), []int64{a.ID}), settings, time.Now())
	requirements.NoError(err)
	assertions.ElementsMatch([]int64{scoped, quiet, missing}, result.ChangedConversationIDs)
	assertions.Positive(result.Revision)
	rows, err := s.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	requirements.Len(rows, 3)
	for _, row := range rows {
		assertions.Equal(conversationpolicy.Cold, row.EffectiveTier)
	}
	again, err := s.ConfigureConversationPolicy(t.Context(), settings, time.Now())
	requirements.NoError(err)
	assertions.Equal(result.Revision, again.Revision)
	assertions.Empty(again.ChangedConversationIDs)
}

func TestConversationPolicySettingsOverrideOrderIsNoop(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("beeper", "ordering")
	requirements.NoError(err)
	_, err = s.EnsureConversation(source.ID, "a", "Room A")
	requirements.NoError(err)
	_, err = s.EnsureConversation(source.ID, "b", "Room B")
	requirements.NoError(err)
	a := conversationpolicy.Override{SourceType: "beeper", SourceIdentifier: "ordering", ConversationID: "a", Tier: conversationpolicy.Full}
	b := conversationpolicy.Override{SourceType: "beeper", SourceIdentifier: "ordering", ConversationID: "b", Tier: conversationpolicy.Cold}
	settings := conversationpolicy.RecommendedSettings()
	settings.Overrides = []conversationpolicy.Override{a, b}
	before, err := s.ConfigureConversationPolicy(t.Context(), settings, time.Now())
	requirements.NoError(err)
	settings.Overrides = []conversationpolicy.Override{b, a}
	original := append([]conversationpolicy.Override(nil), settings.Overrides...)
	after, err := s.ConfigureConversationPolicy(t.Context(), settings, time.Now())
	requirements.NoError(err)
	assertions.Equal(before.Revision, after.Revision)
	assertions.Empty(after.ChangedConversationIDs)
	assertions.Equal(original, settings.Overrides, "normalization must not mutate caller-owned storage")
}

func TestConversationPolicyNewRoomNeverFallsBackToFull(t *testing.T) {
	requirements := require.New(t)

	s := testutil.NewTestStore(t)
	_, err := s.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	requirements.NoError(err)
	src, err := s.GetOrCreateSource("slack", "team:user")
	requirements.NoError(err)
	_, err = s.EnsureConversation(src.ID, "unknown", "Unknown")
	requirements.NoError(err)
	rows, err := s.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assert.Equal(t, conversationpolicy.Cold, rows[0].EffectiveTier)
}

func TestConversationPolicyAutomaticClassificationIsOptIn(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	src, err := s.GetOrCreateSource("beeper", "example-account")
	requirements.NoError(err)
	room, err := s.EnsureConversation(src.ID, "large-room", "Large room")
	requirements.NoError(err)
	tier := func() conversationpolicy.Tier {
		rows, err := s.ListConversationPolicies(t.Context())
		requirements.NoError(err)
		requirements.Len(rows, 1)
		requirements.Equal(room, rows[0].ConversationID)
		return rows[0].EffectiveTier
	}
	assertions.Equal(conversationpolicy.Full, tier(), "an archive without saved settings keeps rooms full")

	now := time.Now()
	_, err = s.ConfigureConversationPolicy(t.Context(), config.NewDefaultConfig().ConversationPolicySettings(), now)
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Full, tier(), "the default configuration keeps an unknown-size room full")

	_, err = s.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), now)
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Cold, tier(), "setting a participant limit turns automatic classification on")
}

func TestConversationPolicyOverrideAppliesImmediately(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	_, policyErr := s.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	require.NoError(t, policyErr)
	src, err := s.GetOrCreateSource("beeper", "account")
	requirements.NoError(err)
	_, err = s.EnsureConversation(src.ID, "room", "Room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
	rows, err := s.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Full, rows[0].EffectiveTier)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "room", ""))
	rows, err = s.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Cold, rows[0].EffectiveTier)
}

func TestConversationPolicyQuietExpiry(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	_, policyErr := s.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	require.NoError(t, policyErr)
	src, err := s.GetOrCreateSource("beeper", "quiet")
	requirements.NoError(err)
	id, err := s.EnsureConversation(src.ID, "room", "Room")
	requirements.NoError(err)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	_, err = s.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: id, SourceMessageID: "own", MessageType: "beeper", IsFromMe: true, SentAt: sql.NullTime{Time: now, Valid: true}})
	requirements.NoError(err)
	result, err := s.RefreshConversationPolicy(t.Context(), []int64{id}, now)
	requirements.NoError(err)
	assertions.Contains(result.ChangedConversationIDs, id)
	rows, err := s.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Full, rows[0].EffectiveTier)
	_, err = s.ExpireConversationPolicy(t.Context(), now.Add(180*24*time.Hour))
	requirements.NoError(err)
	rows, err = s.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Full, rows[0].EffectiveTier)
	_, err = s.ExpireConversationPolicy(t.Context(), now.Add(180*24*time.Hour+time.Second))
	requirements.NoError(err)
	rows, err = s.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Cold, rows[0].EffectiveTier)
}

func TestConversationPolicyTransitionIsAtomic(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	_, policyErr := s.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	require.NoError(t, policyErr)
	src, err := s.GetOrCreateSource("beeper", "atomic")
	requirements.NoError(err)
	_, err = s.EnsureConversation(src.ID, "room", "Room")
	requirements.NoError(err)
	before, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	if s.IsPostgreSQL() {
		_, err = s.DB().Exec(fmt.Sprintf(`ALTER TABLE archive_metadata ADD CONSTRAINT reject_policy_revision CHECK (key<>'conversation_policy_revision' OR value<>'%d')`, before+1))
	} else {
		_, err = s.DB().Exec(`CREATE TRIGGER reject_policy_revision BEFORE UPDATE ON archive_metadata WHEN NEW.key='conversation_policy_revision' BEGIN SELECT RAISE(ABORT,'injected failure'); END`)
	}
	requirements.NoError(err)
	requirements.Error(s.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
	after, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	assertions.Equal(before, after)
	rows, err := s.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Cold, rows[0].EffectiveTier)
	assertions.Empty(rows[0].Override)
}

func TestConversationPolicyBootstrapLimitsAndPrecedence(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	src, err := s.GetOrCreateSource("beeper", "limits")
	requirements.NoError(err)
	small, err := s.EnsureConversation(src.ID, "boundary", "Boundary")
	requirements.NoError(err)
	_, err = s.DB().Exec(s.Rebind(`UPDATE conversations SET metadata=? WHERE id=?`), `{"member_count":20}`, small)
	requirements.NoError(err)
	large, err := s.EnsureConversation(src.ID, "large", "Large")
	requirements.NoError(err)
	_, err = s.DB().Exec(s.Rebind(`UPDATE conversations SET metadata=? WHERE id=?`), `{"member_count":21}`, large)
	requirements.NoError(err)
	unsupported, err := s.GetOrCreateSource("imap", "archive@example.com")
	requirements.NoError(err)
	_, err = s.EnsureConversation(unsupported.ID, "thread", "Thread")
	requirements.NoError(err)
	settings := conversationpolicy.RecommendedSettings()
	settings.Overrides = []conversationpolicy.Override{{SourceType: "beeper", SourceIdentifier: "limits", ConversationID: "large", Tier: conversationpolicy.Full}}
	_, err = s.ConfigureConversationPolicy(t.Context(), settings, time.Now())
	requirements.NoError(err)
	rows, err := s.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	for _, row := range rows {
		assertions.Equal(conversationpolicy.Full, row.EffectiveTier)
	}
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "large", conversationpolicy.Cold))
	rows, err = s.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Cold, rows[1].EffectiveTier)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "large", ""))
	rows, err = s.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Full, rows[1].EffectiveTier)
	settings.Overrides = nil
	settings.MaxParticipants["beeper"] = 0
	_, err = s.ConfigureConversationPolicy(t.Context(), settings, time.Now())
	requirements.NoError(err)
	_, err = s.EnsureConversation(src.ID, "off", "Off")
	requirements.NoError(err)
	rows, err = s.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	for _, row := range rows {
		assertions.Equal(conversationpolicy.Full, row.EffectiveTier)
	}
}

func TestConversationPolicyTransitionWorkAndCleanupRevision(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	_, policyErr := s.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	require.NoError(t, policyErr)
	src, err := s.GetOrCreateSource("beeper", "work")
	requirements.NoError(err)
	id, err := s.EnsureConversation(src.ID, "room", "Room")
	requirements.NoError(err)
	message, err := s.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: id, SourceMessageID: "message", MessageType: "beeper"})
	requirements.NoError(err)
	s.DB().SetMaxOpenConns(1)
	revision, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	requirements.ErrorIs(s.CheckConversationPolicyWork(t.Context(), revision, []int64{message}), store.ErrConversationPolicyStale)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
	latest, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	requirements.ErrorIs(s.CheckConversationPolicyWork(t.Context(), revision, []int64{message}), store.ErrConversationPolicyStale)
	requirements.NoError(s.CheckConversationPolicyWork(t.Context(), latest, []int64{message}))
	target, pending, err := s.ConversationPolicyCleanupPending(t.Context())
	requirements.NoError(err)
	assertions.True(pending)
	ack, err := s.AcknowledgeConversationPolicyCleanup(t.Context(), target-1)
	requirements.NoError(err)
	assertions.False(ack, "an earlier cleanup pass cannot acknowledge the current target")
	ack, err = s.AcknowledgeConversationPolicyCleanup(t.Context(), target)
	requirements.NoError(err)
	assertions.True(ack)
}

func TestConversationPolicyTransitionPreservesArchiveContent(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	src, err := s.GetOrCreateSource("beeper", "retention")
	requirements.NoError(err)
	room, err := s.EnsureConversation(src.ID, "room", "Room")
	requirements.NoError(err)
	message, err := s.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: room, SourceMessageID: "message", MessageType: "beeper", Snippet: sql.NullString{String: "synthetic retained snippet", Valid: true}})
	requirements.NoError(err)
	requirements.NoError(s.UpsertMessageBody(message, sql.NullString{String: "synthetic retained body", Valid: true}, sql.NullString{}))
	requirements.NoError(s.UpsertAttachment(message, "sample.txt", "text/plain", "", "synthetic-content-hash", 1))
	syncID, err := s.StartSyncContext(t.Context(), src.ID, "full")
	requirements.NoError(err)
	requirements.NoError(s.CompleteSyncContext(t.Context(), syncID, "cursor-value"))
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Full))
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "room", conversationpolicy.Cold))
	var body, snippet, cursor string
	var attachments int
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), message).Scan(&body))
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT snippet FROM messages WHERE id=?`), message).Scan(&snippet))
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id=?`), message).Scan(&attachments))
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT cursor_after FROM sync_runs WHERE id=?`), syncID).Scan(&cursor))
	assertions.Equal("synthetic retained body", body)
	assertions.Equal("synthetic retained snippet", snippet)
	assertions.Equal(1, attachments)
	assertions.Equal("cursor-value", cursor)
}

func TestConversationPolicyTransitionMissingStateEligibility(t *testing.T) {
	for _, sourceType := range []string{"beeper", "imap"} {
		t.Run(sourceType, func(t *testing.T) {
			requirements := require.New(t)

			s := testutil.NewTestStore(t)
			source, err := s.GetOrCreateSource(sourceType, "eligibility@example.com")
			requirements.NoError(err)
			room, err := s.EnsureConversation(source.ID, "room", "Room")
			requirements.NoError(err)
			message, err := s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "message", MessageType: sourceType})
			requirements.NoError(err)
			_, err = s.DB().Exec(s.Rebind(`DELETE FROM conversation_policy WHERE conversation_id=?`), room)
			requirements.NoError(err)
			revision, err := s.ConversationPolicyRevision(t.Context())
			requirements.NoError(err)
			err = s.CheckConversationPolicyWork(t.Context(), revision, []int64{message})
			if sourceType == "beeper" {
				requirements.ErrorIs(err, store.ErrConversationPolicyStale)
			} else {
				requirements.NoError(err)
			}
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "room", conversationpolicy.Cold))
			revision, err = s.ConversationPolicyRevision(t.Context())
			requirements.NoError(err)
			requirements.ErrorIs(s.CheckConversationPolicyWork(t.Context(), revision, []int64{message}), store.ErrConversationPolicyStale)
		})
	}
}

func TestConversationPolicyExpiryIncludesConfiguredUnsupportedOverride(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("gmail", "override@example.test")
	requirements.NoError(err)
	settings := conversationpolicy.RecommendedSettings()
	settings.Overrides = []conversationpolicy.Override{{
		SourceType: "gmail", SourceIdentifier: "override@example.test",
		ConversationID: "managed-thread", Tier: conversationpolicy.Cold,
	}}
	_, err = s.ConfigureConversationPolicy(t.Context(), settings, time.Now())
	requirements.NoError(err)
	managed, err := s.EnsureConversation(source.ID, "managed-thread", "Managed thread")
	requirements.NoError(err)
	unmanaged, err := s.EnsureConversation(source.ID, "ordinary-thread", "Ordinary thread")
	requirements.NoError(err)
	_, err = s.ExpireConversationPolicy(t.Context(), time.Now())
	requirements.NoError(err)
	rows, err := s.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	var managedTier conversationpolicy.Tier
	var unmanagedTier conversationpolicy.Tier
	for _, row := range rows {
		switch row.ConversationID {
		case managed:
			managedTier = row.EffectiveTier
		case unmanaged:
			unmanagedTier = row.EffectiveTier
		}
	}
	assertions.Equal(conversationpolicy.Cold, managedTier)
	assertions.Equal(conversationpolicy.Full, unmanagedTier)
	var unmanagedPolicyRows int
	requirements.NoError(s.DB().QueryRowContext(t.Context(), s.Rebind(`SELECT COUNT(*) FROM conversation_policy WHERE conversation_id=?`), unmanaged).Scan(&unmanagedPolicyRows))
	assertions.Zero(unmanagedPolicyRows, "ordinary email threads do not get periodic policy rows")

	settings.Overrides = nil
	_, err = s.ConfigureConversationPolicy(t.Context(), settings, time.Now())
	requirements.NoError(err)
	var removedOverrideTier string
	requirements.NoError(s.DB().QueryRowContext(t.Context(), s.Rebind(`SELECT effective_tier FROM conversation_policy WHERE conversation_id=?`), managed).Scan(&removedOverrideTier))
	assertions.Equal(string(conversationpolicy.Full), removedOverrideTier,
		"removing an explicit config override restores the default tier")
}

func TestConversationPolicyBootstrapAttributionDrift(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("beeper", "attribution")
	requirements.NoError(err)
	_, err = s.EnsureConversation(source.ID, "room", "Room")
	requirements.NoError(err)
	settings := conversationpolicy.RecommendedSettings()
	before, err := s.ConfigureConversationPolicy(t.Context(), settings, time.Now())
	requirements.NoError(err)
	var previous string
	requirements.NoError(s.DB().QueryRow(`SELECT value FROM archive_metadata WHERE key='conversation_policy_activity_dirty_revision'`).Scan(&previous))
	requirements.NoError(s.AddAccountIdentity(source.ID, "self@example.com", "manual"))
	after, err := s.ConfigureConversationPolicy(t.Context(), settings, time.Now())
	requirements.NoError(err)
	assertions.Greater(after.Revision, before.Revision)
	var dirty string
	requirements.NoError(s.DB().QueryRow(`SELECT value FROM archive_metadata WHERE key='conversation_policy_activity_dirty_revision'`).Scan(&dirty))
	assertions.NotEqual(previous, dirty)
	again, err := s.ConfigureConversationPolicy(t.Context(), settings, time.Now())
	requirements.NoError(err)
	assertions.Equal(after.Revision, again.Revision)
}
