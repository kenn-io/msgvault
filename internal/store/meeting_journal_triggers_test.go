package store

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingJournalTriggers(t *testing.T) {
	for _, tc := range []struct {
		name, decision, query string
	}{
		{name: "insert", query: `INSERT INTO meeting_link_decisions(member_message_id,target_message_id,decision) VALUES ($note, $event, 'link')`},
		{name: "update", decision: "link", query: `UPDATE meeting_link_decisions SET decision='split' WHERE member_message_id=$note`},
		{name: "delete", decision: "link", query: `DELETE FROM meeting_link_decisions WHERE member_message_id=$note`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			if st.IsPostgreSQL() {
				t.Skip("SQLite cache journal")
			}
			_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.test"))
			_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{}}`)
			if tc.decision == "link" {
				require.NoError(t, st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
			}
			_, err := st.db.Exec(`DELETE FROM cache_related_change_journal`)
			require.NoError(t, err)
			query := strings.NewReplacer("$event", strconv.FormatInt(event, 10), "$note", strconv.FormatInt(note, 10)).Replace(tc.query)
			_, err = st.db.Exec(query)
			require.NoError(t, err)
			var id int64
			require.NoError(t, st.db.QueryRow(`SELECT message_id FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&id))
			assert.Equal(t, note, id)
		})
	}
}

func TestMeetingLinkEvidenceChangesJournal(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite cache journal")
	}
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"first@example.org"},"summary":"one"}`)
	var before, after int64
	require.NoError(t, st.db.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal`).Scan(&before))
	require.NoError(t, st.UpsertMessageRawWithFormat(note, []byte(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"first@example.org"},"summary":"two"}`), "twenty_json"))
	require.NoError(t, st.db.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&after))
	assert.LessOrEqual(t, after, before)
	require.NoError(t, st.UpsertMessageRawWithFormat(note, []byte(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"second@example.org"}}`), "twenty_json"))
	require.NoError(t, st.db.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&after))
	assert.Greater(t, after, before)
}
