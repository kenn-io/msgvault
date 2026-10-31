package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXAuthorQueueCoversForeignOnlyChainsWithoutForeignPurge(t *testing.T) {
	st := testutil.NewTestStore(t)
	selected, err := st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	foreign, err := st.GetOrCreateSource("x", "1003")
	require.NoError(t, err)
	wrong, err := st.GetOrCreateSource("mbox", "example@example.test")
	require.NoError(t, err)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	var messages []int64
	for _, entry := range []struct {
		source            int64
		owner, id, author string
	}{{selected.ID, "1001", "301", "1001"}, {foreign.ID, "1003", "601", "1001"}, {foreign.ID, "1003", "701", "1002"}} {
		saved, err := st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: entry.source, OwnerID: entry.owner, PostID: entry.id, AuthorID: entry.author, ConversationID: entry.id, EditHistory: []string{entry.id}, Text: "synthetic queued author body", CreatedAt: at, FetchedAt: at, Availability: "public"})
		require.NoError(t, err)
		messages = append(messages, saved.MessageID)
	}
	_, err = st.ResolveXPostContext(t.Context(), selected.ID, "601")
	require.ErrorIs(t, err, store.ErrXPostNotFound)
	err = st.QueueXAuthorRemovalContext(t.Context(), wrong.ID, "1001")
	require.Error(t, err, "a non-X source cannot authorize global X author suppression")
	execution, err := st.AcquireSyncExecutionContext(t.Context(), selected.ID)
	require.NoError(t, err)
	defer func() { assert.NoError(t, execution.Release()) }()
	run, err := execution.StartSyncContext(t.Context(), "x", "")
	require.NoError(t, err)
	scoped := st.ScopedToSync(selected.ID, run)
	err = scoped.QueueXAuthorRemovalContext(t.Context(), foreign.ID, "1001")
	require.Error(t, err, "retain the selected source generation guard")
	pending, err := st.XCleanupPendingContext(t.Context())
	require.NoError(t, err)
	assert.False(t, pending)
	require.NoError(t, scoped.QueueXAuthorRemovalContext(t.Context(), selected.ID, "1001"))
	works, err := st.PendingXRemovalsContext(t.Context(), 0)
	require.NoError(t, err)
	assert.Len(t, works, 2, "both known author chains must be durably queued")
	var foreignPending, unrelatedPending bool
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT cleanup_pending FROM x_post_versions WHERE source_id=? AND post_id=?"), foreign.ID, "601").Scan(&foreignPending))
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT cleanup_pending FROM x_post_versions WHERE source_id=? AND post_id=?"), foreign.ID, "701").Scan(&unrelatedPending))
	assert.True(t, foreignPending)
	assert.False(t, unrelatedPending)
	_, err = scoped.RemoveXPostContentContext(t.Context(), foreign.ID, "601", "protected")
	require.Error(t, err)
	for _, id := range messages {
		var body string
		require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT body_text FROM message_bodies WHERE message_id=?"), id).Scan(&body))
		assert.Equal(t, "synthetic queued author body", body, "queueing never purges under another source's execution")
	}
	require.NoError(t, st.CompleteSyncContext(t.Context(), run, ""))
}
