package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXSourceMergeRefusesBeforeMutation(t *testing.T) {
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	into, err := st.GetOrCreateSource("x", "1002")
	require.NoError(t, err)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	post, err := st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: from.ID, OwnerID: "1001", AuthorID: "1001", PostID: "101", ConversationID: "101", EditHistory: []string{"101"}, Text: "synthetic owner-bound post", CreatedAt: at, FetchedAt: at, Availability: "public"})
	require.NoError(t, err)
	for _, dry := range []bool{true, false} {
		_, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID, DryRun: dry})
		require.ErrorIs(t, err, store.ErrSourceMergeInvalid)
		current, err := st.GetSourceByID(from.ID)
		require.NoError(t, err)
		assert.Zero(t, current.MergedIntoSourceID)
		target, err := st.ResolveXPostContext(t.Context(), from.ID, "101")
		require.NoError(t, err)
		assert.Equal(t, post.MessageID, target.MessageID)
		var sourceID int64
		require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT source_id FROM messages WHERE id=?"), post.MessageID).Scan(&sourceID))
		assert.Equal(t, from.ID, sourceID)
	}
}

func TestXMergeGuardPreservesImportedProviderMerges(t *testing.T) {
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("mbox", "first@example.test")
	require.NoError(t, err)
	into, err := st.GetOrCreateSource("mbox", "second@example.test")
	require.NoError(t, err)
	conversation, err := st.EnsureConversationWithType(from.ID, "synthetic-thread", "email_thread", "Synthetic import")
	require.NoError(t, err)
	message, err := st.UpsertMessage(&store.Message{SourceID: from.ID, ConversationID: conversation, SourceMessageID: "synthetic-message", MessageType: "email"})
	require.NoError(t, err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	require.NoError(t, err)
	current, err := st.GetSourceByID(from.ID)
	require.NoError(t, err)
	assert.Equal(t, into.ID, current.MergedIntoSourceID)
	var sourceID int64
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT source_id FROM messages WHERE id=?"), message).Scan(&sourceID))
	assert.Equal(t, into.ID, sourceID)
}
