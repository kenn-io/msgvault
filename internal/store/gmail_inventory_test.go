package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestGmailInventoryReadSnapshot(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	t.Parallel()
	st := testutil.NewTestStore(t)
	st.DB().SetMaxOpenConns(3)
	source, err := st.GetOrCreateSource("gmail", "snapshot@example.test")
	require.NoError(err)
	conversation, err := st.EnsureConversation(t.Context(), source.ID, "thread", "Synthetic snapshot")
	require.NoError(err)
	first, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "before", MessageType: store.MessageTypeEmail})
	require.NoError(err)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	require.NoError(err)
	defer release()
	original, err := st.ListGmailInventoryIdentitiesContext(ctx, source.ID, 10)
	require.NoError(err)
	require.Equal([]store.GmailInventoryIdentity{{ID: "before"}}, original)
	// This write uses another pool connection while the real reader transaction
	// stays open. Neither its row change nor its new identity may leak into it.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`), first)
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "during", MessageType: store.MessageTypeEmail})
	require.NoError(err)
	again, err := st.ListGmailInventoryIdentitiesContext(ctx, source.ID, 10)
	require.NoError(err)
	assert.Equal(original, again)
	release()
	fresh, err := st.ListGmailInventoryIdentitiesContext(t.Context(), source.ID, 10)
	require.NoError(err)
	assert.Equal([]store.GmailInventoryIdentity{{ID: "before", Hidden: true}, {ID: "during"}}, fresh)
}
