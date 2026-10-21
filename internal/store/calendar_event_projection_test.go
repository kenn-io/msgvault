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
	return calendarFixtureWithSubject(t, st, key, raw, "Example event", calendars...)
}

func calendarFixtureWithSubject(t *testing.T, st *Store, key, raw, subject string, calendars ...string) (*MessagePersistData, int64) {
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
		Message:      &Message{SourceID: source.ID, SourceMessageID: key, MessageType: "calendar_event", Subject: sql.NullString{String: subject, Valid: true}},
		Conversation: &ConversationPersistData{SourceConversationID: key, ConversationType: "calendar", Title: subject},
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

func TestCalendarEventProviderFreshnessAdvancesForUnprojectedChanges(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "unprojected-change", calendarRaw)
	older := time.Now().UTC().Add(-time.Minute)
	_, err := st.db.Exec(`UPDATE calendar_events SET snapshot_updated_at = ? WHERE message_id = ?`, older, id)
	requirements.NoError(err)
	var oldHash string
	requirements.NoError(st.db.QueryRow(`SELECT content_hash FROM calendar_events WHERE message_id = ?`, id).Scan(&oldHash))

	// Google revision and summary changes are provider evidence even though the
	// normalized occurrence projection does not include either field.
	updatedRaw := strings.Replace(calendarRaw, `"description":"Raw description"`, `"description":"Raw description","summary":"Updated summary","updated":"2027-03-02T18:00:00Z"`, 1)
	requirements.NoError(st.UpsertMessageRawWithFormat(id, []byte(updatedRaw), "gcal_json"))
	var fresh time.Time
	var newHash string
	requirements.NoError(st.db.QueryRow(`SELECT content_hash, snapshot_updated_at FROM calendar_events WHERE message_id = ?`, id).Scan(&newHash, &fresh))
	assertions.Equal(oldHash, newHash, "provider-only changes leave normalized projection content unchanged")
	assertions.True(fresh.After(older), "provider-only changes must advance snapshot freshness")
}

func TestCalendarEventProjectionRebuildPreservesSnapshotFreshness(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "rebuild-freshness", calendarRaw)
	older := time.Now().UTC().Add(-time.Minute)
	_, err := st.db.Exec(`UPDATE calendar_events SET snapshot_updated_at = ? WHERE message_id = ?`, older, id)
	requirements.NoError(err)
	var before time.Time
	requirements.NoError(st.db.QueryRow(`SELECT snapshot_updated_at FROM calendar_events WHERE message_id = ?`, id).Scan(&before))
	// A projection version bump causes the normal migration reader to rebuild
	// the row from raw evidence. Reconstruction must retain its provider time.
	_, err = st.db.Exec(`UPDATE calendar_events SET projection_version = ? WHERE message_id = ?`, calendarEventProjectionVersion-1, id)
	requirements.NoError(err)
	requirements.NoError(st.backfillCalendarEventProjectionsContext(t.Context()))
	var after time.Time
	requirements.NoError(st.db.QueryRow(`SELECT snapshot_updated_at FROM calendar_events WHERE message_id = ?`, id).Scan(&after))
	assertions.True(after.Equal(before), "projection rebuilds must not replace provider freshness")
}

