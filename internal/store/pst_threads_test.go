package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func pstThreadMessage(t *testing.T, st *store.Store, sourceID int64, sourceKey, rfcID string) (int64, int64) {
	t.Helper()
	require := require.New(t)
	conv, err := st.EnsureConversation(sourceID, sourceKey, "Thread")
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: sourceID, ConversationID: conv, SourceMessageID: sourceKey + "/" + rfcID, MessageType: "email", RFC822MessageID: sql.NullString{String: rfcID, Valid: rfcID != ""}})
	require.NoError(err)
	return id, conv
}
func pstMessageConversation(t *testing.T, st *store.Store, id int64) int64 {
	t.Helper()
	require := require.New(t)
	var conv int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_id FROM messages WHERE id = ?`), id).Scan(&conv))
	return conv
}

func TestPstThreadReconciliation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	root, rootConv := pstThreadMessage(t, st, f.Source.ID, "root@example.test", "root@example.test")
	middle, middleConv := pstThreadMessage(t, st, f.Source.ID, "legacy-middle", "middle@example.test")
	child, childConv := pstThreadMessage(t, st, f.Source.ID, "middle@example.test", "child@example.test")
	require.NoError(st.RecordPstEmailHeadersContext(t.Context(), f.Source.ID, middle, "middle@example.test", "root@example.test", "root@example.test"))
	require.NoError(st.RecordPstEmailHeadersContext(t.Context(), f.Source.ID, child, "child@example.test", "middle@example.test", "middle@example.test"))
	other, err := st.GetOrCreateSource("pst", "other@example.test")
	require.NoError(err)
	unrelated, unrelatedConv := pstThreadMessage(t, st, other.ID, "middle@example.test", "other@example.test")
	before, err := st.DerivedDataRevision()
	require.NoError(err)
	require.NoError(st.ReconcilePstEmailThreadsContext(t.Context(), f.Source.ID))
	for _, id := range []int64{root, middle, child} {
		assert.Equal(rootConv, pstMessageConversation(t, st, id))
	}
	assert.Equal(unrelatedConv, pstMessageConversation(t, st, unrelated))
	after, err := st.DerivedDataRevision()
	require.NoError(err)
	assert.Greater(after, before)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT message_count FROM conversations WHERE id = ?`), rootConv).Scan(&count))
	assert.Equal(3, count)
	for _, old := range []int64{middleConv, childConv} {
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT message_count FROM conversations WHERE id = ?`), old).Scan(&count))
		assert.Zero(count)
	}
	// This references-only message enters the old, now-empty middle key.
	later, _ := pstThreadMessage(t, st, f.Source.ID, "middle@example.test", "later@example.test")
	require.NoError(st.RecordPstEmailHeadersContext(t.Context(), f.Source.ID, later, "later@example.test", "", "middle@example.test"))
	require.NoError(st.ReconcilePstEmailThreadsContext(t.Context(), f.Source.ID))
	assert.Equal(rootConv, pstMessageConversation(t, st, later))
	stable, err := st.DerivedDataRevision()
	require.NoError(err)
	require.NoError(st.ReconcilePstEmailThreadsContext(t.Context(), f.Source.ID))
	again, err := st.DerivedDataRevision()
	require.NoError(err)
	assert.Equal(stable, again)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(st.ReconcilePstEmailThreadsContext(ctx, f.Source.ID), context.Canceled)
}

func TestPstThreadAliasEligibility(t *testing.T) {
	for _, state := range []string{"unique", "duplicate", "hidden", "cross source", "cycle"} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			st := f.Store
			parent, parentConv := pstThreadMessage(t, st, f.Source.ID, "old-parent", "parent@example.test")
			child, childConv := pstThreadMessage(t, st, f.Source.ID, "parent@example.test", "child@example.test")
			switch state {
			case "duplicate":
				pstThreadMessage(t, st, f.Source.ID, "duplicate", "<parent@example.test>")
			case "hidden":
				_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), parent)
				require.NoError(err)
			case "cross source":
				other, err := st.GetOrCreateSource("pst", "other@example.test")
				require.NoError(err)
				_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET source_id = ? WHERE id = ?`), other.ID, parent)
				require.NoError(err)
			case "cycle":
				require.NoError(st.RecordPstEmailHeadersContext(t.Context(), f.Source.ID, parent, "parent@example.test", "child@example.test", "child@example.test"))
			}
			// A stale pointer alone must never override alias eligibility.
			_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET reply_to_message_id = ? WHERE id = ?`), parent, child)
			require.NoError(err)
			require.NoError(st.ReconcilePstEmailThreadsContext(t.Context(), f.Source.ID))
			want := childConv
			if state == "unique" || state == "cycle" {
				want = parentConv
			}
			assert.Equal(want, pstMessageConversation(t, st, child))
		})
	}
}

func TestPstThreadReconciliationFencesSupersededSync(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	old, err := f.Store.StartSync(f.Source.ID, "import-pst")
	require.NoError(err)
	stale := f.Store.ScopedToSync(f.Source.ID, old)
	require.NoError(f.Store.FailSync(old, "interrupted"))
	_, err = f.Store.StartSync(f.Source.ID, "import-pst")
	require.NoError(err)
	require.ErrorIs(stale.ReconcilePstEmailThreadsContext(t.Context(), f.Source.ID), store.ErrSyncRunSuperseded)
}

func TestPstThreadAcceptedParentWithMissingReferenceRoot(t *testing.T) {
	for _, state := range []string{"unique", "duplicate", "hidden", "cross source"} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			st := f.Store
			parent, parentConv := pstThreadMessage(t, st, f.Source.ID, "parent@example.test", "parent@example.test")
			child, childConv := pstThreadMessage(t, st, f.Source.ID, "missing-root@example.test", "child@example.test")
			require.NoError(st.RecordPstEmailHeadersContext(t.Context(), f.Source.ID, child, "child@example.test", "parent@example.test", "missing-root@example.test"))
			switch state {
			case "duplicate":
				pstThreadMessage(t, st, f.Source.ID, "duplicate", "<parent@example.test>")
			case "hidden":
				_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), parent)
				require.NoError(err)
			case "cross source":
				other, err := st.GetOrCreateSource("pst", "other@example.test")
				require.NoError(err)
				_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET source_id = ? WHERE id = ?`), other.ID, parent)
				require.NoError(err)
			}
			require.NoError(st.ResolveEmailReplyParentsContext(t.Context(), f.Source.ID, 0, nil))
			require.NoError(st.ReconcilePstEmailThreadsContext(t.Context(), f.Source.ID))
			want := childConv
			if state == "unique" {
				want = parentConv
			}
			assert.Equal(want, pstMessageConversation(t, st, child))
		})
	}
}
