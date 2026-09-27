package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/testutil"
)

func TestCountMessagesBySourceMatchesPerSourceCounts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	a, err := st.GetOrCreateSource("gmail", "a@example.com")
	require.NoError(err)
	b, err := st.GetOrCreateSource("imap", "b@example.com")
	require.NoError(err)
	empty, err := st.GetOrCreateSource("imap", "empty@example.com")
	require.NoError(err)

	insert := func(sourceID int64, key string, deleted, sourceDeleted bool) {
		t.Helper()
		_, err := st.DB().Exec(st.Rebind(`
			INSERT INTO conversations (source_id, source_conversation_id, conversation_type) VALUES (?, ?, 'email_thread')`), sourceID, key)
		require.NoError(err)
		var convID int64
		require.NoError(st.DB().QueryRow(st.Rebind(
			`SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?`), sourceID, key).Scan(&convID))
		_, err = st.DB().Exec(st.Rebind(`
			INSERT INTO messages (source_id, source_message_id, conversation_id, message_type,
				deleted_at, deleted_from_source_at)
			VALUES (?, ?, ?, 'email',
				CASE WHEN ? THEN CURRENT_TIMESTAMP END,
				CASE WHEN ? THEN CURRENT_TIMESTAMP END)`),
			sourceID, key, convID, deleted, sourceDeleted)
		require.NoError(err)
	}
	insert(a.ID, "a-live-1", false, false)
	insert(a.ID, "a-live-2", false, false)
	insert(a.ID, "a-source-deleted", false, true)
	insert(a.ID, "a-dedup-hidden", true, false)
	insert(a.ID, "a-hidden-and-source-deleted", true, true)
	insert(b.ID, "b-live", false, false)
	insert(b.ID, "b-source-deleted", false, true)

	counts, err := st.CountMessagesBySourceContext(ctx)
	require.NoError(err)
	for _, src := range []int64{a.ID, b.ID, empty.ID} {
		live, err := st.CountMessagesForSourceContext(ctx, src)
		require.NoError(err)
		sourceDeleted, err := st.CountSourceDeletedMessagesContext(ctx, src)
		require.NoError(err)
		assert.Equal(live, counts[src].Live, "live count for source %d", src)
		assert.Equal(sourceDeleted, counts[src].SourceDeleted, "source-deleted count for source %d", src)
	}
	assert.EqualValues(2, counts[a.ID].Live)
	assert.EqualValues(1, counts[a.ID].SourceDeleted)
}
