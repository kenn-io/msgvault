package store

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingJournalTriggers(t *testing.T) {
	t.Run("source evidence", func(t *testing.T) {
		requirements, assertions := require.New(t), assert.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		if st.IsPostgreSQL() {
			t.Skip("SQLite cache journal")
		}
		raw := `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"first@example.org","startsAt":"2027-03-14T10:00:00Z"},"summary":"one"}`
		_, note := projectionFixture(t, st, "note", "twenty_json", raw)
		var before, after int64
		requirements.NoError(st.db.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&before))
		requirements.NoError(st.UpsertMessageRawWithFormat(note, []byte(strings.Replace(raw, `"summary":"one"`, `"summary":"two"`, 1)), "twenty_json"))
		requirements.NoError(st.db.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&after))
		assertions.Equal(before, after)
		requirements.NoError(st.UpsertMessageRawWithFormat(note, []byte(strings.Replace(raw, "first@example.org", "second@example.org", 1)), "twenty_json"))
		requirements.NoError(st.db.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&after))
		assertions.Greater(after, before)
	})
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