func TestSparseCancellationUsesFreshestExceptionAliasDetails(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	firstRaw := `{"id":"instance","iCalUID":"orphan@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T06:00:00","timeZone":"America/New_York"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"},"summary":"Stale summary","attendees":[{"email":"stale@example.org"}]}`
	secondRaw := `{"id":"instance","iCalUID":"orphan@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T06:00:00-04:00","timeZone":"America/New_York"},"start":{"dateTime":"2027-03-14T14:00:00Z"},"end":{"dateTime":"2027-03-14T15:00:00Z"},"summary":"Fresh summary","attendees":[{"email":"fresh@example.org"}]}`
	firstData, first := calendarFixture(t, st, "instance", firstRaw)
	_, second := calendarFixture(t, st, "series|2027-03-14T10:00:00Z", secondRaw)
	older := time.Now().UTC().Add(-2 * time.Minute)
	_, err := st.db.Exec(`UPDATE calendar_events SET snapshot_updated_at = ? WHERE message_id = ?`, older, first)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE calendar_events SET snapshot_updated_at = ? WHERE message_id = ?`, older.Add(time.Minute), second)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE messages SET subject = ? WHERE id = ?`, "Stale summary", first)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE messages SET subject = ? WHERE id = ?`, "Fresh summary", second)
	requirements.NoError(err)
	_, note := projectionFixture(t, st, "cancellation-note", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: second}))

	metadata, err := st.GetMessageMetadata(first)
	requirements.NoError(err)
	var fields map[string]any
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &fields))
	fields["status"] = "cancelled"
	encoded, err := json.Marshal(fields, json.Deterministic(true))
	requirements.NoError(err)
	requirements.NoError(st.SetMessageMetadata(first, sql.NullString{String: string(encoded), Valid: true}))

	rows := calendarRows(t, st, first)
	requirements.Len(rows, 1)
	assertions.Equal("cancelled", rows[0].Event.Status)
	requirements.NotNil(rows[0].Event.Start)
	requirements.NotNil(rows[0].Event.End)
	assertions.Equal(time.Date(2027, 3, 14, 14, 0, 0, 0, time.UTC), *rows[0].Event.Start)
	assertions.Equal(time.Date(2027, 3, 14, 15, 0, 0, 0, time.UTC), *rows[0].Event.End)
	requirements.Len(rows[0].Event.Attendees, 1)
	assertions.Equal("fresh@example.org", rows[0].Event.Attendees[0].Email)

	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(first, page.Occurrences[0].MessageID)
	assertions.Equal("Fresh summary", page.Occurrences[0].Subject)
	assertions.Equal(from.Add(14*time.Hour), page.Occurrences[0].Start)
	assertions.Equal("cancelled", page.Occurrences[0].Status)

	links, err := st.MeetingLinksContext(t.Context(), note)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", first), links[0].Ref)
	assertions.Equal(from.Add(14*time.Hour), links[0].Start)
	assertions.Equal(from.Add(15*time.Hour), links[0].End)

	// Reconstructing the cancellation from its complete but older raw payload
	// must retain the selected newer alias snapshot.
	requirements.NoError(st.UpdateSourceSyncConfig(firstData.Message.SourceID, `{"time_zone":"America/Los_Angeles"}`))
	assertFreshCancellationAliasDetails(t, st, first, from.Add(14*time.Hour), from.Add(15*time.Hour), "fresh@example.org")
	_, err = st.db.Exec(`UPDATE calendar_events SET projection_version = ? WHERE message_id = ?`, calendarEventProjectionVersion-1, first)
	requirements.NoError(err)
	requirements.NoError(st.backfillCalendarEventProjectionsContext(t.Context()))
	assertFreshCancellationAliasDetails(t, st, first, from.Add(14*time.Hour), from.Add(15*time.Hour), "fresh@example.org")
}

func assertFreshCancellationAliasDetails(t *testing.T, st *Store, messageID int64, start, end time.Time, attendee string) {
	t.Helper()
	assertions, requirements := assert.New(t), require.New(t)
	rows := calendarRows(t, st, messageID)
	requirements.Len(rows, 1)
	requirements.NotNil(rows[0].Event.Start)
	requirements.NotNil(rows[0].Event.End)
	assertions.Equal(start, *rows[0].Event.Start)
	assertions.Equal(end, *rows[0].Event.End)
	requirements.Len(rows[0].Event.Attendees, 1)
	assertions.Equal(attendee, rows[0].Event.Attendees[0].Email)
}

func TestCopySubsetPreservesInheritedCancellationSnapshot(t *testing.T) {
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite subset copy")
	}
	assertions, requirements := assert.New(t), require.New(t)
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	st, err := OpenForTest(sourcePath)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	masterRaw := `{"id":"series","iCalUID":"orphan@example.org","start":{"dateTime":"2027-03-07T10:00:00Z"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`
	calendarFixture(t, st, "series", masterRaw)
	firstRaw := `{"id":"instance","iCalUID":"orphan@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"},"summary":"Old snapshot","attendees":[{"email":"old@example.org"}]}`
	secondRaw := strings.Replace(firstRaw, `"2027-03-14T12:00:00Z"`, `"2027-03-14T14:00:00Z"`, 1)
	secondRaw = strings.Replace(secondRaw, `"2027-03-14T13:00:00Z"`, `"2027-03-14T15:00:00Z"`, 1)
	secondRaw = strings.Replace(secondRaw, "Old snapshot", "Fresh snapshot", 1)
	secondRaw = strings.Replace(secondRaw, "old@example.org", "fresh@example.org", 1)
	_, first := calendarFixture(t, st, "instance", firstRaw)
	_, second := calendarFixture(t, st, "series|2027-03-14T10:00:00Z", secondRaw)
	older := time.Now().UTC().Add(-2 * time.Minute)
	_, err = st.db.Exec(`UPDATE calendar_events SET snapshot_updated_at = ? WHERE message_id = ?`, older, first)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE calendar_events SET snapshot_updated_at = ? WHERE message_id = ?`, older.Add(time.Minute), second)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE messages SET subject = ? WHERE id = ?`, "Old snapshot", first)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE messages SET subject = ? WHERE id = ?`, "Fresh snapshot", second)
	requirements.NoError(err)
	metadata, err := st.GetMessageMetadata(first)
	requirements.NoError(err)
	var fields map[string]any
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &fields))
	fields["status"] = "cancelled"
	encoded, err := json.Marshal(fields, json.Deterministic(true))
	requirements.NoError(err)
	requirements.NoError(st.SetMessageMetadata(first, sql.NullString{String: string(encoded), Valid: true}))
	requirements.NoError(st.Close())

	destination := filepath.Join(t.TempDir(), "subset")
	_, err = CopySubset(sourcePath, destination, 100, false)
	requirements.NoError(err)
	copied, err := OpenForTest(filepath.Join(destination, "msgvault.db"))
	requirements.NoError(err)
	defer func() { requirements.NoError(copied.Close()) }()
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := copied.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(first, page.Occurrences[0].MessageID)
	assertions.Equal(from.Add(14*time.Hour), page.Occurrences[0].Start)
	assertions.Equal("Fresh snapshot", page.Occurrences[0].Subject)
	requirements.Len(page.Occurrences[0].Attendees, 1)
	assertions.Equal("fresh@example.org", page.Occurrences[0].Attendees[0].Email)
}

