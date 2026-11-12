package cmd

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMeetingCountsCalendarHistoryAndAvailableTranscripts(t *testing.T) {
	dir := t.TempDir()
	dbPath, analytics := filepath.Join(dir, "archive.db"), filepath.Join(dir, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	source, err := st.GetOrCreateSource("gcal", "owner@example.com")
	require.NoError(t, err)
	notesSource, err := st.GetOrCreateSource("meeting_import", "owner@example.com")
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), source.ID, `{"calendar_id":"owner@example.com","time_zone":"UTC"}`))
	require.NoError(t, st.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
	require.NoError(t, st.AddAccountIdentity(notesSource.ID, "owner@example.com", "manual"))
	owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(t, err)
	person, err := st.EnsureParticipant("person@example.com", "Person", "example.com")
	require.NoError(t, err)
	start := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
	makeMessage := func(sourceID int64, key, typ, format, raw string) int64 {
		t.Helper()
		id, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: sourceID, SourceMessageID: key, MessageType: typ, SentAt: sql.NullTime{Time: start, Valid: true}, SenderID: sql.NullInt64{Int64: owner, Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting"},
			RawMIME:      []byte(raw), RawFormat: format,
			Metadata: &sql.NullString{String: `{"calendar_id":"owner@example.com"}`, Valid: true},
		})
		require.NoError(t, err)
		require.NoError(t, st.ReplaceMessageRecipients(id, "from", []int64{owner}, []string{""}))
		require.NoError(t, st.ReplaceMessageRecipients(id, "to", []int64{person}, []string{""}))
		return id
	}
	makeMessage(source.ID, "series", "calendar_event", "gcal_json", `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=4"]}`)
	makeMessage(source.ID, "granola", "meeting_transcript", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"series_20240109T100000Z"},"transcript":[{"text":"Hello"}]}`)
	makeMessage(notesSource.ID, "twenty", "meeting_transcript", "twenty_json", `{"schema_version":1,"recording":{"transcript":[{"words":[{"text":"Hello"}]}]},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2024-01-09T10:00:00Z"}}`)
	makeMessage(source.ID, "pending", "meeting_transcript", "meeting_json", `{}`)
	_, err = buildCache(t.Context(), dbPath, analytics, true)
	require.NoError(t, err)
	engine, err := query.NewDuckDBEngine(t.Context(), analytics, "", nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	detail, err := engine.GetPerson(t.Context(), person, query.Context{}, nil)
	require.NoError(t, err)
	require.NotNil(t, detail)
	assert.EqualValues(t, 4, detail.MeetingCount)
	for _, tc := range []struct {
		ids  []int64
		want int64
	}{{[]int64{source.ID}, 4}, {[]int64{notesSource.ID}, 1}, {[]int64{source.ID, notesSource.ID}, 4}} {
		context := query.Context{SourceIDs: tc.ids}
		detail, err := engine.GetPerson(t.Context(), person, context, nil)
		require.NoError(t, err)
		require.NotNil(t, detail)
		assert.Equal(t, tc.want, detail.MeetingCount)
		people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{Explore: query.ExploreRequest{Context: context}})
		require.NoError(t, err)
		found := false
		for _, row := range people.Rows {
			if row.ID == person {
				assert.Equal(t, tc.want, row.MeetingCount)
				found = true
			}
		}
		assert.True(t, found)
		relationships, err := engine.Relationships(t.Context(), query.RelationshipsRequest{Context: context, ShowAll: true})
		require.NoError(t, err)
		found = false
		for _, row := range relationships.Rows {
			if row.CanonicalID == person {
				assert.Equal(t, tc.want, row.Signals.MeetingCount)
				found = true
			}
		}
		assert.True(t, found)
	}
	calendar, err := engine.RelationshipCalendar(t.Context(), query.RelationshipCalendarRequest{CanonicalID: person, Year: 2024, Timezone: "UTC"})
	require.NoError(t, err)
	var days []string
	for _, day := range calendar.Days {
		if day.Meetings != 0 {
			days = append(days, fmt.Sprintf("%s:%d", day.Date, day.Meetings))
		}
	}
	assert.Equal(t, []string{"2024-01-02:1", "2024-01-09:1", "2024-01-16:1", "2024-01-23:1"}, days)
	timeline, err := engine.RelationshipTimeline(t.Context(), query.RelationshipTimelineRequest{CanonicalID: person, Timezone: "UTC"})
	require.NoError(t, err)
	var timelineDates []string
	for _, row := range timeline.Rows {
		timelineDates = append(timelineDates, row.OccurredAt.UTC().Format(time.DateOnly))
	}
	assert.Equal(t, []string{"2024-01-23", "2024-01-16", "2024-01-09", "2024-01-02"}, timelineDates)
	after, before := time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC)
	filtered := query.Context{After: &after, Before: &before}
	timeline, err = engine.RelationshipTimeline(t.Context(), query.RelationshipTimelineRequest{CanonicalID: person, Timezone: "UTC", Context: filtered})
	require.NoError(t, err)
	timelineDates = nil
	for _, row := range timeline.Rows {
		timelineDates = append(timelineDates, row.OccurredAt.UTC().Format(time.DateOnly))
	}
	assert.Equal(t, []string{"2024-01-16", "2024-01-09"}, timelineDates)
	detail, err = engine.GetPerson(t.Context(), person, filtered, nil)
	require.NoError(t, err)
	require.NotNil(t, detail)
	assert.EqualValues(t, 2, detail.MeetingCount)
	people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{Explore: query.ExploreRequest{Context: filtered}})
	require.NoError(t, err)
	var found bool
	for _, row := range people.Rows {
		if row.ID == person {
			assert.EqualValues(t, 2, row.MeetingCount)
			found = true
		}
	}
	assert.True(t, found)
	relationships, err := engine.Relationships(t.Context(), query.RelationshipsRequest{Context: filtered, ShowAll: true})
	require.NoError(t, err)
	found = false
	for _, row := range relationships.Rows {
		if row.CanonicalID == person {
			assert.EqualValues(t, 2, row.Signals.MeetingCount)
			found = true
		}
	}
	assert.True(t, found)
}

