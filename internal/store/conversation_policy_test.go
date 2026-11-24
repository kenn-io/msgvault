package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConversationPolicyOverrides(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	_, policyErr := st.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	require.NoError(t, policyErr)
	a, err := st.GetOrCreateSource("beeper", "policy-account-a")
	requirements.NoError(err)
	b, err := st.GetOrCreateSource("beeper", "policy-account-b")
	requirements.NoError(err)
	first, err := st.EnsureConversation(t.Context(), a.ID, "shared-room", "Room A")
	requirements.NoError(err)
	_, err = st.EnsureConversation(t.Context(), b.ID, "shared-room", "Room B")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationMetadata(first, sql.NullString{String: `{"member_count":27}`, Valid: true}))
	rows, err := st.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	requirements.Len(rows, 2)
	assertions.Equal(27, rows[0].Participants)
	assertions.True(rows[0].KnownSize)
	assertions.False(rows[1].KnownSize)
	assertions.Equal(conversationpolicy.Cold, rows[0].EffectiveTier)
	beforeOverride, err := st.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), a.ID, "shared-room", conversationpolicy.Cold))
	revision, err := st.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	assertions.Equal(beforeOverride+1, revision)
	rows, err = st.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Cold, rows[0].Override)
	assertions.Empty(rows[1].Override)
	assertions.Equal(conversationpolicy.Cold, rows[0].EffectiveTier, "override immediately changes eligibility")
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), a.ID, "shared-room", conversationpolicy.Cold))
	unchanged, err := st.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	assertions.Equal(revision, unchanged)
	requirements.Error(st.SetConversationPolicyOverride(t.Context(), a.ID, "missing", conversationpolicy.Cold))
	requirements.Error(st.SetConversationPolicyOverride(t.Context(), a.ID, "shared-room", conversationpolicy.Stub))
	requirements.Error(st.SetConversationPolicyOverride(t.Context(), a.ID, "shared-room", conversationpolicy.Tier("invalid")))
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), a.ID, "shared-room", ""))
	rows, err = st.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	assertions.Empty(rows[0].Override)
	reset, err := st.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	assertions.Equal(revision+1, reset)
}

func TestConversationPolicyScopeAndRollback(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	before, err := st.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	source, err := st.GetOrCreateSource("beeper", "policy-scope")
	requirements.NoError(err)
	_, err = st.EnsureConversation(t.Context(), source.ID, "room", "Room")
	requirements.NoError(err)
	denied := sourcescope.WithSourceIDs(t.Context(), []int64{})
	rows, err := st.ListConversationPolicies(denied)
	requirements.NoError(err)
	assertions.Empty(rows)
	requirements.Error(st.SetConversationPolicyOverride(denied, source.ID, "room", conversationpolicy.Cold))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	requirements.ErrorIs(st.SetConversationPolicyOverride(ctx, source.ID, "room", conversationpolicy.Cold), context.Canceled)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "room", ""))
	revision, err := st.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	assertions.Equal(before+1, revision)
}

func TestConversationPolicyUnresolvedRoster(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("discord", "policy-floor")
	requirements.NoError(err)
	for _, metadata := range []string{`{}`, `{"member_count":-1}`, `{"member_count":50,"member_count_unknown":true}`, `{"member_count":20}`, `{"member_count":20,"roster_repair_pending":true}`} {
		id, err := st.EnsureConversation(t.Context(), src.ID, metadata, "Room")
		requirements.NoError(err)
		requirements.NoError(st.SetConversationMetadata(id, sql.NullString{String: metadata, Valid: true}))
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE conversations SET participant_count=30 WHERE id=?`), id)
		requirements.NoError(err)
	}
	rows, err := st.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	requirements.Len(rows, 5)
	for i, row := range rows {
		assertions.Equal(i == 3, row.KnownSize)
		assertions.GreaterOrEqual(row.Participants, 30)
	}
}

func TestConversationPolicyUpgrade(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.DB().ExecContext(t.Context(), `DROP TABLE conversation_policy`)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE archive_metadata SET value='10' WHERE key='schema_version'`)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	version, err := st.SchemaVersionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(store.SchemaVersion, version)
	rows, err := st.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	assertions.Empty(rows)
}