func TestCancellationUsesExplicitlyClearedFreshAliasTitle(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	firstRaw := `{"id":"instance","iCalUID":"orphan@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"},"summary":"Old title"}`
	secondRaw := strings.Replace(firstRaw, `"2027-03-14T12:00:00Z"`, `"2027-03-14T14:00:00Z"`, 1)
	secondRaw = strings.Replace(secondRaw, `"2027-03-14T13:00:00Z"`, `"2027-03-14T15:00:00Z"`, 1)
	secondRaw = strings.Replace(secondRaw, `"Old title"`, `""`, 1)
	_, first := calendarFixture(t, st, "instance", firstRaw)
	_, second := calendarFixture(t, st, "series|2027-03-14T10:00:00Z", secondRaw)
	older := time.Now().UTC().Add(-2 * time.Minute)
	_, err := st.db.Exec(`UPDATE calendar_events SET snapshot_updated_at = ? WHERE message_id = ?`, older, first)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE calendar_events SET snapshot_updated_at = ? WHERE message_id = ?`, older.Add(time.Minute), second)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE messages SET subject = ? WHERE id = ?`, "Old title", first)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE messages SET subject = ? WHERE id = ?`, "", second)
	requirements.NoError(err)
	metadata, err := st.GetMessageMetadata(first)
	requirements.NoError(err)
	var fields map[string]any
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &fields))
	fields["status"] = "cancelled"
	encoded, err := json.Marshal(fields, json.Deterministic(true))
	requirements.NoError(err)
	requirements.NoError(st.SetMessageMetadata(first, sql.NullString{String: string(encoded), Valid: true}))
	var subject string
	requirements.NoError(st.db.QueryRow(`SELECT subject FROM messages WHERE id = ?`, first).Scan(&subject))
	assertions.Empty(subject, "an explicit title clear on the selected alias must replace the stale cancellation title")
}

func TestListingKeepsInheritedCancellationDetailsWhenRawAliasIsSparse(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	masterRaw := `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T10:00:00Z"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"],"summary":"Master title","attendees":[{"email":"master@example.org"}]}`
	editedRaw := `{"id":"instance","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T14:00:00Z"},"end":{"dateTime":"2027-03-14T15:00:00Z"},"summary":"Edited title","attendees":[{"email":"edited@example.org"}]}`
	cancelledRaw := `{"id":"instance","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`
	_, master := calendarFixture(t, st, "series", masterRaw)
	_, err := st.db.Exec(`UPDATE messages SET subject = ? WHERE id = ?`, "Master title", master)
	requirements.NoError(err)
	_, cancelled := calendarFixture(t, st, "instance-cancelled", cancelledRaw)
	_, edited := calendarFixture(t, st, "instance-edited", editedRaw)
	_, err = st.db.Exec(`UPDATE messages SET subject = ? WHERE id = ?`, "Edited title", edited)
	requirements.NoError(err)
	metadata, err := st.GetMessageMetadata(cancelled)
	requirements.NoError(err)
	var fields map[string]any
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &fields))
	fields["status"] = "cancelled"
	encoded, err := json.Marshal(fields, json.Deterministic(true))
	requirements.NoError(err)
	requirements.NoError(st.SetMessageMetadata(cancelled, sql.NullString{String: string(encoded), Valid: true}))

	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(cancelled, page.Occurrences[0].MessageID)
	assertions.Equal(from.Add(14*time.Hour), page.Occurrences[0].Start)
	assertions.Equal(from.Add(15*time.Hour), page.Occurrences[0].End)
	assertions.Equal("Edited title", page.Occurrences[0].Subject)
	requirements.Len(page.Occurrences[0].Attendees, 1)
	assertions.Equal("edited@example.org", page.Occurrences[0].Attendees[0].Email)
}

