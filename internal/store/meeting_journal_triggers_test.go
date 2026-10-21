package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		{"decision insert", `INSERT INTO meeting_link_decisions(member_message_id,target_ical_uid,decision) VALUES (?, 'meeting@example.test', 'link')`, note},
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
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"meeting@example.test"}}`)
	for _, tc := range []struct {
		name, raw, format string
		id                int64
		want              int
	}{
		{"RSVP only", `{"id":"event","start":{"dateTime":"2026-08-01T10:00:00Z"},"end":{"dateTime":"2026-08-01T11:00:00Z"},"attendees":[{"email":"person@example.test","responseStatus":"accepted"}]}`, "gcal_json", event, 0},
		{"bounds", `{"id":"event","start":{"dateTime":"2026-08-01T10:00:00Z"},"end":{"dateTime":"2026-08-01T12:00:00Z"}}`, "gcal_json", event, 1},
		{"transcript content", `{"schema_version":1,"recording":{"summary":{"markdown":"New notes"}},"calendar_event":{"iCalUid":"meeting@example.test"}}`, "twenty_json", note, 0},
		{"transcript key", `{"schema_version":1,"recording":{"title":"New title"},"calendar_event":{"iCalUid":"other@example.test"}}`, "twenty_json", note, 1},
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

func TestMeetingJournalTriggersAliasRSVP(t *testing.T) {
	for _, sibling := range []bool{false, true} {
		t.Run(fmt.Sprintf("sibling=%t", sibling), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			if st.IsPostgreSQL() {
				t.Skip("SQLite cache journal")
			}
			raw := `{"id":"instance","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"},"attendees":[{"email":"person@example.test","responseStatus":"needsAction"}]}`
			_, losing := calendarFixture(t, st, "instance", raw)
			if sibling {
				_, _ = calendarFixture(t, st, "series|2027-03-14T10:00:00Z", strings.NewReplacer("12:00:00Z", "14:00:00Z", "13:00:00Z", "15:00:00Z").Replace(raw))
			}
			_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T12:00:00Z"}}`)
			before, err := st.MeetingCountKeysContext(t.Context())
			requirements.NoError(err)
			assertions.Equal(!sibling, before[note] != "")
			_, err = st.db.Exec(`DELETE FROM cache_related_change_journal`)
			requirements.NoError(err)
			requirements.NoError(st.UpsertMessageRawWithFormat(losing, []byte(strings.Replace(raw, "needsAction", "accepted", 1)), "gcal_json"))
			var count int
			requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&count))
			want := 0
			if sibling {
				want = 1
			}
			assertions.Equal(want, count)
			after, err := st.MeetingCountKeysContext(t.Context())
			requirements.NoError(err)
			assertions.Equal("uid:weekly@example.org/20270314T100000Z", after[note])
		})
	}
}

func TestMeetingJournalTriggersLegacyCalendarUpgrade(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite cache journal")
	}
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.test"))
	// Older archives have neither this branch's trigger nor the observation column.
	_, err := st.db.Exec(`DROP TRIGGER trg_cache_meeting_events_update`)
	requirements.NoError(err)
	_, err = st.db.Exec(`ALTER TABLE calendar_events DROP COLUMN provider_observation_order`)
	requirements.NoError(err)
	_, err = st.db.Exec(`DROP TABLE calendar_event_observation_clock`)
	requirements.NoError(err)
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationCalendarProviderObservationV1)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE archive_metadata SET value = '12' WHERE key = 'schema_version'`)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	_, err = st.db.Exec(`DELETE FROM cache_related_change_journal`)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE calendar_events SET end_at='2027-03-14T12:00:00Z' WHERE message_id=?`, event)
	requirements.NoError(err)
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&count))
	assert.Equal(t, 1, count)
}
