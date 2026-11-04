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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
)

const calendarRaw = `{"id":"event-example","status":"confirmed","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"iCalUID":"event-uid@example.com","description":"Raw description","attendees":[{"email":"PERSON@EXAMPLE.COM","responseStatus":"accepted"}]}`

func calendarFixture(t *testing.T, st *Store, key, raw string, calendars ...string) (*MessagePersistData, int64) {
	t.Helper()
	calendar := "calendar@example.com"
	if len(calendars) > 0 {
		calendar = calendars[0]
	}
	source, err := st.GetOrCreateSource("gcal", calendar)
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(source.ID, fmt.Sprintf(`{"calendar_id":%q,"time_zone":"UTC"}`, calendar)))
	var event struct {
		ICalUID  string    `json:"iCalUID"`
		Updated  time.Time `json:"updated"`
		Sequence int       `json:"sequence"`
	}
	// Projection failure fixtures deliberately include malformed provider data.
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		event.ICalUID = ""
	}
	metadata := map[string]any{"calendar_id": calendar, "ical_uid": event.ICalUID}
	if !event.Updated.IsZero() {
		metadata["provider_updated_at_unix_nano"] = event.Updated.UTC().UnixNano()
	}
	if event.Sequence != 0 {
		metadata["sequence"] = event.Sequence
	}
	metadataJSON, err := json.Marshal(metadata, json.Deterministic(true))
	require.NoError(t, err)
	data := &MessagePersistData{
		Message:      &Message{SourceID: source.ID, SourceMessageID: key, MessageType: "calendar_event", Subject: sql.NullString{String: "Example event", Valid: true}},
		Conversation: &ConversationPersistData{SourceConversationID: key, ConversationType: "calendar", Title: "Example event"},
		RawMIME:      []byte(raw), RawFormat: "gcal_json",
		Metadata: &sql.NullString{String: string(metadataJSON), Valid: true},
	}
	id, err := st.PersistMessage(data)
	require.NoError(t, err)
	require.NoError(t, st.db.QueryRow(`SELECT conversation_id FROM messages WHERE id = ?`, id).Scan(&data.Message.ConversationID))
	return data, id
}

func caldavEventFixture(t *testing.T, st *Store, key, raw string, selectedEventIDs ...string) (int64, int64) {
	t.Helper()
	const identifier = "https://calendar.example.org/calendars/synthetic"
	source, err := st.GetOrCreateSource(calevent.CalDAVSourceType, identifier)
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(source.ID, `{"calendar_url":"https://calendar.example.org/calendars/synthetic","time_zone":"UTC"}`))
	components, err := calevent.ParseCalDAVResource(t.Context(), []byte(raw), "UTC")
	require.NoError(t, err)
	var selected calevent.CalDAVEvent
	if len(selectedEventIDs) == 0 {
		require.Len(t, components, 1)
		selected = components[0]
	} else {
		require.Len(t, selectedEventIDs, 1)
		for _, component := range components {
			if component.Event.ProviderEventID == selectedEventIDs[0] {
				selected = component
				break
			}
		}
		require.Equal(t, selectedEventIDs[0], selected.Event.ProviderEventID)
	}
	metadata, err := json.Marshal(calevent.CalDAVMetadata{EventID: selected.Event.ProviderEventID, TimeZone: "UTC"}, json.Deterministic(true))
	require.NoError(t, err)
	data := &MessagePersistData{
		Message:      &Message{SourceID: source.ID, SourceMessageID: key, MessageType: "calendar_event", Subject: sql.NullString{String: selected.Summary, Valid: true}},
		Conversation: &ConversationPersistData{SourceConversationID: key, ConversationType: "calendar", Title: selected.Summary},
		RawMIME:      []byte(raw), RawFormat: calevent.CalDAVRawFormat,
		Metadata: &sql.NullString{String: string(metadata), Valid: true},
	}
	id, err := st.PersistMessage(data)
	require.NoError(t, err)
	return source.ID, id
}

