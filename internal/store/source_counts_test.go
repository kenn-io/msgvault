package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// requireStoredCountsMatch checks the stored counts of every source against
// a fresh COUNT over messages. A source without a stored row must count zero.
func requireStoredCountsMatch(t *testing.T, st *store.Store, step string) {
	t.Helper()
	ctx := t.Context()
	stored, err := st.SourceMessageCountsContext(ctx)
	require.NoError(t, err, step)
	sources, err := st.ListSources("")
	require.NoError(t, err, step)
	seen := make(map[int64]bool)
	for _, src := range sources {
		seen[src.ID] = true
		live, err := st.CountMessagesForSourceContext(ctx, src.ID)
		require.NoError(t, err, step)
		sourceDeleted, err := st.CountSourceDeletedMessagesContext(ctx, src.ID)
		require.NoError(t, err, step)
		assert.Equal(t, store.SourceMessageCounts{Live: live, SourceDeleted: sourceDeleted}, stored[src.ID],
			"%s: counts for source %d", step, src.ID)
	}
	for sourceID := range stored {
		assert.True(t, seen[sourceID], "%s: counts row for missing source %d", step, sourceID)
	}
}

func upsertCountedMessage(t *testing.T, st *store.Store, sourceID, convID int64, sourceMessageID string) int64 {
	t.Helper()
	id, err := st.UpsertMessage(&store.Message{
		SourceID: sourceID, ConversationID: convID, SourceMessageID: sourceMessageID, MessageType: "email",
	})
	require.NoError(t, err, sourceMessageID)
	return id
}

// TestSourceMessageCountsFollowWrites drives each kind of message write
// through the store and compares the stored counts with a fresh COUNT after
// every step.
func TestSourceMessageCountsFollowWrites(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	check := func(step string) { t.Helper(); requireStoredCountsMatch(t, st, step) }

	alice, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	bob, err := st.GetOrCreateSource("gmail", "bob@example.com")
	require.NoError(err)
	chat, err := st.GetOrCreateSource("discord", "guild-1")
	require.NoError(err)
	_, err = st.GetOrCreateSource("imap", "empty@example.com")
	require.NoError(err)
	check("new sources")

	aliceThread, err := st.EnsureConversation(ctx, alice.ID, "thread-a", "")
	require.NoError(err)
	bobThread, err := st.EnsureConversation(ctx, bob.ID, "thread-b", "")
	require.NoError(err)
	a1 := upsertCountedMessage(t, st, alice.ID, aliceThread, "a-1")
	upsertCountedMessage(t, st, alice.ID, aliceThread, "a-2")
	a3 := upsertCountedMessage(t, st, alice.ID, aliceThread, "a-3")
	upsertCountedMessage(t, st, alice.ID, aliceThread, "a-4")
	upsertCountedMessage(t, st, bob.ID, bobThread, "b-1")
	upsertCountedMessage(t, st, bob.ID, bobThread, "b-2")
	check("insert")
	upsertCountedMessage(t, st, alice.ID, aliceThread, "a-1")
	check("re-upsert of an existing message")

	require.NoError(st.MarkMessageDeletedContext(ctx, alice.ID, "a-2"))
	check("delete from source")
	require.NoError(st.MarkMessageDeletedContext(ctx, alice.ID, "a-2"))
	check("repeated delete from source")
	require.NoError(st.MarkMessagesDeletedByGmailIDBatch(ctx, []string{"b-1", "b-2"}))
	check("batched delete from source")

	require.NoError(st.MigrateSourceMessageID(ctx, alice.ID, aliceThread, "a-2", "a-2-renamed"))
	check("migrated source message ID clears the deletion marker")

	_, err = st.MergeDuplicates(ctx, a1, []int64{a3}, "batch-1")
	require.NoError(err)
	check("dedup hides a duplicate")
	restored, err := st.UndoDedup(ctx, "batch-1")
	require.NoError(err)
	assert.EqualValues(1, restored)
	check("undo dedup")
	_, err = st.MergeDuplicates(ctx, a1, []int64{a3}, "batch-2")
	require.NoError(err)
	deleted, err := st.DeleteDedupedBatchesContext(ctx, []string{"batch-2"})
	require.NoError(err)
	assert.EqualValues(1, deleted)
	check("hard delete of a dedup batch")

	channel, err := st.EnsureConversation(ctx, chat.ID, "channel-1", "Removed channel")
	require.NoError(err)
	kept, err := st.EnsureConversation(ctx, chat.ID, "channel-2", "Kept channel")
	require.NoError(err)
	upsertCountedMessage(t, st, chat.ID, channel, "c-1")
	upsertCountedMessage(t, st, chat.ID, channel, "c-2")
	upsertCountedMessage(t, st, chat.ID, kept, "c-3")
	check("chat messages")
	require.NoError(st.PurgeChannelContext(ctx, chat.ID, "channel-1"))
	check("channel purge")

	_, err = st.MergeSourcesContext(ctx, store.MergeSourcesRequest{FromSourceID: bob.ID, IntoSourceID: alice.ID})
	require.NoError(err)
	check("source merge")

	require.NoError(st.RemoveSource(chat.ID))
	check("source removal")

	stored, err := st.SourceMessageCountsContext(ctx)
	require.NoError(err)
	assert.NotContains(stored, chat.ID, "a removed source leaves no counts row")
}

func TestSourceMessageCountsMigrationRecounts(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	conv, err := st.EnsureConversation(ctx, src.ID, "thread", "")
	require.NoError(err)
	upsertCountedMessage(t, st, src.ID, conv, "m-1")
	upsertCountedMessage(t, st, src.ID, conv, "m-2")
	require.NoError(st.MarkMessageDeletedContext(ctx, src.ID, "m-2"))

	// Simulate an archive that predates the counts: wrong stored values and
	// no ledger entry. Initialization must recount from messages.
	_, err = st.DB().ExecContext(ctx, `UPDATE source_message_counts SET live_count = 99, source_deleted_count = 99`)
	require.NoError(err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), "source_message_counts_v1")
	require.NoError(err)
	require.NoError(st.InitSchemaContext(ctx))
	requireStoredCountsMatch(t, st, "after migration")

	stored, err := st.SourceMessageCountsContext(ctx)
	require.NoError(err)
	require.Equal(store.SourceMessageCounts{Live: 1, SourceDeleted: 1}, stored[src.ID])
}
