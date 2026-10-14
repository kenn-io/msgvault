package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
)

const calendarRaw = `{"id":"event-example","status":"confirmed","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"iCalUID":"event-uid@example.com","description":"Raw description","attendees":[{"email":"PERSON@EXAMPLE.COM","responseStatus":"accepted"}]}`

func calendarFixture(t *testing.T, st *Store, key, raw string) (*MessagePersistData, int64) {
	t.Helper()
	source, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(source.ID, `{"calendar_id":"calendar@example.com","time_zone":"UTC"}`))
	data := &MessagePersistData{
		Message:      &Message{SourceID: source.ID, SourceMessageID: key, MessageType: "calendar_event", Subject: sql.NullString{String: "Example event", Valid: true}},
		Conversation: &ConversationPersistData{SourceConversationID: key, ConversationType: "calendar", Title: "Example event"},
		RawMIME:      []byte(raw), RawFormat: "gcal_json",
		Metadata: &sql.NullString{String: `{"calendar_id":"calendar@example.com"}`, Valid: true},
	}
	id, err := st.PersistMessage(data)
	require.NoError(t, err)
	require.NoError(t, st.db.QueryRow(`SELECT conversation_id FROM messages WHERE id = ?`, id).Scan(&data.Message.ConversationID))
	return data, id
}

func calendarRows(t *testing.T, st *Store, ids ...int64) []CalendarEventRow {
	t.Helper()
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	require.NoError(t, err)
	defer release()
	rows, err := calendarEventRowsByMessageIDs(ctx, st.snapshotReader(ctx), OwnerScope(), ids)
	require.NoError(t, err)
	return rows
}

func rejectCalendarWrites(t *testing.T, st *Store) {
	t.Helper()
	statements := []string{`CREATE TRIGGER reject_calendar_insert BEFORE INSERT ON calendar_events BEGIN SELECT RAISE(ABORT, 'calendar projection blocked'); END`, `CREATE TRIGGER reject_calendar_update BEFORE UPDATE ON calendar_events BEGIN SELECT RAISE(ABORT, 'calendar projection blocked'); END`}
	if st.IsPostgreSQL() {
		statements = []string{`CREATE FUNCTION reject_calendar_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'calendar projection blocked'; END $$`, `CREATE TRIGGER reject_calendar_write BEFORE INSERT OR UPDATE ON calendar_events FOR EACH ROW EXECUTE FUNCTION reject_calendar_write()`}
	}
	for _, statement := range statements {
		_, err := st.db.Exec(statement)
		require.NoError(t, err)
	}
}

