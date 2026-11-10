package store

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingJournalTriggers(t *testing.T) {
	const uidNote = `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"meeting@example.test","startsAt":"2027-03-14T10:00:00Z"}}`
	const googleNote = `{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":"event","scheduled_start_time":"2027-03-14T10:00:00Z"}}`
	for _, tc := range []struct {
		name, format, note, decision, query, raw, noteUpdate, syncConfig string
		member, quiet                                                    bool
	}{
		{name: "calendar mailbox change", syncConfig: `{"calendar_id":"other@example.com","time_zone":"UTC"}`, quiet: true},
		{name: "decision insert", query: `INSERT INTO meeting_link_decisions(member_message_id,target_message_id,decision) VALUES ($note, $event, 'link')`, member: true},
		{name: "decision update", decision: "link", query: `UPDATE meeting_link_decisions SET decision='split' WHERE member_message_id=$note`, member: true},
		{name: "decision delete", decision: "link", query: `DELETE FROM meeting_link_decisions WHERE member_message_id=$note`, member: true},
		{name: "key insert", noteUpdate: uidNote, member: true},
		{name: "key update", note: uidNote, query: `UPDATE meeting_link_key SET value='other@example.test' WHERE message_id=$note`, member: true},
		{name: "key delete", note: uidNote, query: `DELETE FROM meeting_link_key WHERE message_id=$note`, member: true},
		{name: "projection bounds", note: uidNote, query: `UPDATE calendar_events SET end_at='2027-03-14T12:00:00Z' WHERE message_id=$event`},
		{name: "Google event bounds", format: "granola_json", note: googleNote, query: `UPDATE calendar_events SET end_at='2027-03-14T12:00:00Z' WHERE message_id=$event`},
		{name: "Google event UID update", format: "granola_json", note: googleNote, query: `UPDATE messages SET metadata='{"ical_uid":"other@example.test"}' WHERE id=$event`},
		{name: "Google instance bounds", format: "granola_json", note: strings.Replace(googleNote, `"event"`, `"event_20270314T100000Z"`, 1), query: `UPDATE calendar_events SET end_at='2027-03-14T12:00:00Z' WHERE message_id=$event`},
		{name: "UID update", note: uidNote, query: `UPDATE messages SET metadata='{"ical_uid":"other@example.test"}' WHERE id=$event`},
		{name: "UID becomes referenced", note: strings.Replace(uidNote, "meeting@example.test", "other@example.test", 1), query: `UPDATE messages SET metadata='{"ical_uid":"other@example.test"}' WHERE id=$event`},
		{name: "UID removed", note: uidNote, query: `UPDATE messages SET metadata='{}' WHERE id=$event`},
		{name: "saved split UID update", decision: "split", query: `UPDATE messages SET metadata='{"ical_uid":"other@example.test"}' WHERE id=$event`},
		{name: "projection delete", note: uidNote, query: `DELETE FROM calendar_events WHERE message_id=$event`},
		{name: "unrelated projection bounds", note: strings.Replace(uidNote, "meeting@example.test", "unrelated@example.test", 1), query: `UPDATE calendar_events SET end_at='2027-03-14T12:00:00Z' WHERE message_id=$event`, quiet: true},
		{name: "unrelated UID update", format: "granola_json", note: strings.Replace(googleNote, `"event"`, `"unrelated"`, 1), query: `UPDATE messages SET metadata='{"ical_uid":"other@example.test"}' WHERE id=$event`, quiet: true},
		{name: "unrelated projection delete", note: strings.Replace(uidNote, "meeting@example.test", "unrelated@example.test", 1), query: `DELETE FROM calendar_events WHERE message_id=$event`, quiet: true},
		{name: "import RSVP only", decision: "link", raw: `{"id":"event","start":{"dateTime":"2026-08-01T10:00:00Z"},"end":{"dateTime":"2026-08-01T11:00:00Z"},"attendees":[{"email":"person@example.test","responseStatus":"accepted"}]}`, quiet: true},
		{name: "import bounds", decision: "link", raw: `{"id":"event","start":{"dateTime":"2026-08-01T10:00:00Z"},"end":{"dateTime":"2026-08-01T12:00:00Z"}}`},
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
			data, event := calendarFixture(t, st, "event", raw)
			format, noteRaw := tc.format, tc.note
			if format == "" {
				format = "twenty_json"
			}
			if noteRaw == "" {
				noteRaw = `{"schema_version":1,"recording":{}}`
			}
			_, note := projectionFixture(t, st, "note", format, noteRaw)
			exec := func(query string) {
				t.Helper()
				query = strings.NewReplacer("$event", strconv.FormatInt(event, 10), "$note", strconv.FormatInt(note, 10)).Replace(query)
				_, err := st.db.Exec(query)
				requirements.NoError(err)
			}
			switch tc.decision {
			case "link":
				requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
			case "split":
				requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
			}
			exec(`DELETE FROM cache_related_change_journal`)
			if tc.syncConfig != "" {
				requirements.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, tc.syncConfig))
			} else if tc.raw != "" {
				requirements.NoError(st.UpsertMessageRawWithFormat(event, []byte(tc.raw), "gcal_json"))
			} else if tc.noteUpdate != "" {
				requirements.NoError(st.UpsertMessageRawWithFormat(note, []byte(tc.noteUpdate), format))
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
	for _, saved := range []bool{false, true} {
		name := "UID key on exception"
		if saved {
			name = "saved link to series"
		}
		t.Run(name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			if st.IsPostgreSQL() {
				t.Skip("SQLite cache journal")
			}
			_, master := calendarFixture(t, st, "master", strings.ReplaceAll(linkSeriesRaw, `"iCalUID":"weekly@example.org",`, ""))
			raw := `{"id":"exception","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
			_, exception := calendarFixture(t, st, "exception", raw)
			event := master
			if saved {
				_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
				requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}))
				event = exception
			} else {
				projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
			}
			_, err := st.db.Exec(`DELETE FROM cache_related_change_journal`)
			requirements.NoError(err)
			_, err = st.db.Exec(`UPDATE calendar_events SET end_at='2027-03-14T12:00:00Z' WHERE message_id=?`, event)
			requirements.NoError(err)
			var count int
			requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset='meeting_links' AND message_id=?`, event).Scan(&count))
			assertions.Equal(1, count)
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
		_, other := calendarFixture(t, st, "other", singleLinkRaw("other", "unrelated@example.test"))
		_, otherNote := projectionFixture(t, st, "other-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"unrelated@example.test"}}`)
		requirements.NoError(st.LinkMeetingContext(t.Context(), otherNote, OccurrenceRef{MessageID: other}))
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
		assertions.Zero(count, "an unrelated calendar insert must not refresh transcript keys")
		noteRaw := `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"meeting@example.test"}}`
		_, syncNote := projectionFixture(t, st, "sync-note", "twenty_json", noteRaw)
		exec(`DELETE FROM calendar_events WHERE message_id=?`, event)
		exec(`DELETE FROM cache_related_change_journal`)
		requirements.NoError(st.UpsertMessageRawWithFormat(event, []byte(raw), "gcal_json"))
		requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset='meeting_links' AND message_id=?`, event).Scan(&count))
		assertions.Equal(1, count, "an insert matching an existing transcript must refresh keys")
		exec(`DELETE FROM cache_related_change_journal`)
		requirements.NoError(st.UpsertMessageRawWithFormat(event, []byte(raw), "gcal_json"))
		requirements.NoError(st.UpsertMessageRawWithFormat(syncNote, []byte(noteRaw), "twenty_json"))
		exec(`UPDATE calendar_events SET content_hash=content_hash WHERE message_id=?`, event)
		requirements.NoError(st.SetMessageMetadata(event, sql.NullString{String: `{"ical_uid":"meeting@example.test","unrelated":"changed"}`, Valid: true}))
		requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&count))
		assertions.Zero(count)
	})
}
