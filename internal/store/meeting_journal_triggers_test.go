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
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			if st.IsPostgreSQL() {
				t.Skip("SQLite cache journal")
			}
			_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.test"))
			_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{}}`)
			if tc.decision == "link" {
				requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
			}
			_, err := st.db.Exec(`DELETE FROM cache_related_change_journal`)
			requirements.NoError(err)
			query := strings.NewReplacer("$event", strconv.FormatInt(event, 10), "$note", strconv.FormatInt(note, 10)).Replace(tc.query)
			_, err = st.db.Exec(query)
			requirements.NoError(err)
			var id int64
			requirements.NoError(st.db.QueryRow(`SELECT message_id FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&id))
			assert.Equal(t, note, id)
		})
	}
}