func TestCalendarEventProjectionPersistRefreshDelete(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, id := calendarFixture(t, st, "persist", calendarRaw)
	rows := calendarRows(t, st, id)
	requirements.Len(rows, 1)
	assertions.Equal("event-example", rows[0].Event.ProviderEventID)
	assertions.Equal("Raw description", rows[0].Event.Description)
	requirements.Len(rows[0].Event.Attendees, 1)
	assertions.Equal("person@example.com", rows[0].Event.Attendees[0].Email)
	requirements.NoError(st.SetMessageMetadata(id, sql.NullString{String: `{"calendar_id":"calendar@example.com","status":"cancelled"}`, Valid: true}))
	rows = calendarRows(t, st, id)
	requirements.Len(rows, 1)
	assertions.Equal("cancelled", rows[0].Event.Status)
	requirements.NoError(st.UpsertMessageRawWithFormat(id, []byte(strings.ReplaceAll(calendarRaw, "event-example", "changed-example")), "gcal_json"))
	assertions.Equal("changed-example", calendarRows(t, st, id)[0].Event.ProviderEventID)
	data.Message.MessageType = "email"
	_, err := st.UpsertMessage(data.Message)
	requirements.NoError(err)
	assertions.Empty(calendarRows(t, st, id))
	var attendees int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM calendar_event_attendees WHERE message_id = ?`, id).Scan(&attendees))
	assertions.Zero(attendees)
	data.Message.MessageType = "calendar_event"
	_, err = st.UpsertMessage(data.Message)
	requirements.NoError(err)
	requirements.Len(calendarRows(t, st, id), 1)
	requirements.NoError(st.UpsertMessageRawWithFormat(id, []byte(`{}`), "unknown_calendar"))
	assertions.Empty(calendarRows(t, st, id))
}

func TestCalDAVAllDayProjectionPersistsSkippedMidnightCivilDates(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	const calendarID = "skipped-midnight-calendar"
	source, err := st.GetOrCreateSource("caldav", calendarID)
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(source.ID, `{"calendar_id":"skipped-midnight-calendar","time_zone":"America/Sao_Paulo"}`))
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\nUID:skipped-midnight@example.org\r\n" +
		"DTSTART;VALUE=DATE:20181104\r\nDTEND;VALUE=DATE:20181105\r\n" +
		"RRULE:FREQ=DAILY;COUNT=2\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:skipped-midnight@example.org\r\n" +
		"RECURRENCE-ID;VALUE=DATE:20181104\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n")
	sc := calevent.SourceContext{CalendarID: calendarID, CalendarTimeZone: "America/Sao_Paulo"}
	events, err := calevent.ParseCalDAVResource(raw, sc)
	requirements.NoError(err)
	requirements.Len(events, 2)
	for i, item := range events {
		metadata, err := json.Marshal(calevent.CalDAVMetadata{EventID: item.Event.ProviderEventID})
		requirements.NoError(err)
		message := &MessagePersistData{
			Message:      &Message{SourceID: source.ID, SourceMessageID: item.Event.ProviderEventID, MessageType: "calendar_event"},
			Conversation: &ConversationPersistData{SourceConversationID: item.Event.ICalUID, ConversationType: "calendar"},
			RawMIME:      raw,
			RawFormat:    calevent.CalDAVRawFormat,
			Metadata:     &sql.NullString{String: string(metadata), Valid: true},
		}
		messageID, err := st.PersistMessage(message)
		requirements.NoError(err)
		rows := calendarRows(t, st, messageID)
		requirements.Len(rows, 1)
		if i == 0 {
			assertions.Equal("2018-11-04", rows[0].Event.StartDate)
			assertions.Equal("2018-11-05", rows[0].Event.EndDate)
			assertions.Equal(time.Date(2018, time.November, 4, 3, 0, 0, 0, time.UTC), *rows[0].Event.Start)
			assertions.Equal(time.Date(2018, time.November, 5, 2, 0, 0, 0, time.UTC), *rows[0].Event.End)
		} else {
			requirements.NotNil(rows[0].Event.Original)
			assertions.Equal("2018-11-04", rows[0].Event.Original.Date)
		}
	}
}

func TestCalDAVAllDayProjectionPersistsWhollySkippedExclusiveEnd(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	const calendarID = "skipped-exclusive-end-calendar"
	source, err := st.GetOrCreateSource("caldav", calendarID)
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(source.ID, `{"calendar_id":"skipped-exclusive-end-calendar","time_zone":"Pacific/Apia"}`))
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\nUID:skipped-end@example.org\r\n" +
		"DTSTART;VALUE=DATE:20111229\r\nDTEND;VALUE=DATE:20111230\r\n" +
		"SUMMARY:Skipped exclusive date\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	sc := calevent.SourceContext{CalendarID: calendarID, CalendarTimeZone: "Pacific/Apia"}
	events, err := calevent.ParseCalDAVResource(raw, sc)
	requirements.NoError(err)
	requirements.Len(events, 1)
	metadata, err := json.Marshal(calevent.CalDAVMetadata{EventID: events[0].Event.ProviderEventID})
	requirements.NoError(err)
	message := &MessagePersistData{
		Message:      &Message{SourceID: source.ID, SourceMessageID: events[0].Event.ProviderEventID, MessageType: "calendar_event"},
		Conversation: &ConversationPersistData{SourceConversationID: events[0].Event.ICalUID, ConversationType: "calendar"},
		RawMIME:      raw,
		RawFormat:    calevent.CalDAVRawFormat,
		Metadata:     &sql.NullString{String: string(metadata), Valid: true},
	}
	messageID, err := st.PersistMessage(message)
	requirements.NoError(err)
	rows := calendarRows(t, st, messageID)
	requirements.Len(rows, 1)
	assertions.Equal("2011-12-29", rows[0].Event.StartDate)
	assertions.Equal("2011-12-30", rows[0].Event.EndDate)
	assertions.Equal(time.Date(2011, time.December, 29, 10, 0, 0, 0, time.UTC), *rows[0].Event.Start)
	assertions.Equal(time.Date(2011, time.December, 30, 10, 0, 0, 0, time.UTC), *rows[0].Event.End)
}

func TestGoogleAllDayProjectionPersistsSkippedMidnightCivilDates(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	const calendarID = "google-skipped-midnight-calendar"
	source, err := st.GetOrCreateSource("gcal", calendarID)
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(source.ID, `{"calendar_id":"google-skipped-midnight-calendar","time_zone":"America/Sao_Paulo"}`))
	message := &MessagePersistData{
		Message:      &Message{SourceID: source.ID, SourceMessageID: "google-skipped-midnight-event", MessageType: "calendar_event"},
		Conversation: &ConversationPersistData{SourceConversationID: "google-skipped-midnight-event", ConversationType: "calendar"},
		RawMIME:      []byte(`{"id":"google-skipped-midnight-event","start":{"date":"2018-11-04"},"end":{"date":"2018-11-05"}}`),
		RawFormat:    "gcal_json",
		Metadata:     &sql.NullString{String: `{"calendar_id":"google-skipped-midnight-calendar"}`, Valid: true},
	}
	messageID, err := st.PersistMessage(message)
	requirements.NoError(err)
	rows := calendarRows(t, st, messageID)
	requirements.Len(rows, 1)
	assertions.Equal("2018-11-04", rows[0].Event.StartDate)
	assertions.Equal("2018-11-05", rows[0].Event.EndDate)
	assertions.Equal(time.Date(2018, time.November, 4, 3, 0, 0, 0, time.UTC), *rows[0].Event.Start)
	assertions.Equal(time.Date(2018, time.November, 5, 2, 0, 0, 0, time.UTC), *rows[0].Event.End)
}

func TestGoogleAllDayProjectionPersistsWhollySkippedExclusiveEnd(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	const calendarID = "google-skipped-exclusive-end-calendar"
	source, err := st.GetOrCreateSource("gcal", calendarID)
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(source.ID, `{"calendar_id":"google-skipped-exclusive-end-calendar","time_zone":"Pacific/Apia"}`))
	message := &MessagePersistData{
		Message:      &Message{SourceID: source.ID, SourceMessageID: "google-skipped-end-event", MessageType: "calendar_event"},
		Conversation: &ConversationPersistData{SourceConversationID: "google-skipped-end-event", ConversationType: "calendar"},
		RawMIME:      []byte(`{"id":"google-skipped-end-event","start":{"date":"2011-12-29"},"end":{"date":"2011-12-30"}}`),
		RawFormat:    "gcal_json",
		Metadata:     &sql.NullString{String: `{"calendar_id":"google-skipped-exclusive-end-calendar"}`, Valid: true},
	}
	messageID, err := st.PersistMessage(message)
	requirements.NoError(err)
	rows := calendarRows(t, st, messageID)
	requirements.Len(rows, 1)
	assertions.Equal("2011-12-29", rows[0].Event.StartDate)
	assertions.Equal("2011-12-30", rows[0].Event.EndDate)
	assertions.Equal(time.Date(2011, time.December, 29, 10, 0, 0, 0, time.UTC), *rows[0].Event.Start)
	assertions.Equal(time.Date(2011, time.December, 30, 10, 0, 0, 0, time.UTC), *rows[0].Event.End)
}

func TestCalendarRecurrenceEndPreservesTimedCivilDurationAcrossDST(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source, err := st.GetOrCreateSource("caldav", "civil-duration-calendar")
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncConfig(source.ID, `{"calendar_id":"civil-duration-calendar","time_zone":"America/New_York"}`))
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:civil-duration@example.org\r\nDTSTART;TZID=America/New_York:20270313T090000\r\nDURATION:P1D\r\nRRULE:FREQ=WEEKLY;COUNT=2\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	sc := calevent.SourceContext{CalendarID: "civil-duration-calendar", CalendarTimeZone: "America/New_York"}
	events, err := calevent.ParseCalDAVResource(raw, sc)
	require.NoError(err)
	require.Len(events, 1)
	metadata, err := json.Marshal(calevent.CalDAVMetadata{EventID: events[0].Event.ProviderEventID})
	require.NoError(err)
	message := &MessagePersistData{
		Message:      &Message{SourceID: source.ID, SourceMessageID: events[0].Event.ProviderEventID, MessageType: "calendar_event"},
		Conversation: &ConversationPersistData{SourceConversationID: "civil-duration", ConversationType: "calendar"},
		RawMIME:      raw,
		RawFormat:    calevent.CalDAVRawFormat,
		Metadata:     &sql.NullString{String: string(metadata), Valid: true},
	}
	messageID, err := st.PersistMessage(message)
	require.NoError(err)
	rows := calendarRows(t, st, messageID)
	require.Len(rows, 1)
	require.NotNil(rows[0].RecurrenceEndAt)
	assert.Equal(time.Date(2027, 3, 21, 13, 0, 0, 0, time.UTC), *rows[0].RecurrenceEndAt)
	projected := calendarRows(t, st, messageID)
	require.Len(projected, 1)
	assert.Equal(1, projected[0].Event.RecurrenceDurationDays)
}

func TestCalendarRecurrenceDurationMigratesSchema6Projection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source, err := st.GetOrCreateSource("caldav", "schema6-civil-duration-calendar")
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncConfig(source.ID, `{"calendar_id":"schema6-civil-duration-calendar","time_zone":"America/New_York"}`))
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:schema6-civil-duration@example.org\r\nDTSTART;TZID=America/New_York:20270313T090000\r\nDURATION:P1D\r\nRRULE:FREQ=WEEKLY;COUNT=2\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	events, err := calevent.ParseCalDAVResource(raw, calevent.SourceContext{CalendarID: "schema6-civil-duration-calendar", CalendarTimeZone: "America/New_York"})
	require.NoError(err)
	require.Len(events, 1)
	metadata, err := json.Marshal(calevent.CalDAVMetadata{EventID: events[0].Event.ProviderEventID})
	require.NoError(err)
	message := &MessagePersistData{
		Message:      &Message{SourceID: source.ID, SourceMessageID: "schema6-civil-duration", MessageType: "calendar_event"},
		Conversation: &ConversationPersistData{SourceConversationID: "schema6-civil-duration", ConversationType: "calendar"},
		RawMIME:      raw,
		RawFormat:    calevent.CalDAVRawFormat,
		Metadata:     &sql.NullString{String: string(metadata), Valid: true},
	}
	messageID, err := st.PersistMessage(message)
	require.NoError(err)

	_, err = st.DB().Exec(`ALTER TABLE calendar_events DROP COLUMN recurrence_duration_days`)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE calendar_events SET projection_version = 1 WHERE message_id = ?`), messageID)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM applied_migrations WHERE name IN (?, ?)`), migrationCalendarEventRecurrenceDuration, migrationCalendarEventProjectionV2)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE archive_metadata SET value = '6' WHERE key = 'schema_version'`)
	require.NoError(err)

	require.NoError(st.InitSchemaContext(t.Context()))
	projected := calendarRows(t, st, messageID)
	require.Len(projected, 1)
	assert.Equal(1, projected[0].Event.RecurrenceDurationDays)
	version, err := st.SchemaVersionContext(t.Context())
	require.NoError(err)
	assert.Equal(SchemaVersion, version)
}