func TestMeetingCountsRefresh(t *testing.T) {
	dir := t.TempDir()
	dbPath, analytics := filepath.Join(dir, "archive.db"), filepath.Join(dir, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	source, err := st.GetOrCreateSource("gcal", "owner@example.com")
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), source.ID, `{"calendar_id":"owner@example.com","time_zone":"UTC"}`))
	require.NoError(t, st.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
	owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(t, err)
	person, err := st.EnsureParticipant("person@example.com", "Person", "example.com")
	require.NoError(t, err)
	alias, err := st.EnsureParticipant("person-alias@example.com", "Person", "example.com")
	require.NoError(t, err)
	_, err = st.LinkParticipants(person, alias)
	require.NoError(t, err)
	start := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
	makeMessage := func(key, typ, format, raw string) int64 {
		t.Helper()
		id, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: key, MessageType: typ, SentAt: sql.NullTime{Time: start, Valid: true}, SenderID: sql.NullInt64{Int64: owner, Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting"},
			RawMIME:      []byte(raw), RawFormat: format,
			Metadata: &sql.NullString{String: `{"calendar_id":"owner@example.com"}`, Valid: true},
		})
		require.NoError(t, err)
		require.NoError(t, st.ReplaceMessageRecipients(id, "from", []int64{owner}, []string{""}))
		require.NoError(t, st.ReplaceMessageRecipients(id, "to", []int64{person}, []string{""}))
		return id
	}
	event := makeMessage("event", "calendar_event", "gcal_json", `{"id":"event","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"},"attendees":[{"email":"person-alias@example.com","responseStatus":"accepted"}]}`)
	note := makeMessage("note", "meeting_transcript", "meeting_json", `{"transcript":"Notes"}`)
	eventRef, noteRef := store.MeetingLinkRef{Kind: "evt", OccurrenceRef: store.OccurrenceRef{MessageID: event}}, store.MeetingLinkRef{Kind: "msg", OccurrenceRef: store.OccurrenceRef{MessageID: note}}
	_, err = st.DecideMeetingLinkContext(t.Context(), eventRef, noteRef, "link")
	require.NoError(t, err)
	_, err = buildCache(t.Context(), dbPath, analytics, true)
	require.NoError(t, err)
	count := func() int64 {
		t.Helper()
		engine, err := query.NewDuckDBEngine(t.Context(), analytics, "", nil)
		require.NoError(t, err)
		defer func() { require.NoError(t, engine.Close()) }()
		person, err := engine.GetPerson(t.Context(), person, query.Context{}, nil)
		require.NoError(t, err)
		require.NotNil(t, person)
		return person.MeetingCount
	}
	assert.EqualValues(t, 1, count())
	_, err = st.DecideMeetingLinkContext(t.Context(), eventRef, noteRef, "unlink")
	require.NoError(t, err)
	result, err := buildCache(t.Context(), dbPath, analytics, false)
	require.NoError(t, err)
	assert.True(t, result.IdentityOnly)
	assert.EqualValues(t, 2, count())

	_, err = st.DecideMeetingLinkContext(t.Context(), eventRef, noteRef, "link")
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE meeting_details SET transcript_state='pending' WHERE message_id=?`, note)
	require.NoError(t, err)
	result, err = buildCache(t.Context(), dbPath, analytics, false)
	require.NoError(t, err)
	assert.True(t, result.IdentityOnly)
	assert.EqualValues(t, 1, count())
	tx, err := st.DB().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE calendar_events SET content_hash='updated-evidence' WHERE message_id=?`, event)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `DELETE FROM calendar_event_attendees WHERE message_id=?`, event)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO calendar_event_attendees (message_id, ordinal, email, role, rsvp) VALUES (?, 0, 'person-alias@example.com', 'required', 'declined')`, event)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	result, err = buildCache(t.Context(), dbPath, analytics, false)
	require.NoError(t, err)
	assert.True(t, result.IdentityOnly)
	assert.EqualValues(t, 0, count())
	_, err = st.DB().ExecContext(t.Context(), `UPDATE meeting_details SET transcript_state='available' WHERE message_id=?`, note)
	require.NoError(t, err)
	result, err = buildCache(t.Context(), dbPath, analytics, false)
	require.NoError(t, err)
	assert.True(t, result.IdentityOnly)
	assert.EqualValues(t, 1, count())
	_, err = st.DB().ExecContext(t.Context(), `UPDATE meeting_details SET transcript_state='pending' WHERE message_id=?`, note)
	require.NoError(t, err)
	derivedRefreshBeforeSnapshotHook = func() {
		_, hookErr := st.DB().ExecContext(t.Context(), `UPDATE calendar_events SET status='cancelled' WHERE message_id=?`, event)
		require.NoError(t, hookErr)
	}
	t.Cleanup(func() { derivedRefreshBeforeSnapshotHook = nil })
	result, err = buildCache(t.Context(), dbPath, analytics, false)
	require.NoError(t, err)
	derivedRefreshBeforeSnapshotHook = nil
	assert.True(t, result.IdentityOnly)
	assert.EqualValues(t, 0, count())
	assert.False(t, cacheNeedsBuild(dbPath, analytics).NeedsBuild)
}

func TestMeetingCountsExcludeFutureEventWithPastMessageDate(t *testing.T) {
	dir := t.TempDir()
	dbPath, analytics := filepath.Join(dir, "archive.db"), filepath.Join(dir, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	source, err := st.GetOrCreateSource("gcal", "owner@example.com")
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), source.ID, `{"calendar_id":"owner@example.com","time_zone":"UTC"}`))
	require.NoError(t, st.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
	owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(t, err)
	person, err := st.EnsureParticipant("person@example.com", "Person", "example.com")
	require.NoError(t, err)
	start := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	raw := fmt.Sprintf(`{"id":"future","start":{"dateTime":%q},"end":{"dateTime":%q},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339))
	id, err := st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "future", MessageType: "calendar_event", SentAt: sql.NullTime{Time: start.Add(-48 * time.Hour), Valid: true}, SenderID: sql.NullInt64{Int64: owner, Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "future", ConversationType: "meeting"},
		RawMIME:      []byte(raw), RawFormat: "gcal_json", Metadata: &sql.NullString{String: `{"calendar_id":"owner@example.com"}`, Valid: true},
	})
	require.NoError(t, err)
	require.NoError(t, st.ReplaceMessageRecipients(id, "from", []int64{owner}, []string{""}))
	require.NoError(t, st.ReplaceMessageRecipients(id, "to", []int64{person}, []string{""}))
	_, err = buildCache(t.Context(), dbPath, analytics, true)
	require.NoError(t, err)
	count := func() int64 {
		t.Helper()
		engine, err := query.NewDuckDBEngine(t.Context(), analytics, "", nil)
		require.NoError(t, err)
		defer func() { require.NoError(t, engine.Close()) }()
		detail, err := engine.GetPerson(t.Context(), person, query.Context{}, nil)
		require.NoError(t, err)
		require.NotNil(t, detail)
		return detail.MeetingCount
	}
	assert.Zero(t, count())
	assert.False(t, cacheNeedsBuild(dbPath, analytics).NeedsBuild)
	oldNow := derivedRefreshNow
	deadline := start
	if nextYear := time.Date(time.Now().UTC().Year()+1, 1, 1, 0, 0, 0, 0, time.UTC); nextYear.Before(deadline) {
		deadline = nextYear
	}
	derivedRefreshNow = func() time.Time { return deadline.Add(-time.Second) }
	assert.False(t, cacheNeedsBuild(dbPath, analytics).NeedsBuild)
	derivedRefreshNow = func() time.Time { return start.Add(time.Hour) }
	t.Cleanup(func() { derivedRefreshNow = oldNow })
	assert.True(t, cacheNeedsBuild(dbPath, analytics).NeedsBuild)
	result, err := buildCache(t.Context(), dbPath, analytics, false)
	require.NoError(t, err)
	assert.True(t, result.IdentityOnly)
	assert.EqualValues(t, 1, count())
}