func TestCompleteCancellationAliasInheritsFreshMissingDetails(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	freshRaw := `{"id":"instance","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T14:00:00Z"},"end":{"dateTime":"2027-03-14T15:00:00Z"},"summary":"Fresh title","organizer":{"email":"fresh-organizer@example.org"},"attendees":[{"email":"fresh@example.org"}]}`
	_, fresh := calendarFixture(t, st, "fresh-alias", freshRaw)
	_, err := st.db.Exec(`UPDATE messages SET subject = ? WHERE id = ?`, "Fresh title", fresh)
	requirements.NoError(err)
	cancelledRaw := `{"id":"instance","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled","start":{"dateTime":"2027-03-14T14:00:00Z"},"end":{"dateTime":"2027-03-14T15:00:00Z"}}`
	_, cancelled := calendarFixtureWithSubject(t, st, "cancelled-alias", cancelledRaw, "")
	rows := calendarRows(t, st, cancelled)
	requirements.Len(rows, 1)
	requirements.Len(rows[0].Event.Attendees, 2)
	assertions.Equal("fresh@example.org", rows[0].Event.Attendees[0].Email)
	assertions.Equal("fresh-organizer@example.org", rows[0].Event.Attendees[1].Email)
	assertions.Equal("Fresh title", rows[0].Subject, "an omitted cancellation summary inherits the freshest alias")
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal("Fresh title", page.Occurrences[0].Subject)
	requirements.Len(page.Occurrences[0].Attendees, 2)
	assertions.Equal("fresh@example.org", page.Occurrences[0].Attendees[0].Email)
}