func TestSourceNativeCountsIncludeCalDAVEvents(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	caldavSource, _ := caldavEventFixture(t, st, "native-event", "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:native@example.org\r\nDTSTART:20270302T170000Z\r\nDTEND:20270302T180000Z\r\nSUMMARY:Native event\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	emailSource, err := st.GetOrCreateSource("gmail", "owner@example.test")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(emailSource.ID, "email-thread", "email_thread", "Synthetic")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&Message{
		SourceID: emailSource.ID, SourceMessageID: "email-derived-event",
		ConversationID: conversationID, MessageType: "calendar_event",
	})
	requirements.NoError(err)

	caldavCount, err := st.CountMessagesForSourceContext(t.Context(), caldavSource)
	requirements.NoError(err)
	emailCount, err := st.CountMessagesForSourceContext(t.Context(), emailSource.ID)
	requirements.NoError(err)
	counts, err := st.CountMessagesBySourceContext(t.Context())
	requirements.NoError(err)
	assertions.EqualValues(1, caldavCount)
	assertions.EqualValues(0, emailCount)
	assertions.EqualValues(1, counts[caldavSource].Live)
	assertions.EqualValues(0, counts[emailSource.ID].Live)
}

func TestReconcileSourceMessageSnapshotIncludesNativeCalDAVEvents(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	caldavSource, caldavMessage := caldavEventFixture(t, st, "native-event", "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:native@example.org\r\nDTSTART:20270302T170000Z\r\nDTEND:20270302T180000Z\r\nSUMMARY:Native event\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")

	syncID, err := st.StartSync(caldavSource, "full")
	requirements.NoError(err)
	scoped := st.ScopedToSync(caldavSource, syncID)
	reconciled, err := scoped.ReconcileSourceMessageSnapshot(t.Context(), caldavSource, map[string]struct{}{})
	requirements.NoError(err)
	assertions.EqualValues(1, reconciled)
	var deleted bool
	requirements.NoError(st.db.QueryRowContext(t.Context(), st.Rebind("SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id=?"), caldavMessage).Scan(&deleted))
	assertions.True(deleted)

	emailSource, err := st.GetOrCreateSource("gmail", "owner@example.test")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(emailSource.ID, "email-thread", "email_thread", "Synthetic")
	requirements.NoError(err)
	emailEvent, err := st.UpsertMessage(&Message{
		SourceID: emailSource.ID, SourceMessageID: "email-derived-event",
		ConversationID: conversationID, MessageType: "calendar_event",
	})
	requirements.NoError(err)
	emailSyncID, err := st.StartSync(emailSource.ID, "full")
	requirements.NoError(err)
	emailScoped := st.ScopedToSync(emailSource.ID, emailSyncID)
	reconciled, err = emailScoped.ReconcileSourceMessageSnapshot(t.Context(), emailSource.ID, map[string]struct{}{})
	requirements.NoError(err)
	assertions.Zero(reconciled)
	requirements.NoError(st.db.QueryRowContext(t.Context(), st.Rebind("SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id=?"), emailEvent).Scan(&deleted))
	assertions.False(deleted)
}

