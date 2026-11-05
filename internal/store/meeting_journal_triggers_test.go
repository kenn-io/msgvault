package store

import (
	"database/sql"
	"fmt"
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
	note := granolaLinkFixture(t, st, "event")
	before, err := st.MeetingCountKeysContext(t.Context())
	requirements.NoError(err)
	requirements.Equal([]string{"uid:meeting@example.test/"}, before[note])
	_, err = st.db.Exec(`DELETE FROM cache_related_change_journal`)
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, `{"calendar_id":"other@example.com","time_zone":"UTC"}`))
	after, err := st.MeetingCountKeysContext(t.Context())
	requirements.NoError(err)
	assertions.Empty(after[note])
	var accountAddress sql.NullString
	requirements.NoError(st.db.QueryRow(`SELECT account_address FROM messages WHERE id=?`, event).Scan(&accountAddress))
	assertions.Empty(accountAddress.String, "neither mailbox is a confirmed account identity")
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset='meeting_links' AND message_id=?`, event).Scan(&count))
	assertions.Positive(count, "a mailbox-only mapping change must invalidate meeting count keys")
}

func TestMeetingJournalTriggers(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite cache journal")
	}
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.test"))
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	exec := func(query string, args ...any) {
		t.Helper()
		_, err := st.db.Exec(query, args...)
		require.NoError(t, err)
	}
	for _, tc := range []struct {
		name, query string
		id          int64
	}{
		{"decision insert", fmt.Sprintf(`INSERT INTO meeting_link_decisions(member_message_id,target_message_id,decision) VALUES (?, %d, 'link')`, event), note},
		{"decision update", `UPDATE meeting_link_decisions SET decision='split' WHERE member_message_id=?`, note},
		{"decision delete", `DELETE FROM meeting_link_decisions WHERE member_message_id=?`, note},
		{"key insert", `INSERT INTO meeting_link_key(message_id,kind,value) VALUES (?, 'ical_uid','meeting@example.test')`, note},
		{"key update", `UPDATE meeting_link_key SET value='other@example.test' WHERE message_id=?`, note},
		{"key delete", `DELETE FROM meeting_link_key WHERE message_id=?`, note},
		{"projection bounds", `UPDATE calendar_events SET end_at='2027-03-14T12:00:00Z' WHERE message_id=?`, event},
		{"UID update", `UPDATE messages SET metadata='{"ical_uid":"other@example.test"}' WHERE id=?`, event},
		{"UID removed", `UPDATE messages SET metadata='{}' WHERE id=?`, event},
		{"projection delete", `DELETE FROM calendar_events WHERE message_id=?`, event},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			exec(`DELETE FROM cache_related_change_journal`)
			exec(tc.query, tc.id)
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
			assertions.Equal([]int64{tc.id}, ids)
		})
	}
	t.Run("projection insert and unchanged sync", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		exec(`DELETE FROM cache_related_change_journal`)
		raw := singleLinkRaw("event", "meeting@example.test")
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
		requirements.NoError(st.SetMessageMetadata(event, sql.NullString{String: `{"unrelated":"changed"}`, Valid: true}))
		requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&count))
		assertions.Zero(count)
	})
}

func TestMeetingJournalIgnoresContentOnlyUpdates(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite cache journal")
	}
	_, event := calendarFixture(t, st, "event", `{"id":"event","start":{"dateTime":"2026-08-01T10:00:00Z"},"end":{"dateTime":"2026-08-01T11:00:00Z"},"attendees":[{"email":"person@example.test","responseStatus":"needsAction"}]}`)
	for _, tc := range []struct {
		name, raw, format string
		id                int64
		want              int
	}{
		{"RSVP only", `{"id":"event","start":{"dateTime":"2026-08-01T10:00:00Z"},"end":{"dateTime":"2026-08-01T11:00:00Z"},"attendees":[{"email":"person@example.test","responseStatus":"accepted"}]}`, "gcal_json", event, 0},
		{"bounds", `{"id":"event","start":{"dateTime":"2026-08-01T10:00:00Z"},"end":{"dateTime":"2026-08-01T12:00:00Z"}}`, "gcal_json", event, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			_, err := st.db.Exec(`DELETE FROM cache_related_change_journal`)
			require.NoError(err)
			require.NoError(st.UpsertMessageRawWithFormat(tc.id, []byte(tc.raw), tc.format))
			var count int
			require.NoError(st.db.QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&count))
			assert.Equal(t, tc.want, count)
		})
	}
}

func TestMeetingJournalIgnoresExceptionCopiesInOtherCalendars(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite cache journal")
	}
	raw := `{"id":"instance","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`
	_, first := calendarFixture(t, st, "instance", raw, "first@example.test")
	_, _ = calendarFixture(t, st, "instance", raw, "second@example.test")
	_, err := st.db.Exec(`DELETE FROM cache_related_change_journal`)
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageRawWithFormat(first, []byte(raw), "gcal_json"))
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&count))
	assert.Zero(t, count, "a copy in another calendar cannot change which exception wins")
}