func TestCalendarEventProjectionSkipsUnchanged(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, id := calendarFixture(t, st, "unchanged", calendarRaw)
	rejectCalendarWrites(t, st)
	requirements.NoError(st.withTxContext(t.Context(), func(tx *loggedTx) error { return st.refreshCalendarEventProjectionWith(t.Context(), tx, id) }))
	// Canonical persistence also skips an identical typed projection.
	_, err := st.PersistMessage(data)
	requirements.NoError(err)
}

func TestCalendarEventProjectionStorageFailureRollsBack(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "rollback", calendarRaw)
	rejectCalendarWrites(t, st)
	err := st.SetMessageMetadata(id, sql.NullString{String: `{"status":"cancelled"}`, Valid: true})
	requirements.ErrorContains(err, "calendar projection blocked")
	metadata, err := st.GetMessageMetadata(id)
	requirements.NoError(err)
	assertions.NotContains(metadata.String, "cancelled")
	assertions.Equal("confirmed", calendarRows(t, st, id)[0].Event.Status)
}

func TestCalendarEventBackfillResumesAfterCancel(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	base := newRFC822IDBackfillBackendStore(t)
	// More than one page, with real raw snapshots and real commits.
	for i := range 103 {
		calendarFixture(t, base, fmt.Sprintf("backfill-%d", i), calendarRaw)
	}
	_, err := base.db.Exec(`DELETE FROM calendar_events`)
	requirements.NoError(err)
	_, err = base.db.Exec(`DELETE FROM applied_migrations WHERE name = 'calendar_event_projection_v1'`)
	requirements.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var pending atomic.Bool
	committed := 0
	gate := &meetingProjectionGate{
		before: func(_ context.Context, query string, _ []driver.NamedValue) error {
			if strings.HasPrefix(query, "INSERT INTO calendar_events") {
				pending.Store(true)
			}
			return nil
		},
		committed: func(_ bool) {
			if pending.Swap(false) {
				committed++
				if committed == 3 {
					cancel()
				}
			}
		},
	}
	st := gatedMeetingProjectionStore(t, base, gate)
	requirements.ErrorIs(st.InitSchemaContext(ctx), context.Canceled)
	var count int
	requirements.NoError(base.db.QueryRow(`SELECT COUNT(*) FROM calendar_events`).Scan(&count))
	assertions.Equal(3, count)
	applied, err := base.IsMigrationAppliedContext(t.Context(), "calendar_event_projection_v1", 1)
	requirements.NoError(err)
	assertions.False(applied)
	gate.committed = nil
	requirements.NoError(st.InitSchemaContext(t.Context()))
	requirements.NoError(base.db.QueryRow(`SELECT COUNT(*) FROM calendar_events`).Scan(&count))
	assertions.Equal(103, count)
	rejectCalendarWrites(t, base)
	requirements.NoError(st.InitSchemaContext(t.Context()))
}