func calendarRows(t *testing.T, st *Store, ids ...int64) []CalendarEventRow {
	t.Helper()
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	require.NoError(t, err)
	defer release()
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := queryCalendarEventRowsWithinBudget(ctx, st.meetingReader(ctx), "e.message_id IN ("+placeholders(len(ids))+")", args, len(ids))
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

func TestCalendarEventProjectionSkipsUnchanged(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, id := calendarFixture(t, st, "unchanged", calendarRaw)
	rejectCalendarWrites(t, st)
	requirements.NoError(st.withTxContext(t.Context(), func(tx *loggedTx) error { return st.refreshCalendarEventProjectionWith(t.Context(), tx, id) }))
	// Metadata-only persistence does not represent a new provider observation.
	data.RawMIME = nil
	_, err := st.PersistMessage(data)
	requirements.NoError(err)
}

func TestCalendarProviderObservationOrderAdvancesForRepeatedSnapshotsAndCancellations(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, id := calendarFixture(t, st, "observed", calendarRaw)
	var before int64
	require.NoError(st.db.QueryRow(`SELECT provider_observation_order FROM calendar_events WHERE message_id = ?`, id).Scan(&before))

	// Identical provider snapshots still advance order when archived repeatedly.
	require.NoError(st.UpsertMessageRawWithFormat(id, data.RawMIME, data.RawFormat))
	var afterRaw int64
	require.NoError(st.db.QueryRow(`SELECT provider_observation_order FROM calendar_events WHERE message_id = ?`, id).Scan(&afterRaw))
	assert.Greater(afterRaw, before)

	require.NoError(st.SetCalendarEventProviderMetadata(id, sql.NullString{
		String: `{"calendar_id":"calendar@example.com","ical_uid":"event-uid@example.com","status":"cancelled"}`,
		Valid:  true,
	}))
	var afterCancellation int64
	require.NoError(st.db.QueryRow(`SELECT provider_observation_order FROM calendar_events WHERE message_id = ?`, id).Scan(&afterCancellation))
	assert.Greater(afterCancellation, afterRaw)
}

func TestCalendarProviderObservationOrderMigration(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, older := calendarFixture(t, st, "older", calendarRaw)
	_, newer := calendarFixture(t, st, "newer", strings.ReplaceAll(calendarRaw, "event-example", "newer-event"))

	// Recreate an archive created before the observation-order column and table.
	_, err := st.db.Exec(`ALTER TABLE calendar_events DROP COLUMN provider_observation_order`)
	require.NoError(err)
	_, err = st.db.Exec(`DROP TABLE calendar_event_observation_clock`)
	require.NoError(err)
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationCalendarProviderObservationV1)
	require.NoError(err)
	_, err = st.db.Exec(`UPDATE archive_metadata SET value = '12' WHERE key = 'schema_version'`)
	require.NoError(err)
	setCalendarEventLastModified(t, st, older, time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC))
	setCalendarEventLastModified(t, st, newer, time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC))

	require.NoError(st.InitSchemaContext(t.Context()))
	var olderOrder, newerOrder, clock int64
	require.NoError(st.db.QueryRow(`SELECT provider_observation_order FROM calendar_events WHERE message_id = ?`, older).Scan(&olderOrder))
	require.NoError(st.db.QueryRow(`SELECT provider_observation_order FROM calendar_events WHERE message_id = ?`, newer).Scan(&newerOrder))
	require.NoError(st.db.QueryRow(`SELECT observation_order FROM calendar_event_observation_clock WHERE singleton = 1`).Scan(&clock))
	assert.Greater(newerOrder, olderOrder)
	assert.Equal(newerOrder, clock)
	version, err := st.SchemaVersionContext(t.Context())
	require.NoError(err)
	assert.Equal(SchemaVersion, version)
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
	_, err = base.db.Exec(`DELETE FROM applied_migrations WHERE name = 'calendar_event_projection_v2'`)
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
	applied, err := base.IsMigrationAppliedContext(t.Context(), migrationCalendarEventProjectionV2, calendarEventProjectionVersion)
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
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = 'calendar_event_projection_v2'`)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	rows := calendarRows(t, st, id)
	requirements.Len(rows, 1)
	var end sql.NullTime
	requirements.NoError(st.db.QueryRow(`SELECT recurrence_end_at FROM calendar_events WHERE message_id = ?`, id).Scan(&end))
	assertions.False(end.Valid)
}

func TestCalendarEventUpgradeRebuildsProjection(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "upgrade", calendarRaw)
	for _, query := range []string{`DROP TABLE calendar_event_attendees`, `DROP TABLE calendar_events`, `DELETE FROM applied_migrations WHERE name = 'calendar_event_projection_v2'`, `UPDATE archive_metadata SET value = '10' WHERE key = 'schema_version'`} {
		_, err := st.db.Exec(query)
		requirements.NoError(err)
	}
	requirements.NoError(st.Close())
	reopened, err := OpenForTest(st.dbPath)
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(reopened.Close()) })
	requirements.NoError(reopened.InitSchemaContext(t.Context()))
	rows := calendarRows(t, reopened, id)
	requirements.Len(rows, 1)
	assertions.Equal("event-example", rows[0].Event.ProviderEventID)
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

func TestCopySubsetPreservesCalendarProviderObservationOrder(t *testing.T) {
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite subset copy")
	}
	requirements, assertions := require.New(t), assert.New(t)
	path := filepath.Join(t.TempDir(), "source.db")
	st, err := OpenForTest(path)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	rawA := `{"id":"alias-a","iCalUID":"subset@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-09T09:00:00Z"},"start":{"dateTime":"2027-03-09T10:00:00Z"},"end":{"dateTime":"2027-03-09T11:00:00Z"}}`
	dataA, aliasA := calendarFixture(t, st, "alias-a", rawA)
	dataB, aliasB := calendarFixture(t, st, "alias-b", `{"id":"alias-b","iCalUID":"subset@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-09T09:00:00Z"},"start":{"dateTime":"2027-03-09T12:00:00Z"},"end":{"dateTime":"2027-03-09T13:00:00Z"}}`)
	dataA.RawMIME = []byte(`{"id":"alias-a","iCalUID":"subset@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-09T09:00:00Z"},"start":{"dateTime":"2027-03-09T14:00:00Z"},"end":{"dateTime":"2027-03-09T15:00:00Z"}}`)
	_, err = st.PersistMessage(dataA)
	requirements.NoError(err)
	var sourceOrderA, sourceOrderB int64
	requirements.NoError(st.db.QueryRow(`SELECT provider_observation_order FROM calendar_events WHERE message_id = ?`, aliasA).Scan(&sourceOrderA))
	requirements.NoError(st.db.QueryRow(`SELECT provider_observation_order FROM calendar_events WHERE message_id = ?`, aliasB).Scan(&sourceOrderB))
	assertions.Greater(sourceOrderA, sourceOrderB, "the final A observation must outrank the intervening B snapshot")
	requirements.NoError(st.Close())

	destination := filepath.Join(t.TempDir(), "subset")
	_, err = CopySubset(path, destination, 100, false)
	requirements.NoError(err)
	copied, err := OpenForTest(filepath.Join(destination, "msgvault.db"))
	requirements.NoError(err)
	defer func() { requirements.NoError(copied.Close()) }()
	var copiedOrderA, copiedOrderB, copiedClock int64
	requirements.NoError(copied.db.QueryRow(`SELECT provider_observation_order FROM calendar_events WHERE message_id = ?`, aliasA).Scan(&copiedOrderA))
	requirements.NoError(copied.db.QueryRow(`SELECT provider_observation_order FROM calendar_events WHERE message_id = ?`, aliasB).Scan(&copiedOrderB))
	requirements.NoError(copied.db.QueryRow(`SELECT observation_order FROM calendar_event_observation_clock WHERE singleton = 1`).Scan(&copiedClock))
	assertions.Equal(sourceOrderA, copiedOrderA)
	assertions.Equal(sourceOrderB, copiedOrderB)
	assertions.Equal(max(sourceOrderA, sourceOrderB), copiedClock)

	requirements.NoError(copied.UpsertMessageRawWithFormat(aliasB, dataB.RawMIME, dataB.RawFormat))
	var nextOrderB int64
	requirements.NoError(copied.db.QueryRow(`SELECT provider_observation_order FROM calendar_events WHERE message_id = ?`, aliasB).Scan(&nextOrderB))
	assertions.Greater(nextOrderB, max(sourceOrderA, sourceOrderB), "new observations in the subset must outrank every copied snapshot")
}

func TestCalDAVSubsetOmitsEventsKeepsGoogle(t *testing.T) {
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite subset copy")
	}
	assertions, requirements := assert.New(t), require.New(t)
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	st, err := OpenForTest(sourcePath)
	requirements.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	requirements.NoError(st.InitSchemaContext(t.Context()))
	source, err := st.GetOrCreateSource("caldav", "calendar@example.org")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "shared-resource", "calendar", "Shared resource")
	requirements.NoError(err)
	const resource = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\nUID:event-a@example.org\r\nDTSTART:20270314T130000Z\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:event-b@example.org\r\nDTSTART:20270321T130000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	for _, eventID := range []string{"event-a@example.org", "event-b@example.org"} {
		id, err := st.UpsertMessage(&Message{ConversationID: conversationID, SourceID: source.ID, SourceMessageID: eventID, MessageType: "calendar_event"})
		requirements.NoError(err)
		requirements.NoError(st.UpsertMessageRawWithFormat(id, []byte(resource), "caldav_ics"))
		requirements.Len(calendarRows(t, st, id), 1)
	}
	_, googleID := calendarFixture(t, st, "google", calendarRaw)
	requirements.NoError(st.Close())
	destination := filepath.Join(t.TempDir(), "subset")
	result, err := CopySubsetWithOptions(sourcePath, destination, 10, CopySubsetOptions{})
	requirements.NoError(err)
	assertions.Equal(int64(1), result.Messages)
	copied, err := OpenForTest(filepath.Join(destination, "msgvault.db"))
	requirements.NoError(err)
	t.Cleanup(func() { _ = copied.Close() })
	rows, err := copied.db.Query(`SELECT m.id, r.raw_format FROM messages m JOIN message_raw r ON r.message_id = m.id`)
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()
	var ids []int64
	var formats []string
	for rows.Next() {
		var id int64
		var format string
		requirements.NoError(rows.Scan(&id, &format))
		ids = append(ids, id)
		formats = append(formats, format)
	}
	requirements.NoError(rows.Err())
	assertions.Equal([]int64{googleID}, ids)
	assertions.Equal([]string{"gcal_json"}, formats)
	requirements.NoError(rows.Close())
	var calDAVMessages, calDAVProjections, calDAVSources int
	requirements.NoError(copied.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE source_id = ?`, source.ID).Scan(&calDAVMessages))
	requirements.NoError(copied.db.QueryRow(`SELECT COUNT(*) FROM calendar_events WHERE message_id <> ?`, googleID).Scan(&calDAVProjections))
	requirements.NoError(copied.db.QueryRow(`SELECT COUNT(*) FROM sources WHERE source_type = 'caldav'`).Scan(&calDAVSources))
	assertions.Zero(calDAVMessages)
	assertions.Zero(calDAVProjections)
	assertions.Zero(calDAVSources)
}