func TestProviderCancellationMergesExplicitFieldsWithInheritedDetails(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	freshRaw := `{"id":"instance","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T14:00:00Z"},"end":{"dateTime":"2027-03-14T15:00:00Z"},"summary":"Fresh title","organizer":{"email":"old-organizer@example.org"},"attendees":[{"email":"old@example.org"}]}`
	_, fresh := calendarFixture(t, st, "fresh-alias", freshRaw)
	_, err := st.db.Exec(`UPDATE messages SET subject = ? WHERE id = ?`, "Fresh title", fresh)
	requirements.NoError(err)
	cancelledRaw := `{"id":"instance","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled","summary":"","organizer":{"email":"new-organizer@example.org"},"attendees":[{"email":"new@example.org"}]}`
	_, cancelled := calendarFixture(t, st, "cancelled-alias", `{"id":"instance","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`)
	_, err = st.db.Exec(`UPDATE messages SET subject = '' WHERE id = ?`, cancelled)
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageRawWithFormat(cancelled, []byte(cancelledRaw), "gcal_json"))
	rows := calendarRows(t, st, cancelled)
	requirements.Len(rows, 1)
	requirements.NotNil(rows[0].Event.Start)
	requirements.NotNil(rows[0].Event.End)
	assertions.Equal(time.Date(2027, 3, 14, 14, 0, 0, 0, time.UTC), *rows[0].Event.Start)
	assertions.Equal(time.Date(2027, 3, 14, 15, 0, 0, 0, time.UTC), *rows[0].Event.End)
	requirements.Len(rows[0].Event.Attendees, 2)
	assertions.Equal("new@example.org", rows[0].Event.Attendees[0].Email)
	assertions.Equal("new-organizer@example.org", rows[0].Event.Attendees[1].Email)
	assertions.Empty(rows[0].Subject, "an explicitly empty provider summary must remain empty")
}

func TestProviderCancellationMergesAttendeeAndOrganizerFieldsIndependently(t *testing.T) {
	freshRaw := `{"id":"instance","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T14:00:00Z"},"end":{"dateTime":"2027-03-14T15:00:00Z"},"organizer":{"email":"owner@example.org","displayName":"Series Owner","self":true},"attendees":[{"email":"guest@example.org","responseStatus":"accepted"},{"email":"owner@example.org","organizer":true,"responseStatus":"accepted","displayName":"Series Owner","self":true}]}`
	cases := []struct {
		name            string
		cancellationRaw string
		want            []calevent.Attendee
	}{
		{
			name:            "attendee update preserves omitted organizer",
			cancellationRaw: `{"id":"instance","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled","attendees":[{"email":"new@example.org","responseStatus":"declined"}]}`,
			want: []calevent.Attendee{
				{Email: "new@example.org", Role: "required", RSVP: "declined"},
				{Email: "owner@example.org", DisplayName: "Series Owner", Role: "organizer", RSVP: "accepted", IsCopySelf: true},
			},
		},
		{
			name:            "empty attendees preserves omitted organizer",
			cancellationRaw: `{"id":"instance","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled","attendees":[]}`,
			want: []calevent.Attendee{
				{Email: "owner@example.org", DisplayName: "Series Owner", Role: "organizer", RSVP: "accepted", IsCopySelf: true},
			},
		},
		{
			name:            "unchanged organizer preserves omitted details",
			cancellationRaw: `{"id":"instance","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled","organizer":{"email":"owner@example.org"}}`,
			want: []calevent.Attendee{
				{Email: "guest@example.org", Role: "required", RSVP: "accepted"},
				{Email: "owner@example.org", DisplayName: "Series Owner", Role: "organizer", RSVP: "accepted", IsCopySelf: true},
			},
		},
		{
			name:            "replacement organizer preserves attendees",
			cancellationRaw: `{"id":"instance","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled","organizer":{"email":"new-owner@example.org"}}`,
			want: []calevent.Attendee{
				{Email: "guest@example.org", Role: "required", RSVP: "accepted"},
				{Email: "new-owner@example.org", Role: "organizer", RSVP: "unknown"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			calendarFixture(t, st, "fresh-alias", freshRaw)
			_, cancelled := calendarFixture(t, st, "cancelled-alias", `{"id":"instance","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`)
			requirements.NoError(st.UpsertMessageRawWithFormat(cancelled, []byte(tc.cancellationRaw), "gcal_json"))
			rows := calendarRows(t, st, cancelled)
			requirements.Len(rows, 1)
			assertions.ElementsMatch(tc.want, rows[0].Event.Attendees)
		})
	}
}

func TestCancelledAllDayExceptionUsesEventZoneOverCalendarZone(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, event := calendarFixture(t, st, "all-day-event-zone", `{"id":"instance","iCalUID":"allday@example.org","recurringEventId":"missing-series","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-14","timeZone":"America/New_York"},"end":{"date":"2027-03-15","timeZone":"America/New_York"},"summary":"All-day exception"}`)
	requirements.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, `{"calendar_id":"calendar@example.com","time_zone":"America/Los_Angeles"}`))
	metadata, err := st.GetMessageMetadata(event)
	requirements.NoError(err)
	var fields map[string]any
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &fields))
	fields["status"] = "cancelled"
	encoded, err := json.Marshal(fields, json.Deterministic(true))
	requirements.NoError(err)
	requirements.NoError(st.SetMessageMetadata(event, sql.NullString{String: string(encoded), Valid: true}))

	rows := calendarRows(t, st, event)
	requirements.Len(rows, 1)
	assertions.Equal("America/New_York", rows[0].Event.EventTimeZone)
	assertions.Equal("America/Los_Angeles", rows[0].Event.CalendarTimeZone)
	requirements.NotNil(rows[0].Event.Start)
	requirements.NotNil(rows[0].Event.End)
	assertions.Equal(time.Date(2027, 3, 14, 5, 0, 0, 0, time.UTC), *rows[0].Event.Start)
	assertions.Equal(time.Date(2027, 3, 15, 4, 0, 0, 0, time.UTC), *rows[0].Event.End)
}