func TestCalendarEventBackfillMalformedRecurrenceDoesNotFailInit(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "bad-rule", `{"id":"bad-series","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"},"recurrence":["RRULE:FREQ=NOT_A_FREQUENCY"]}`)
	_, err := st.db.Exec(`DELETE FROM calendar_events`)
	requirements.NoError(err)
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = 'calendar_event_projection_v1'`)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	rows := calendarRows(t, st, id)
	requirements.Len(rows, 1)
	assertions.NotEmpty(rows[0].RecurrenceError)
	assertions.Nil(rows[0].RecurrenceEndAt)
}

func TestCalendarEventProjectionAndBackfillRejectOutOfRangeWeekdayOrdinal(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"bad-ordinal","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=MONTHLY;BYDAY=-53MO;COUNT=2"]}`
	var id int64
	if !assert.NotPanics(t, func() {
		_, id = calendarFixture(t, st, "bad-ordinal", raw)
	}) {
		return
	}
	rows := calendarRows(t, st, id)
	requirements.Len(rows, 1)
	assertions.NotEmpty(rows[0].RecurrenceError)
	assertions.Nil(rows[0].RecurrenceEndAt)

	_, err := st.db.Exec(`DELETE FROM calendar_events WHERE message_id = ?`, id)
	requirements.NoError(err)
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = 'calendar_event_projection_v1'`)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	rows = calendarRows(t, st, id)
	requirements.Len(rows, 1)
	assertions.NotEmpty(rows[0].RecurrenceError)
	assertions.Nil(rows[0].RecurrenceEndAt)
}

func TestCalendarEventProjectionRejectsUnattainableMonthlySetPosition(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"bad-set-position","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=MONTHLY;BYDAY=MO;BYSETPOS=366;COUNT=1"]}`
	_, id := calendarFixture(t, st, "bad-set-position", raw)
	rows := calendarRows(t, st, id)
	requirements.Len(rows, 1)
	assertions.NotEmpty(rows[0].RecurrenceError)
	assertions.Nil(rows[0].RecurrenceEndAt)
}