func TestConversationPolicySubset(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "source.db")
	st, err := store.OpenForTest(t.Context(), path)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	requirements.NoError(st.InitSchemaContext(t.Context()))
	source, err := st.GetOrCreateSource("beeper", "subset-policy")
	requirements.NoError(err)
	room, err := st.EnsureConversation(t.Context(), source.ID, "room", "Subset room")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "message", MessageType: "beeper"})
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "room", conversationpolicy.Cold))
	dest := filepath.Join(dir, "subset")
	_, err = store.CopySubset(t.Context(), path, dest, 1, false)
	requirements.NoError(err)
	copied, err := store.OpenForTest(t.Context(), filepath.Join(dest, "msgvault.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(copied.Close()) })
	rows, err := copied.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(conversationpolicy.Cold, rows[0].Override)
	assertions.Equal(conversationpolicy.Cold, rows[0].EffectiveTier)
}

func TestConversationPolicySubsetKeepsConfiguredClassification(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "source.db")
	st, err := store.OpenForTest(t.Context(), path)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	requirements.NoError(st.InitSchemaContext(t.Context()))
	source, err := st.GetOrCreateSource("beeper", "configured-subset-policy")
	requirements.NoError(err)
	room, err := st.EnsureConversation(t.Context(), source.ID, "large-room", "Large room")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "message", MessageType: "beeper"})
	requirements.NoError(err)
	requirements.NoError(st.SetConversationMemberCountContext(t.Context(), room, 21))
	// Classification, not an override, makes this quiet large room Cold. The
	// configured overrides name one copied room and one room outside the subset.
	settings := conversationpolicy.RecommendedSettings()
	settings.Overrides = []conversationpolicy.Override{
		{SourceType: "beeper", SourceIdentifier: "configured-subset-policy", ConversationID: "large-room", Tier: conversationpolicy.Cold},
		{SourceType: "beeper", SourceIdentifier: "excluded-account", ConversationID: "excluded-room", Tier: conversationpolicy.Full},
	}
	_, err = st.ConfigureConversationPolicy(t.Context(), settings, time.Now())
	requirements.NoError(err)
	rows, err := st.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	requirements.Len(rows, 1)
	requirements.Equal(conversationpolicy.Cold, rows[0].EffectiveTier)
	dest := filepath.Join(dir, "subset")
	_, err = store.CopySubset(t.Context(), path, dest, 1, false)
	requirements.NoError(err)
	copied, err := store.OpenForTest(t.Context(), filepath.Join(dest, "msgvault.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(copied.Close()) })
	rows, err = copied.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(conversationpolicy.Cold, rows[0].EffectiveTier,
		"the subset must classify copied rooms with the source's policy settings")
	copiedSettings, err := copied.ConversationPolicySettingsContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(settings.MaxParticipants, copiedSettings.MaxParticipants)
	assertions.Equal(settings.ActivityWindowDays, copiedSettings.ActivityWindowDays)
	assertions.Equal(settings.Overrides[:1], copiedSettings.Overrides,
		"overrides for rooms outside the subset must not reveal their accounts or rooms")
	// The copied settings and tiers match what the bootstrap after the copy
	// computes, so it changes nothing beyond a new archive's own setup.
	fresh, err := testutil.NewTestStore(t).ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	copiedRevision, err := copied.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	assertions.Equal(fresh, copiedRevision, "copying must not look like a policy change")
}

func TestConversationPolicySubsetBootstrapsLegacySource(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "source.db")
	st, err := store.OpenForTest(t.Context(), path)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	requirements.NoError(st.InitSchemaContext(t.Context()))
	source, err := st.GetOrCreateSource("beeper", "legacy-subset-policy")
	requirements.NoError(err)
	room, err := st.EnsureConversation(t.Context(), source.ID, "room", "Legacy room")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "message", MessageType: "beeper"})
	requirements.NoError(err)
	// An archive from before room policy has chats but no policy rows.
	_, err = st.DB().ExecContext(t.Context(), `DROP TABLE conversation_policy`)
	requirements.NoError(err)
	dest := filepath.Join(dir, "subset")
	_, err = store.CopySubset(t.Context(), path, dest, 1, false)
	requirements.NoError(err)
	copied, err := store.OpenForTest(t.Context(), filepath.Join(dest, "msgvault.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(copied.Close()) })
	rows, err := copied.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	requirements.Len(rows, 1, "the copied chat must be classified after the copy")
	assertions.Equal(conversationpolicy.Full, rows[0].EffectiveTier)
}

// A configured override names a room by its source. After a source merge moves
// the room, the override no longer matches and the room must not stay Cold.
func TestConversationPolicySourceMergeDropsStaleConfiguredOverride(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	from, err := st.GetOrCreateSource("imap", "history@example.test")
	requirements.NoError(err)
	into, err := st.GetOrCreateSource("imap", "live@example.test")
	requirements.NoError(err)
	room, err := st.EnsureConversation(ctx, from.ID, "thread-1", "Thread")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: from.ID, ConversationID: room, SourceMessageID: "message-1", MessageType: "email"})
	requirements.NoError(err)
	settings := conversationpolicy.DefaultSettings()
	settings.Overrides = []conversationpolicy.Override{{SourceType: "imap", SourceIdentifier: "history@example.test", ConversationID: "thread-1", Tier: conversationpolicy.Cold}}
	_, err = st.ConfigureConversationPolicy(ctx, settings, time.Now())
	requirements.NoError(err)
	tier := func() string {
		var effective string
		requirements.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT effective_tier FROM conversation_policy WHERE conversation_id = ?`), room).Scan(&effective))
		return effective
	}
	requirements.Equal(string(conversationpolicy.Cold), tier())
	_, err = st.MergeSourcesContext(ctx, store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	requirements.NoError(err)
	assertions.NotEqual(string(conversationpolicy.Cold), tier(), "the moved room no longer matches its configured override")
}

// A chat room without its classification is excluded, and queue
// processing skips its activity. Including it with its first override must
// replay that activity, as any change from excluded to included does.
func TestConversationPolicyFirstFullOverrideReplaysExcludedActivity(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	source, err := st.GetOrCreateSource("beeper", "first-override")
	requirements.NoError(err)
	room, err := st.EnsureConversation(ctx, source.ID, "new-room", "New room")
	requirements.NoError(err)
	message, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "message", MessageType: "beeper"})
	requirements.NoError(err)
	// An interrupted save leaves the room without its classification.
	_, err = st.DB().ExecContext(ctx, st.Rebind(`DELETE FROM conversation_policy WHERE conversation_id = ?`), room)
	requirements.NoError(err)
	// Queue processing has seen the message while the room was excluded.
	_, err = st.DB().ExecContext(ctx, `UPDATE activity_projection_queue SET processed_revision = revision`)
	requirements.NoError(err)

	requirements.NoError(st.SetConversationPolicyOverride(ctx, source.ID, "new-room", conversationpolicy.Full))
	queued, err := st.ListActivityProjectionQueueContext(ctx, 10)
	requirements.NoError(err)
	ids := make([]int64, 0, len(queued))
	for _, item := range queued {
		ids = append(ids, item.MessageID)
	}
	assertions.Contains(ids, message, "including the room must replay its activity")
}

// Deleting the last Discord source keeps its participants and their Discord
// identifiers. A name that only a Cold Discord room supported must stay
// unsupported, so the participant still counts as a chat participant.
func TestConversationPolicyDeletedDiscordSourceKeepsChatProvenance(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	source, err := st.GetOrCreateSource("discord", "guild-1")
	requirements.NoError(err)
	room, err := st.EnsureConversation(ctx, source.ID, "channel-1", "Channel")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(ctx, source.ID, "channel-1", conversationpolicy.Cold))
	member, err := st.EnsureParticipantByIdentifier("discord_user_id", "350", "Cold Member")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "message", MessageType: "discord", SenderID: sql.NullInt64{Int64: member, Valid: true}})
	requirements.NoError(err)
	nameSupported := func() bool {
		var supported bool
		requirements.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT EXISTS(SELECT 1 FROM participants p WHERE p.id = ? AND `+
			store.FullParticipantNameWhere("p.id", "p.display_name")+`)`), member).Scan(&supported))
		return supported
	}
	requirements.False(nameSupported(), "a Cold room does not support the name")
	requirements.NoError(st.RemoveSource(source.ID))
	assertions.False(nameSupported(), "deleting the source must not make the name independent")
}