func TestCancelledAllDayExceptionRecomputesBoundsAfterSourceZoneChange(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, event := calendarFixture(t, st, "all-day-exception", `{"id":"instance","iCalUID":"allday@example.org","recurringEventId":"missing-series","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"},"summary":"All-day exception"}`)
	metadata, err := st.GetMessageMetadata(event)
	requirements.NoError(err)
	var fields map[string]any
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &fields))
	fields["status"] = "cancelled"
	encoded, err := json.Marshal(fields, json.Deterministic(true))
	requirements.NoError(err)
	requirements.NoError(st.SetMessageMetadata(event, sql.NullString{String: string(encoded), Valid: true}))
	_, note := projectionFixture(t, st, "all-day-note", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	requirements.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, `{"calendar_id":"calendar@example.com","time_zone":"America/Los_Angeles"}`))

	wantStart := time.Date(2027, 3, 14, 8, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2027, 3, 15, 7, 0, 0, 0, time.UTC)
	rows := calendarRows(t, st, event)
	requirements.Len(rows, 1)
	assertions.Equal("America/Los_Angeles", rows[0].Event.CalendarTimeZone)
	requirements.NotNil(rows[0].Event.Start)
	requirements.NotNil(rows[0].Event.End)
	assertions.Equal(wantStart, *rows[0].Event.Start)
	assertions.Equal(wantEnd, *rows[0].Event.End)
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 2))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(wantStart, page.Occurrences[0].Start)
	assertions.Equal(wantEnd, page.Occurrences[0].End)
	links, err := st.MeetingLinksContext(t.Context(), note)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(wantStart, links[0].Start)
	assertions.Equal(wantEnd, links[0].End)
}

