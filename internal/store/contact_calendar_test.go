package store

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCalendarAttendedIdentityMapReportsMixedRawOverflow(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := openTestStore(t)

	source, err := st.GetOrCreateSource("gmail", "identity-map@example.test")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "identity-map-budget", "Identity map budget")
	require.NoError(err)

	const olderUID = "older-ics-meeting@example.test"
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	olderICS, err := st.UpsertMessage(&Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "older-ics-meeting",
		MessageType:     "meeting_transcript",
		SentAt:          sql.NullTime{Time: now.Add(-time.Hour), Valid: true},
		IsFromMe:        true,
	})
	require.NoError(err)
	_, err = st.db.ExecContext(t.Context(), st.Rebind(`INSERT INTO meeting_link_key(message_id,kind,value,calendar_id) VALUES (?,'ical_uid',?,'')`), olderICS, olderUID)
	require.NoError(err)

	_, err = st.db.ExecContext(t.Context(), st.Rebind(`WITH digits(n) AS (VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9)),
numbered(n) AS (
 SELECT ones.n + 10*tens.n + 100*hundreds.n + 1000*thousands.n + 10000*ten_thousands.n + 1
 FROM digits ones CROSS JOIN digits tens CROSS JOIN digits hundreds CROSS JOIN digits thousands CROSS JOIN digits ten_thousands
)
INSERT INTO messages(conversation_id,source_id,source_message_id,message_type,sent_at,is_from_me)
SELECT ?,?, 'identity-overflow-decoy-' || n,'meeting_transcript',?,TRUE FROM numbered WHERE n<=10001`), conversationID, source.ID, now)
	require.NoError(err)
	_, err = st.db.ExecContext(t.Context(), st.Rebind(`INSERT INTO meeting_link_key(message_id,kind,value,calendar_id)
SELECT id,
 CASE WHEN id=(SELECT MAX(id) FROM messages WHERE source_id=? AND source_message_id LIKE 'identity-overflow-decoy-%') THEN 'ical_uid' ELSE 'google_event_id' END,
 CASE WHEN id=(SELECT MAX(id) FROM messages WHERE source_id=? AND source_message_id LIKE 'identity-overflow-decoy-%') THEN 'decoy-' || id || '@example.test' ELSE 'invalid-google-' || id END,
 ''
FROM messages WHERE source_id=? AND source_message_id LIKE 'identity-overflow-decoy-%'`), source.ID, source.ID, source.ID)
	require.NoError(err)
	var newestDecoyID int64
	require.NoError(st.db.QueryRowContext(t.Context(), st.Rebind(`SELECT MAX(id) FROM messages WHERE source_id=? AND source_message_id LIKE 'identity-overflow-decoy-%'`), source.ID).Scan(&newestDecoyID))

	identities, more, err := st.calendarAttendedIdentityMap(t.Context())
	require.NoError(err)
	assert.True(more, "the raw identity limit must report skipped provider keys")
	assert.Len(identities, 1)
	assert.Equal("ical_uid", identities[0].kind)
	assert.Equal(fmt.Sprintf("decoy-%d@example.test", newestDecoyID), identities[0].value)
	assert.NotEqual(olderUID, identities[0].value, "the older ICS mapping lies beyond the raw row limit")
}