func TestCalendarEventUpgradeFromSchema4Fixture(t *testing.T) {
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("schema-4 SQLite fixture")
	}
	assertions, requirements := assert.New(t), require.New(t)
	path := filepath.Join(t.TempDir(), "schema4.db")
	st, err := OpenForTest(path)
	requirements.NoError(err)
	for _, file := range []string{"calendar_schema4.sql", "calendar_schema4_sqlite.sql"} {
		raw, err := os.ReadFile(filepath.Join("testdata", file))
		requirements.NoError(err)
		_, err = st.db.Exec(string(raw))
		requirements.NoError(err)
	}
	_, err = st.db.Exec(`INSERT INTO sources (id, source_type, identifier, sync_config) VALUES (1, 'gcal', 'schema4@example.com', '{"calendar_id":"calendar@example.com","time_zone":"UTC"}')`)
	requirements.NoError(err)
	_, err = st.db.Exec(`INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type) VALUES (1, 1, 'schema4', 'calendar')`)
	requirements.NoError(err)
	_, err = st.db.Exec(`INSERT INTO messages (id, source_id, conversation_id, source_message_id, message_type, metadata) VALUES (1, 1, 1, 'schema4', 'calendar_event', '{"calendar_id":"calendar@example.com"}')`)
	requirements.NoError(err)
	requirements.NoError(upsertMessageRawWithFormat(st.db, 1, []byte(calendarRaw), "gcal_json"))
	_, err = st.db.Exec(`INSERT INTO archive_metadata(key,value) VALUES ('schema_version','4')`)
	requirements.NoError(err)
	requirements.NoError(st.Close())
	st, err = OpenForTest(path)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	rows := calendarRows(t, st, 1)
	requirements.Len(rows, 1)
	assertions.Equal("event-example", rows[0].Event.ProviderEventID)
	rejectCalendarWrites(t, st)
	requirements.NoError(st.Close())
	st, err = OpenForTest(path)
	requirements.NoError(err)
	defer func() { requirements.NoError(st.Close()) }()
	requirements.NoError(st.InitSchemaContext(t.Context()))
	requirements.Len(calendarRows(t, st, 1), 1)
}