func TestMeetingCountsAllDayCalendarUsesCivilDate(t *testing.T) {
	dir := t.TempDir()
	dbPath, analytics := filepath.Join(dir, "archive.db"), filepath.Join(dir, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	source, err := st.GetOrCreateSource("gcal", "owner@example.com")
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), source.ID, `{"calendar_id":"owner@example.com","time_zone":"UTC"}`))
	require.NoError(t, st.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
	owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(t, err)
	person, err := st.EnsureParticipant("person@example.com", "Person", "example.com")
	require.NoError(t, err)
	for _, date := range []string{"2024-01-01", "2024-12-31"} {
		start, err := time.Parse(time.DateOnly, date)
		require.NoError(t, err)
		raw := fmt.Sprintf(`{"id":%q,"start":{"date":%q},"end":{"date":%q}}`, date, date, start.AddDate(0, 0, 1).Format(time.DateOnly))
		id, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: date, MessageType: "calendar_event", SentAt: sql.NullTime{Time: start, Valid: true}, SenderID: sql.NullInt64{Int64: owner, Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: date, ConversationType: "meeting"},
			RawMIME:      []byte(raw), RawFormat: "gcal_json", Metadata: &sql.NullString{String: `{"calendar_id":"owner@example.com"}`, Valid: true},
		})
		require.NoError(t, err)
		require.NoError(t, st.ReplaceMessageRecipients(id, "from", []int64{owner}, []string{""}))
		require.NoError(t, st.ReplaceMessageRecipients(id, "to", []int64{person}, []string{""}))
	}
	_, err = buildCache(t.Context(), dbPath, analytics, true)
	require.NoError(t, err)
	engine, err := query.NewDuckDBEngine(t.Context(), analytics, "", nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	for _, zone := range []string{"America/Los_Angeles", "Pacific/Kiritimati"} {
		t.Run(zone, func(t *testing.T) {
			calendar, err := engine.RelationshipCalendar(t.Context(), query.RelationshipCalendarRequest{CanonicalID: person, Year: 2024, Timezone: zone})
			require.NoError(t, err)
			var days []string
			for _, day := range calendar.Days {
				if day.Meetings != 0 {
					days = append(days, day.Date)
				}
			}
			assert.Equal(t, []string{"2024-01-01", "2024-12-31"}, days)
		})
	}
}
