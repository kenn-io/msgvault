package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestReconcileCalendarEventSourceMessageIDSkipsFTSInvalidationWhenUnavailable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		t.Skip("FTS5 availability is SQLite-specific")
	}

	source, err := st.GetOrCreateSource("gcal", "calendar@example.test")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "series-example", "Series")
	require.NoError(err)
	createEvent := func(sourceMessageID string) int64 {
		id, upsertErr := st.UpsertMessage(&store.Message{
			SourceID: source.ID, ConversationID: conversationID,
			SourceMessageID: sourceMessageID, MessageType: "calendar_event",
		})
		require.NoError(upsertErr)
		require.NoError(st.SetMessageMetadata(id, sql.NullString{
			String: `{"status":"confirmed","all_day":false,"start":"2027-03-09T17:00:00Z","end":"2027-03-09T18:00:00Z"}`,
			Valid:  true,
		}))
		require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "", Valid: true}, sql.NullString{}))
		require.NoError(st.UpsertMessageRawWithFormat(id, []byte(`{"id":"instance-example","status":"confirmed","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-09T17:00:00Z"},"start":{"dateTime":"2027-03-09T17:00:00Z"},"end":{"dateTime":"2027-03-09T18:00:00Z"}}`), "gcal_json"))
		require.NoError(st.UpsertFTS(id, "Archived event", "Calendar snapshot", "", "", ""))
		return id
	}
	legacyID := createEvent("instance-example")
	duplicateID := createEvent("series-example|2027-03-09T17:00:00Z")

	store.SetFTS5AvailableForTest(st, false)

	err = st.ReconcileCalendarEventSourceMessageIDContext(
		t.Context(), source.ID, "instance-example", "series-example|2027-03-09T17:00:00Z",
	)
	require.NoError(err, "reconciliation must not query the optional FTS table")

	var retainedID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`,
	), source.ID, "series-example|2027-03-09T17:00:00Z").Scan(&retainedID))
	assert.Equal(legacyID, retainedID, "the legacy archive message ID is retained")
	for _, messageID := range []int64{legacyID, duplicateID} {
		var ftsRows int
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
			`SELECT COUNT(*) FROM messages_fts WHERE rowid = ?`,
		), messageID).Scan(&ftsRows))
		assert.Equal(1, ftsRows, "FTS invalidation is skipped for unavailable FTS5 on message %d", messageID)
	}
	var duplicateCount int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_id = ? AND id <> ?`,
	), source.ID, legacyID).Scan(&duplicateCount))
	assert.Zero(duplicateCount, "the duplicate row is removed")
}