func TestCopySubsetRebuildsCalendarEvents(t *testing.T) {
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite subset copy")
	}
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "source.db")
	st, err := OpenForTest(path)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	_, id := calendarFixture(t, st, "subset", calendarRaw)
	// Stale derived data must not be copied or trusted.
	_, err = st.db.Exec(`UPDATE calendar_events SET provider_event_id = 'stale'`)
	requirements.NoError(err)
	requirements.NoError(st.Close())
	dest := filepath.Join(t.TempDir(), "subset")
	_, err = CopySubset(path, dest, 100, false)
	requirements.NoError(err)
	copied, err := OpenForTest(filepath.Join(dest, "msgvault.db"))
	requirements.NoError(err)
	defer func() { requirements.NoError(copied.Close()) }()
	rows := calendarRows(t, copied, id)
	requirements.Len(rows, 1)
	assert.Equal(t, "event-example", rows[0].Event.ProviderEventID)
}

func TestDedupRawCopyRefreshesSurvivor(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, survivor := calendarFixture(t, st, "survivor", calendarRaw)
	_, err := st.db.Exec(`DELETE FROM message_raw WHERE message_id = ?`, survivor)
	requirements.NoError(err)
	_, err = st.db.Exec(`DELETE FROM calendar_events WHERE message_id = ?`, survivor)
	requirements.NoError(err)
	data.Message.SourceMessageID = "duplicate"
	data.Conversation.SourceConversationID = "duplicate"
	data.RawMIME = []byte(strings.ReplaceAll(calendarRaw, "event-example", "copied-example"))
	duplicate, err := st.PersistMessage(data)
	requirements.NoError(err)
	result, err := st.MergeDuplicates(survivor, []int64{duplicate}, "example-batch")
	requirements.NoError(err)
	assertions.Equal(1, result.RawMIMEBackfilled)
	rows := calendarRows(t, st, survivor)
	requirements.Len(rows, 1)
	assertions.Equal("copied-example", rows[0].Event.ProviderEventID)
	assertions.Empty(calendarRows(t, st, duplicate))
}

func TestCalendarEventConsolidationRefreshesSurvivor(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	legacyData, legacy := calendarFixture(t, st, "legacy-source-key", calendarRaw)
	_, survivor := calendarFixture(t, st, "canonical-source-key", calendarRaw)
	_, err := st.db.Exec(`UPDATE calendar_events SET projection_version = 0, status = 'stale' WHERE message_id = ?`, survivor)
	requirements.NoError(err)
	requirements.NoError(st.MigrateSourceMessageID(legacyData.Message.SourceID, legacyData.Message.ConversationID, "legacy-source-key", "canonical-source-key"))
	assertions.Empty(calendarRows(t, st, legacy))
	rows := calendarRows(t, st, survivor)
	requirements.Len(rows, 1)
	assertions.Equal("confirmed", rows[0].Event.Status)
}