func TestUIDlessCancellationAliasKeepsSavedSplitAcrossSourceReimport(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	originalRaw := `{"id":"instance","iCalUID":"orphan@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T06:00:00","timeZone":"America/New_York"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`
	data, original := calendarFixture(t, st, "instance", originalRaw)
	_, note := projectionFixture(t, st, "uidless-cancel-note", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: original}))
	cancelledRaw := `{"id":"instance","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`
	_, cancelled := calendarFixture(t, st, "series|2027-03-14T10:00:00Z", cancelledRaw)
	rows := calendarRows(t, st, cancelled)
	requirements.Len(rows, 1)
	assertions.Equal("orphan@example.org", rows[0].ICalUID)
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: cancelled}))
	var uid, decision string
	requirements.NoError(st.db.QueryRow(`SELECT target_ical_uid, decision FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&uid, &decision))
	assertions.Equal("orphan@example.org", uid)
	assertions.Equal("split", decision)

	requirements.NoError(st.RemoveSource(data.Message.SourceID))
	calendarFixture(t, st, "instance", originalRaw)
	links, err := st.MeetingLinksContext(t.Context(), note)
	requirements.NoError(err)
	assertions.Empty(links, "the saved UID split remains effective after source reimport")
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
	assertions, requirements := assert.New(t), require.New(t)

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

func TestCalendarEventUpgradeProjectsCompleteAliasesBeforeCancellations(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	original := `"originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}`
	staleRaw := `{"id":"instance","iCalUID":"weekly@example.org","recurringEventId":"series",` + original + `,"status":"cancelled","start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"},"summary":"Stale details"}`
	freshRaw := `{"id":"instance","iCalUID":"weekly@example.org","recurringEventId":"series",` + original + `,"start":{"dateTime":"2027-03-14T14:00:00Z"},"end":{"dateTime":"2027-03-14T15:00:00Z"},"summary":"Fresh details"}`
	_, cancelled := calendarFixture(t, st, "cancelled-alias", staleRaw)
	_, fresh := calendarFixture(t, st, "fresh-alias", freshRaw)
	newer := time.Date(2027, 3, 15, 0, 0, 0, 0, time.UTC)
	_, err := st.db.Exec(`UPDATE messages SET last_modified = ? WHERE id = ?`, newer, cancelled)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE messages SET last_modified = ? WHERE id = ?`, newer.Add(-time.Hour), fresh)
	requirements.NoError(err)
	for _, query := range []string{`DROP TABLE calendar_event_attendees`, `DROP TABLE calendar_events`, `DELETE FROM applied_migrations WHERE name = 'calendar_event_projection_v1'`, `UPDATE archive_metadata SET value = '10' WHERE key = 'schema_version'`} {
		_, err := st.db.Exec(query)
		requirements.NoError(err)
	}
	requirements.NoError(st.Close())
	reopened, err := OpenForTest(st.dbPath)
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(reopened.Close()) })
	requirements.NoError(reopened.InitSchemaContext(t.Context()))
	rows := calendarRows(t, reopened, cancelled)
	requirements.Len(rows, 1)
	assertions.Equal("cancelled", rows[0].Event.Status)
	requirements.NotNil(rows[0].Event.Start)
	requirements.NotNil(rows[0].Event.End)
	assertions.Equal(time.Date(2027, 3, 14, 14, 0, 0, 0, time.UTC), *rows[0].Event.Start)
	assertions.Equal(time.Date(2027, 3, 14, 15, 0, 0, 0, time.UTC), *rows[0].Event.End)
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := reopened.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(cancelled, page.Occurrences[0].MessageID)
	assertions.Equal(from.Add(14*time.Hour), page.Occurrences[0].Start)
}

func TestCalendarEventUpgradeSeedsCancelledAliasesBeforeReconstruction(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	original := `"originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}`
	staleRaw := `{"id":"instance","iCalUID":"weekly@example.org","recurringEventId":"series",` + original + `,"status":"cancelled","start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"},"attendees":[{"email":"stale@example.org"}]}`
	freshRaw := `{"id":"instance","iCalUID":"weekly@example.org","recurringEventId":"series",` + original + `,"status":"cancelled","start":{"dateTime":"2027-03-14T14:00:00Z"},"end":{"dateTime":"2027-03-14T15:00:00Z"},"attendees":[{"email":"fresh@example.org"}]}`
	_, stale := calendarFixture(t, st, "stale-cancelled-alias", staleRaw)
	_, fresh := calendarFixture(t, st, "fresh-cancelled-alias", freshRaw)
	newer := time.Date(2027, 3, 15, 0, 0, 0, 0, time.UTC)
	_, err := st.db.Exec(`UPDATE messages SET last_modified = ? WHERE id = ?`, newer.Add(-time.Hour), stale)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE messages SET last_modified = ? WHERE id = ?`, newer, fresh)
	requirements.NoError(err)
	for _, query := range []string{`DROP TABLE calendar_event_attendees`, `DROP TABLE calendar_events`, `DELETE FROM applied_migrations WHERE name = 'calendar_event_projection_v1'`, `UPDATE archive_metadata SET value = '10' WHERE key = 'schema_version'`} {
		_, err := st.db.Exec(query)
		requirements.NoError(err)
	}
	requirements.NoError(st.Close())
	reopened, err := OpenForTest(st.dbPath)
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(reopened.Close()) })
	requirements.NoError(reopened.InitSchemaContext(t.Context()))
	rows := calendarRows(t, reopened, stale)
	requirements.Len(rows, 1)
	assertions.Equal("cancelled", rows[0].Event.Status)
	requirements.NotNil(rows[0].Event.Start)
	requirements.NotNil(rows[0].Event.End)
	assertions.Equal(time.Date(2027, 3, 14, 14, 0, 0, 0, time.UTC), *rows[0].Event.Start)
	assertions.Equal(time.Date(2027, 3, 14, 15, 0, 0, 0, time.UTC), *rows[0].Event.End)
	requirements.Len(rows[0].Event.Attendees, 1)
	assertions.Equal("fresh@example.org", rows[0].Event.Attendees[0].Email)
}

func TestCopySubsetRebuildsCalendarEvents(t *testing.T) {
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite subset copy")
	}
	assertions, requirements := assert.New(t), require.New(t)
	path := filepath.Join(t.TempDir(), "source.db")
	st, err := OpenForTest(path)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	_, id := calendarFixture(t, st, "subset", calendarRaw)
	firstRaw := `{"id":"instance","iCalUID":"orphan@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`
	secondRaw := strings.Replace(firstRaw, `"2027-03-14T12:00:00Z"`, `"2027-03-14T14:00:00Z"`, 1)
	secondRaw = strings.Replace(secondRaw, `"2027-03-14T13:00:00Z"`, `"2027-03-14T15:00:00Z"`, 1)
	_, first := calendarFixture(t, st, "instance", firstRaw)
	_, second := calendarFixture(t, st, "series|2027-03-14T10:00:00Z", secondRaw)
	firstFresh := time.Now().UTC()
	secondFresh := firstFresh.Add(-time.Minute)
	_, err = st.db.Exec(`UPDATE calendar_events SET snapshot_updated_at = ? WHERE message_id = ?`, firstFresh, first)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE calendar_events SET snapshot_updated_at = ? WHERE message_id = ?`, secondFresh, second)
	requirements.NoError(err)
	// The event projection is carried only to preserve provider freshness. Its
	// derived fields are marked stale and rebuilt from raw before the copy opens.
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
	assertions.Equal("event-example", rows[0].Event.ProviderEventID)
	var copiedFirstFresh, copiedSecondFresh time.Time
	requirements.NoError(copied.db.QueryRow(`SELECT snapshot_updated_at FROM calendar_events WHERE message_id = ?`, first).Scan(&copiedFirstFresh))
	requirements.NoError(copied.db.QueryRow(`SELECT snapshot_updated_at FROM calendar_events WHERE message_id = ?`, second).Scan(&copiedSecondFresh))
	assertions.True(copiedFirstFresh.After(copiedSecondFresh), "subset reconstruction must preserve provider snapshot ordering even when it opposes message ID order")
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := copied.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(first, page.Occurrences[0].MessageID)
	assertions.Equal(from.Add(12*time.Hour), page.Occurrences[0].Start)
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
