package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/testutil"
)

func TestCountMessagesBySourceMatchesPerSourceCounts(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	a, err := st.GetOrCreateSource("gmail", "a@example.com")
	require.NoError(t, err)
	b, err := st.GetOrCreateSource("imap", "b@example.com")
	require.NoError(t, err)
	caldav, err := st.GetOrCreateSource("caldav", "calendar@example.com")
	require.NoError(t, err)
	empty, err := st.GetOrCreateSource("imap", "empty@example.com")
	require.NoError(t, err)

	insert := func(sourceID int64, key, messageType string, deleted, sourceDeleted bool) {
		t.Helper()
		_, err := st.DB().Exec(st.Rebind(`
			INSERT INTO conversations (source_id, source_conversation_id, conversation_type) VALUES (?, ?, 'email_thread')`), sourceID, key)
		require.NoError(t, err)
		var convID int64
		require.NoError(t, st.DB().QueryRow(st.Rebind(
			`SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?`), sourceID, key).Scan(&convID))
		_, err = st.DB().Exec(st.Rebind(`
			INSERT INTO messages (source_id, source_message_id, conversation_id, message_type,
				deleted_at, deleted_from_source_at)
			VALUES (?, ?, ?, ?,
				CASE WHEN ? THEN CURRENT_TIMESTAMP END,
				CASE WHEN ? THEN CURRENT_TIMESTAMP END)`),
			sourceID, key, convID, messageType, deleted, sourceDeleted)
		require.NoError(t, err)
	}
	insert(a.ID, "a-live-1", "email", false, false)
	insert(a.ID, "a-live-2", "email", false, false)
	insert(a.ID, "a-source-deleted", "email", false, true)
	insert(a.ID, "a-dedup-hidden", "email", true, false)
	insert(a.ID, "a-hidden-and-source-deleted", "email", true, true)
	insert(b.ID, "b-live", "email", false, false)
	insert(b.ID, "b-source-deleted", "email", false, true)
	insert(caldav.ID, "c-calendar-live", "calendar_event", false, false)
	insert(caldav.ID, "c-calendar-source-deleted", "calendar_event", false, true)

	counts, err := st.CountMessagesBySourceContext(ctx)
	require.NoError(t, err)
	for _, src := range []int64{a.ID, b.ID, caldav.ID, empty.ID} {
		live, err := st.CountMessagesForSourceContext(ctx, src)
		require.NoError(t, err)
		sourceDeleted, err := st.CountSourceDeletedMessagesContext(ctx, src)
		require.NoError(t, err)
		assert.Equal(t, live, counts[src].Live, "live count for source %d", src)
		assert.Equal(t, sourceDeleted, counts[src].SourceDeleted, "source-deleted count for source %d", src)
	}
	assert.EqualValues(t, 2, counts[a.ID].Live)
	assert.EqualValues(t, 1, counts[a.ID].SourceDeleted)
	assert.EqualValues(t, 1, counts[caldav.ID].Live)
	assert.EqualValues(t, 1, counts[caldav.ID].SourceDeleted)
}
