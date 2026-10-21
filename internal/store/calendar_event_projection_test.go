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
		ICalUID string `json:"iCalUID"`
	}
	// Projection failure fixtures deliberately include malformed provider data.
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		event.ICalUID = ""
	}
	data := &MessagePersistData{
		Message:      &Message{SourceID: source.ID, SourceMessageID: key, MessageType: "calendar_event", Subject: sql.NullString{String: "Example event", Valid: true}},
		Conversation: &ConversationPersistData{SourceConversationID: key, ConversationType: "calendar", Title: "Example event"},
		RawMIME:      []byte(raw), RawFormat: "gcal_json",
		Metadata: &sql.NullString{String: fmt.Sprintf(`{"calendar_id":%q,"ical_uid":%q}`, calendar, event.ICalUID), Valid: true},
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
	var end sql.NullTime
	requirements.NoError(st.db.QueryRow(`SELECT recurrence_end_at FROM calendar_events WHERE message_id = ?`, id).Scan(&end))
	assertions.False(end.Valid)
}

func TestCalendarEventUpgradeRebuildsProjection(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "upgrade", calendarRaw)
	for _, query := range []string{`DROP TABLE calendar_event_attendees`, `DROP TABLE calendar_events`, `DELETE FROM applied_migrations WHERE name = 'calendar_event_projection_v1'`, `UPDATE archive_metadata SET value = '10' WHERE key = 'schema_version'`} {
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