// Contextual embeddings built before room policy named participants whatever
// supported them. The one-time people bootstrap must rebuild the ones that
// name a participant whose details only an excluded room supports.
func TestConversationPolicyPeopleBootstrapRebuildsUnsupportedContext(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	source, err := st.GetOrCreateSource("beeper", "bootstrap-context")
	requirements.NoError(err)
	cold, err := st.EnsureConversation(ctx, source.ID, "cold-room", "Cold")
	requirements.NoError(err)
	full, err := st.EnsureConversation(ctx, source.ID, "full-room", "Full")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(ctx, source.ID, "cold-room", conversationpolicy.Cold))
	requirements.NoError(st.SetConversationPolicyOverride(ctx, source.ID, "full-room", conversationpolicy.Full))
	member, err := st.EnsureParticipantByIdentifier("beeper", "cold-only-member", "Cold Only Name")
	requirements.NoError(err)
	requirements.NoError(st.EnsureConversationParticipant(cold, member, "member"))
	requirements.NoError(st.EnsureConversationParticipant(full, member, "member"))
	shared, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: full, SourceMessageID: "shared", MessageType: "beeper"})
	requirements.NoError(err)
	revision := func() int64 {
		var value int64
		requirements.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT embedding_revision FROM messages WHERE id = ?`), shared).Scan(&value))
		return value
	}
	// An archive from before people features honored policy has not run the
	// people bootstrap yet.
	_, err = st.DB().ExecContext(ctx, `DELETE FROM archive_metadata WHERE key = 'conversation_policy_people_bootstrapped'`)
	requirements.NoError(err)
	before := revision()
	_, err = st.ConfigureConversationPolicy(ctx, conversationpolicy.RecommendedSettings(), time.Now())
	requirements.NoError(err)
	assertions.Greater(revision(), before, "context naming an unsupported participant must be rebuilt")
}