func TestCalendarSourceZoneRefreshKeepsConcurrentRawUpdate(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	dbURL := skipUnlessPostgresInternal(t)
	base := newPGStoreInternal(t, dbURL)
	data, id := calendarFixture(t, base, "race", calendarRaw)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	held, err := base.db.BeginTx(ctx, nil)
	requirements.NoError(err)
	defer func() { _ = held.Rollback() }()
	requirements.NoError(base.lockMeetingEvidenceWith(ctx, held, id))
	requirements.NoError(upsertMessageRawWithFormat(boundQuerier{ctx: ctx, q: held}, id, []byte(strings.ReplaceAll(calendarRaw, "event-example", "new-example")), "gcal_json"))
	requirements.NoError(base.refreshCalendarEventProjectionWith(ctx, held, id))
	reached := make(chan struct{}, 1)
	var once sync.Once
	gate := &meetingProjectionGate{before: func(_ context.Context, query string, _ []driver.NamedValue) error {
		// With the evidence lock the refresh waits before reading raw. Without it,
		// it reaches the projection write with the old committed raw snapshot.
		if strings.Contains(query, "FROM messages WHERE id =") && strings.Contains(query, "FOR UPDATE") || strings.HasPrefix(query, "INSERT INTO calendar_events") {
			once.Do(func() { reached <- struct{}{} })
		}
		return nil
	}}
	st := gatedMeetingProjectionStore(t, base, gate)
	done := make(chan error, 1)
	go func() { done <- st.UpdateSourceSyncConfig(data.Message.SourceID, `{"time_zone":"Asia/Tokyo"}`) }()
	select {
	case <-reached:
	case <-ctx.Done():
		requirements.NoError(ctx.Err(), "zone refresh did not reach the message")
	}
	requirements.NoError(held.Commit())
	select {
	case err := <-done:
		requirements.NoError(err)
	case <-ctx.Done():
		requirements.NoError(ctx.Err(), "zone refresh did not finish")
	}
	row := calendarRows(t, base, id)[0]
	assertions.Equal("new-example", row.Event.ProviderEventID)
	assertions.Equal("Asia/Tokyo", row.Event.CalendarTimeZone)
}
