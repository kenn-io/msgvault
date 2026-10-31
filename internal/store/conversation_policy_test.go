package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

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
	a, err := st.GetOrCreateSource("beeper", "policy-account-a")
	requirements.NoError(err)
	b, err := st.GetOrCreateSource("beeper", "policy-account-b")
	requirements.NoError(err)
	first, err := st.EnsureConversation(a.ID, "shared-room", "Room A")
	requirements.NoError(err)
	_, err = st.EnsureConversation(b.ID, "shared-room", "Room B")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationMetadata(first, sql.NullString{String: `{"member_count":27}`, Valid: true}))
	rows, err := st.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	requirements.Len(rows, 2)
	assertions.Equal(27, rows[0].Participants)
	assertions.True(rows[0].KnownSize)
	assertions.False(rows[1].KnownSize)
	assertions.Equal(conversationpolicy.Full, rows[0].EffectiveTier)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), a.ID, "shared-room", conversationpolicy.Cold))
	revision, err := st.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	assertions.Equal(int64(1), revision)
	rows, err = st.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.Cold, rows[0].Override)
	assertions.Empty(rows[1].Override)
	assertions.Equal(conversationpolicy.Full, rows[0].EffectiveTier, "configuration alone must not enable cold eligibility")
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
	source, err := st.GetOrCreateSource("beeper", "policy-scope")
	requirements.NoError(err)
	_, err = st.EnsureConversation(source.ID, "room", "Room")
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
	assertions.Zero(revision)
}

func TestConversationPolicyUnresolvedRoster(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("discord", "policy-floor")
	requirements.NoError(err)
	for _, metadata := range []string{`{}`, `{"member_count":-1}`, `{"member_count":50,"member_count_unknown":true}`, `{"member_count":20}`, `{"member_count":20,"roster_repair_pending":true}`} {
		id, err := st.EnsureConversation(src.ID, metadata, "Room")
		requirements.NoError(err)
		requirements.NoError(st.SetConversationMetadata(id, sql.NullString{String: metadata, Valid: true}))
		_, err = st.DB().Exec(st.Rebind(`UPDATE conversations SET participant_count=30 WHERE id=?`), id)
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
	_, err := st.DB().Exec(`DROP TABLE conversation_policy`)
	requirements.NoError(err)
	_, err = st.DB().Exec(`UPDATE archive_metadata SET value='10' WHERE key='schema_version'`)
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
	st, err := store.OpenForTest(path)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	requirements.NoError(st.InitSchemaContext(t.Context()))
	source, err := st.GetOrCreateSource("beeper", "subset-policy")
	requirements.NoError(err)
	room, err := st.EnsureConversation(source.ID, "room", "Subset room")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "message", MessageType: "beeper"})
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "room", conversationpolicy.Cold))
	dest := filepath.Join(dir, "subset")
	_, err = store.CopySubset(path, dest, 1, false)
	requirements.NoError(err)
	copied, err := store.OpenForTest(filepath.Join(dest, "msgvault.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(copied.Close()) })
	rows, err := copied.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(conversationpolicy.Cold, rows[0].Override)
	assertions.Equal(conversationpolicy.Full, rows[0].EffectiveTier)
}
