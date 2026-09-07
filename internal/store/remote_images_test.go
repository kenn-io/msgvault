package store_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestRemoteImageBackfillIncludesLegacyEmailTypes(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, sourceID, conversationID := newLegacyNullableMessageTypeStore(t)
	var want []int64
	for i, messageType := range []any{"email", "", nil, "slack"} {
		id, err := st.UpsertMessage(&store.Message{
			SourceID: sourceID, ConversationID: conversationID,
			SourceMessageID: fmt.Sprintf("remote-image-%d", i), MessageType: store.MessageTypeEmail,
		})
		require.NoError(err)
		// Persist the historical representation; normal ingestion defaults to email.
		_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET message_type = ? WHERE id = ?`), messageType, id)
		require.NoError(err)
		if i < 3 {
			want = append(want, id)
		}
	}

	ids, err := st.RemoteImageBackfillMessageIDs(t.Context(), 0, sourceID, 100)
	require.NoError(err)
	assert.Equal(want, ids, "explicit, empty, and NULL email types must all be eligible")
	ids, err = st.RemoteImageBackfillMessageIDs(t.Context(), want[0], sourceID, 1)
	require.NoError(err)
	assert.Equal(want[1:2], ids, "legacy email must respect the cursor and page limit")
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), want[2])
	require.NoError(err)
	ids, err = st.RemoteImageBackfillMessageIDs(t.Context(), 0, sourceID, 100)
	require.NoError(err)
	assert.Equal(want[:2], ids, "deleted legacy email must stay excluded")
}
