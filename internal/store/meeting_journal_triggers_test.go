package store

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingJournalCalendarMailboxChange(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite cache journal")
	}
	data, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.test"))
	_, err := st.db.Exec(`DELETE FROM cache_related_change_journal`)
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, `{"calendar_id":"other@example.com","time_zone":"UTC"}`))
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset='meeting_links' AND message_id=?`, event).Scan(&count))
	assertions.Positive(count, "a mailbox-only mapping change must invalidate meeting count keys")
}

func TestMeetingJournalTriggers(t *testing.T) {
	for _, tc := range []struct {
		name, setup, query, raw string
		member, quiet           bool
	}{
		{"decision insert", "", `INSERT INTO meeting_link_decisions(member_message_id,target_message_id,decision) VALUES ($note, $event, 'link')`, "", true, false},
		{"decision update", `INSERT INTO meeting_link_decisions(member_message_id,target_message_id,decision) VALUES ($note, $event, 'link')`, `UPDATE meeting_link_decisions SET decision='split' WHERE member_message_id=$note`, "", true, false},
		{"decision delete", `INSERT INTO meeting_link_decisions(member_message_id,target_message_id,decision) VALUES ($note, $event, 'link')`, `DELETE FROM meeting_link_decisions WHERE member_message_id=$note`, "", true, false},
		{"key insert", "", `INSERT INTO meeting_link_key(message_id,kind,value) VALUES ($note, 'ical_uid','meeting@example.test')`, "", true, false},
		{"key update", `INSERT INTO meeting_link_key(message_id,kind,value) VALUES ($note, 'ical_uid','meeting@example.test')`, `UPDATE meeting_link_key SET value='other@example.test' WHERE message_id=$note`, "", true, false},
		{"key delete", `INSERT INTO meeting_link_key(message_id,kind,value) VALUES ($note, 'ical_uid','meeting@example.test')`, `DELETE FROM meeting_link_key WHERE message_id=$note`, "", true, false},
		{"projection bounds", "", `UPDATE calendar_events SET end_at='2027-03-14T12:00:00Z' WHERE message_id=$event`, "", false, false},
		{"UID update", "", `UPDATE messages SET metadata='{"ical_uid":"other@example.test"}' WHERE id=$event`, "", false, false},
		{"UID removed", "", `UPDATE messages SET metadata='{}' WHERE id=$event`, "", false, false},
		{"projection delete", "", `DELETE FROM calendar_events WHERE message_id=$event`, "", false, false},
		{"import RSVP only", "", "", `{"id":"event","start":{"dateTime":"2026-08-01T10:00:00Z"},"end":{"dateTime":"2026-08-01T11:00:00Z"},"attendees":[{"email":"person@example.test","responseStatus":"accepted"}]}`, false, true},
		{"import bounds", "", "", `{"id":"event","start":{"dateTime":"2026-08-01T10:00:00Z"},"end":{"dateTime":"2026-08-01T12:00:00Z"}}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			if st.IsPostgreSQL() {
				t.Skip("SQLite cache journal")
			}
			raw := singleLinkRaw("event", "meeting@example.test")
			if tc.raw != "" {
				raw = `{"id":"event","start":{"dateTime":"2026-08-01T10:00:00Z"},"end":{"dateTime":"2026-08-01T11:00:00Z"},"attendees":[{"email":"person@example.test","responseStatus":"needsAction"}]}`
			}
			_, event := calendarFixture(t, st, "event", raw)
			_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
			exec := func(query string) {
				t.Helper()
				query = strings.NewReplacer("$event", strconv.FormatInt(event, 10), "$note", strconv.FormatInt(note, 10)).Replace(query)
				_, err := st.db.Exec(query)
				requirements.NoError(err)
			}
			if tc.setup != "" {
				exec(tc.setup)
			}
			exec(`DELETE FROM cache_related_change_journal`)
			if tc.raw != "" {
				requirements.NoError(st.UpsertMessageRawWithFormat(event, []byte(tc.raw), "gcal_json"))
			} else {
				exec(tc.query)
			}
			var ids []int64
			rows, err := st.db.Query(`SELECT message_id FROM cache_related_change_journal WHERE dataset='meeting_links'`)
			requirements.NoError(err)
			defer func() { requirements.NoError(rows.Close()) }()
			for rows.Next() {
				var id int64
				requirements.NoError(rows.Scan(&id))
				ids = append(ids, id)
			}
			requirements.NoError(rows.Err())
			id := event
			if tc.member {
				id = note
			}
			if tc.quiet {
				assertions.Empty(ids)
			} else {
				assertions.Equal([]int64{id}, ids)
			}
		})
	}
	t.Run("projection insert and unchanged sync", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		if st.IsPostgreSQL() {
			t.Skip("SQLite cache journal")
		}
		raw := singleLinkRaw("event", "meeting@example.test")
		_, event := calendarFixture(t, st, "event", raw)
		exec := func(query string, args ...any) {
			t.Helper()
			_, err := st.db.Exec(query, args...)
			requirements.NoError(err)
		}
		exec(`DELETE FROM calendar_events WHERE message_id=?`, event)
		exec(`DELETE FROM cache_related_change_journal`)
		requirements.NoError(st.UpsertMessageRawWithFormat(event, []byte(raw), "gcal_json"))
		var count int
		requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset='meeting_links' AND message_id=?`, event).Scan(&count))
		assertions.Equal(1, count)
		noteRaw := `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"meeting@example.test"}}`
		_, syncNote := projectionFixture(t, st, "sync-note", "twenty_json", noteRaw)
		exec(`DELETE FROM cache_related_change_journal`)
		requirements.NoError(st.UpsertMessageRawWithFormat(event, []byte(raw), "gcal_json"))
		requirements.NoError(st.UpsertMessageRawWithFormat(syncNote, []byte(noteRaw), "twenty_json"))
		exec(`UPDATE calendar_events SET content_hash=content_hash WHERE message_id=?`, event)
		requirements.NoError(st.SetMessageMetadata(event, sql.NullString{String: `{"ical_uid":"meeting@example.test","unrelated":"changed"}`, Valid: true}))
		requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&count))
		assertions.Zero(count)
	})
}
