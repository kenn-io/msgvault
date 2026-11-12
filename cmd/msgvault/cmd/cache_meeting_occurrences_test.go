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
	makeMessage(notesSource.ID, "granola", "meeting_transcript", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"series_20240109T100000Z"},"transcript":[{"text":"Hello"}]}`)
	makeMessage(notesSource.ID, "twenty", "meeting_transcript", "twenty_json", `{"schema_version":1,"recording":{"transcript":[{"words":[{"text":"Hello"}]}]},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2024-01-09T10:00:00Z"}}`)
	makeMessage(notesSource.ID, "pending", "meeting_transcript", "meeting_json", `{}`)
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
				assert.Equal(t, tc.want, row.ActivityCount)
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
	assert.EqualValues(t, 2, detail.ActivityCount)
	assert.Equal(t, time.Date(2024, 1, 9, 10, 0, 0, 0, time.UTC), detail.FirstAt)
	domain, err := engine.GetDomain(t.Context(), "example.com", filtered)
	require.NoError(t, err)
	require.NotNil(t, domain)
	assert.EqualValues(t, 2, domain.ActivityCount)
	people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{Explore: query.ExploreRequest{Context: filtered}})
	require.NoError(t, err)
	var found bool
	for _, row := range people.Rows {
		if row.ID == person {
			assert.EqualValues(t, 2, row.MeetingCount)
			assert.EqualValues(t, 2, row.ActivityCount)
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
	notesSource, err := st.GetOrCreateSource("meeting_import", "owner@example.com")
	require.NoError(t, err)
	require.NoError(t, st.AddAccountIdentity(notesSource.ID, "owner@example.com", "manual"))
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
	event := makeMessage(source.ID, "event", "calendar_event", "gcal_json", `{"id":"event","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"},"attendees":[{"email":"person-alias@example.com","responseStatus":"accepted"}]}`)
	note := makeMessage(notesSource.ID, "note", "meeting_transcript", "meeting_json", `{"transcript":"Notes"}`)
	eventRef, noteRef := store.MeetingLinkRef{Kind: "evt", MessageID: event}, store.MeetingLinkRef{Kind: "msg", MessageID: note}
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
	assert.Equal(t, note, makeMessage(notesSource.ID, "note", "meeting_transcript", "meeting_json", `{}`))
	_, err = buildCache(t.Context(), dbPath, analytics, false)
	require.NoError(t, err)
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
	assert.Equal(t, note, makeMessage(notesSource.ID, "note", "meeting_transcript", "meeting_json", `{"transcript":"Notes"}`))
	_, err = buildCache(t.Context(), dbPath, analytics, false)
	require.NoError(t, err)
	assert.EqualValues(t, 1, count())
	assert.Equal(t, note, makeMessage(notesSource.ID, "note", "meeting_transcript", "meeting_json", `{}`))
	tx, err = st.DB().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE calendar_events SET content_hash='accepted-evidence' WHERE message_id=?`, event)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `DELETE FROM calendar_event_attendees WHERE message_id=?`, event)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO calendar_event_attendees (message_id, ordinal, email, role, rsvp) VALUES (?, 0, 'person-alias@example.com', 'required', 'accepted')`, event)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	_, err = buildCache(t.Context(), dbPath, analytics, false)
	require.NoError(t, err)
	assert.EqualValues(t, 1, count())
	_, err = st.DB().ExecContext(t.Context(), `UPDATE calendar_events SET status='tentative' WHERE message_id=?`, event)
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

func TestMeetingCountsPinnedSnapshot(t *testing.T) {
	for _, csv := range []bool{false, true} {
		for _, change := range []string{"link", "cancel"} {
			t.Run(fmt.Sprintf("csv-%t/%s", csv, change), func(t *testing.T) {
				if csv {
					t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "1")
				}
				dir := t.TempDir()
				dbPath, analytics := filepath.Join(dir, "archive.db"), filepath.Join(dir, "analytics")
				st := testutil.NewSQLiteTestStoreAt(t, dbPath)
				calendar, err := st.GetOrCreateSource("gcal", "owner@example.com")
				require.NoError(t, err)
				notes, err := st.GetOrCreateSource("meeting_import", "owner@example.com")
				require.NoError(t, err)
				require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), calendar.ID, `{"calendar_id":"owner@example.com"}`))
				require.NoError(t, st.AddAccountIdentity(calendar.ID, "owner@example.com", "manual"))
				require.NoError(t, st.AddAccountIdentity(notes.ID, "owner@example.com", "manual"))
				owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
				require.NoError(t, err)
				person, err := st.EnsureParticipant("person@example.com", "Person", "example.com")
				require.NoError(t, err)
				start := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
				persist := func(source int64, key, typ, format, raw string) int64 {
					id, err := st.PersistMessage(&store.MessagePersistData{
						Message:      &store.Message{SourceID: source, SourceMessageID: key, MessageType: typ, SentAt: sql.NullTime{Time: start, Valid: true}, SenderID: sql.NullInt64{Int64: owner, Valid: true}},
						Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting"}, RawMIME: []byte(raw), RawFormat: format,
					})
					require.NoError(t, err)
					require.NoError(t, st.ReplaceMessageRecipients(id, "from", []int64{owner}, []string{""}))
					require.NoError(t, st.ReplaceMessageRecipients(id, "to", []int64{person}, []string{""}))
					return id
				}
				event := persist(calendar.ID, "event", "calendar_event", "gcal_json", `{"id":"event","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"}}`)
				var note int64
				if change == "link" {
					note = persist(notes.ID, "note", "meeting_transcript", "meeting_json", `{"transcript":"Notes"}`)
				}
				var pinned, written int64
				buildCacheBeforeMessagesExportHook = func() error {
					if err := st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal`).Scan(&pinned); err != nil {
						return err
					}
					if change == "link" {
						_, err = st.DecideMeetingLinkContext(t.Context(), store.MeetingLinkRef{Kind: "evt", MessageID: event}, store.MeetingLinkRef{Kind: "msg", MessageID: note}, "link")
					} else {
						_, err = st.DB().ExecContext(t.Context(), `UPDATE calendar_events SET status='cancelled' WHERE message_id=?`, event)
					}
					if err != nil {
						return err
					}
					return st.DB().QueryRowContext(t.Context(), `SELECT MAX(seq) FROM cache_related_change_journal`).Scan(&written)
				}
				t.Cleanup(func() { buildCacheBeforeMessagesExportHook = nil })
				_, err = buildCache(t.Context(), dbPath, analytics, true)
				buildCacheBeforeMessagesExportHook = nil
				require.NoError(t, err)
				state, err := query.ReadCacheSyncState(analytics)
				require.NoError(t, err)
				assert.Equal(t, pinned, state.LastRelatedChangeSeq)
				assert.Greater(t, written, pinned)
				count := func() int64 {
					engine, err := query.NewDuckDBEngine(t.Context(), analytics, "", nil)
					require.NoError(t, err)
					defer func() { require.NoError(t, engine.Close()) }()
					row, err := engine.GetPerson(t.Context(), person, query.Context{}, nil)
					require.NoError(t, err)
					require.NotNil(t, row)
					return row.MeetingCount
				}
				if change == "link" {
					assert.EqualValues(t, 2, count())
				} else {
					assert.EqualValues(t, 1, count())
				}
				_, err = buildCache(t.Context(), dbPath, analytics, false)
				require.NoError(t, err)
				if change == "link" {
					assert.EqualValues(t, 1, count())
				} else {
					assert.Zero(t, count())
				}
			})
		}
	}
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
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	oldNow := derivedRefreshNow
	derivedRefreshNow = func() time.Time { return now }
	t.Cleanup(func() { derivedRefreshNow = oldNow })
	start := now.Add(2 * time.Hour)
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
	_, err = st.DB().ExecContext(t.Context(), `UPDATE calendar_events SET content_hash='refresh-fixture' WHERE message_id=?`, id)
	require.NoError(t, err)
	_, err = buildCache(t.Context(), dbPath, analytics, false)
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
	derivedRefreshNow = func() time.Time { return start.Add(-time.Second) }
	assert.False(t, cacheNeedsBuild(dbPath, analytics).NeedsBuild)
	derivedRefreshNow = func() time.Time { return start.Add(time.Hour) }
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

func TestMeetingCountsAllDayRefresh(t *testing.T) {
	year := 2024
	for _, tc := range []struct {
		name, zone          string
		date, before, start time.Time
		newYear             bool
	}{
		{"tokyo", "Asia/Tokyo", time.Date(year, 3, 5, 0, 0, 0, 0, time.UTC), time.Date(year, 3, 4, 14, 30, 0, 0, time.UTC), time.Date(year, 3, 4, 15, 0, 0, 0, time.UTC), false},
		{"los_angeles", "America/Los_Angeles", time.Date(year, 3, 5, 0, 0, 0, 0, time.UTC), time.Date(year, 3, 5, 0, 30, 0, 0, time.UTC), time.Date(year, 3, 5, 8, 0, 0, 0, time.UTC), false},
		{"tokyo_new_year", "Asia/Tokyo", time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(year, 12, 31, 9, 0, 0, 0, time.UTC), time.Date(year, 12, 31, 15, 0, 0, 0, time.UTC), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath, analytics := filepath.Join(dir, "archive.db"), filepath.Join(dir, "analytics")
			st := testutil.NewSQLiteTestStoreAt(t, dbPath)
			source, err := st.GetOrCreateSource("gcal", "owner@example.com")
			require.NoError(t, err)
			require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), source.ID, fmt.Sprintf(`{"calendar_id":"owner@example.com","time_zone":%q}`, tc.zone)))
			require.NoError(t, st.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
			owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
			require.NoError(t, err)
			person, err := st.EnsureParticipant("person@example.com", "Person", "example.com")
			require.NoError(t, err)
			raw := fmt.Sprintf(`{"id":"offsite","start":{"date":%q},"end":{"date":%q}}`, tc.date.Format(time.DateOnly), tc.date.AddDate(0, 0, 1).Format(time.DateOnly))
			id, err := st.PersistMessage(&store.MessagePersistData{
				Message:      &store.Message{SourceID: source.ID, SourceMessageID: "offsite", MessageType: "calendar_event", SentAt: sql.NullTime{Time: tc.date, Valid: true}, SenderID: sql.NullInt64{Int64: owner, Valid: true}},
				Conversation: &store.ConversationPersistData{SourceConversationID: "offsite", ConversationType: "meeting"},
				RawMIME:      []byte(raw), RawFormat: "gcal_json", Metadata: &sql.NullString{String: `{"calendar_id":"owner@example.com"}`, Valid: true},
			})
			require.NoError(t, err)
			require.NoError(t, st.ReplaceMessageRecipients(id, "from", []int64{owner}, []string{""}))
			require.NoError(t, st.ReplaceMessageRecipients(id, "to", []int64{person}, []string{""}))
			_, err = buildCache(t.Context(), dbPath, analytics, true)
			require.NoError(t, err)
			oldNow := derivedRefreshNow
			t.Cleanup(func() { derivedRefreshNow = oldNow })
			refresh := func(now time.Time) {
				t.Helper()
				derivedRefreshNow = func() time.Time { return now }
				result, err := buildCache(t.Context(), dbPath, analytics, false)
				require.NoError(t, err)
				assert.True(t, result.IdentityOnly)
			}
			state := func() query.CacheSyncState {
				t.Helper()
				value, err := query.ReadCacheSyncState(analytics)
				require.NoError(t, err)
				return value
			}
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
			_, err = st.DB().ExecContext(t.Context(), `UPDATE calendar_events SET content_hash='refresh-fixture' WHERE message_id=?`, id)
			require.NoError(t, err)
			refresh(tc.before)
			assert.Zero(t, count())
			if tc.newYear {
				assert.Equal(t, tc.before.Add(time.Hour), state().NextMeetingAt)
				refresh(tc.before.Add(time.Hour))
			}
			assert.Equal(t, tc.start, state().NextMeetingAt)
			derivedRefreshNow = func() time.Time { return tc.start.Add(30 * time.Minute) }
			assert.True(t, cacheNeedsBuild(dbPath, analytics).HasMeetingTimeDrift)
			refresh(tc.start.Add(30 * time.Minute))
			assert.EqualValues(t, 1, count())
			engine, err := query.NewDuckDBEngine(t.Context(), analytics, "", nil)
			require.NoError(t, err)
			for _, zone := range []string{"UTC", "America/Los_Angeles"} {
				calendar, err := engine.RelationshipCalendar(t.Context(), query.RelationshipCalendarRequest{CanonicalID: person, Year: tc.date.Year(), Timezone: zone})
				require.NoError(t, err)
				var dates []string
				for _, day := range calendar.Days {
					if day.Meetings > 0 {
						dates = append(dates, day.Date)
					}
				}
				assert.Equal(t, []string{tc.date.Format(time.DateOnly)}, dates)
				assert.Positive(t, calendar.Current.Signals.MeetingSignal)
				if tc.newYear {
					previous, err := engine.RelationshipCalendar(t.Context(), query.RelationshipCalendarRequest{CanonicalID: person, Year: tc.date.Year() - 1, Timezone: zone})
					require.NoError(t, err)
					for _, day := range previous.Days {
						assert.Zero(t, day.Meetings)
					}
				}
			}
			require.NoError(t, engine.Close())
			if tc.name == "tokyo" {
				_, err := st.PersistMessage(&store.MessagePersistData{
					Message:      &store.Message{SourceID: source.ID, SourceMessageID: "later", MessageType: "email", SentAt: sql.NullTime{Time: tc.date, Valid: true}, SenderID: sql.NullInt64{Int64: owner, Valid: true}},
					Conversation: &store.ConversationPersistData{SourceConversationID: "later", ConversationType: "email"},
				})
				require.NoError(t, err)
				result, err := buildCache(t.Context(), dbPath, analytics, false)
				require.NoError(t, err)
				assert.False(t, result.IdentityOnly)
				assert.EqualValues(t, 1, count())
			}
		})
	}
}

func TestMeetingCountsDeletedAndSourceScope(t *testing.T) {
	for _, tc := range []struct {
		name    string
		deleted bool
	}{
		{"deleted_event", true},
		{"source_scope", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath, analytics := filepath.Join(dir, "archive.db"), filepath.Join(dir, "analytics")
			st := testutil.NewSQLiteTestStoreAt(t, dbPath)
			source, err := st.GetOrCreateSource("gcal", "owner@example.com")
			require.NoError(t, err)
			notesSource, err := st.GetOrCreateSource("meeting_import", "owner@example.com")
			require.NoError(t, err)
			require.NoError(t, st.AddAccountIdentity(notesSource.ID, "owner@example.com", "manual"))
			require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), source.ID, `{"calendar_id":"owner@example.com","time_zone":"UTC"}`))
			require.NoError(t, st.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
			owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
			require.NoError(t, err)
			person, err := st.EnsureParticipant("person@example.com", "Person", "example.com")
			require.NoError(t, err)
			at := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
			makeMessage := func(sourceID int64, key, typ, format, raw string) int64 {
				t.Helper()
				id, err := st.PersistMessage(&store.MessagePersistData{
					Message:      &store.Message{SourceID: sourceID, SourceMessageID: key, MessageType: typ, SentAt: sql.NullTime{Time: at, Valid: true}, SenderID: sql.NullInt64{Int64: owner, Valid: true}},
					Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting"},
					RawMIME:      []byte(raw), RawFormat: format, Metadata: &sql.NullString{String: `{"calendar_id":"owner@example.com"}`, Valid: true},
				})
				require.NoError(t, err)
				require.NoError(t, st.ReplaceMessageRecipients(id, "from", []int64{owner}, []string{""}))
				require.NoError(t, st.ReplaceMessageRecipients(id, "to", []int64{person}, []string{""}))
				return id
			}
			event := makeMessage(source.ID, "event", "calendar_event", "gcal_json", `{"id":"event","start":{"dateTime":"2024-01-02T10:00:00Z"},"end":{"dateTime":"2024-01-02T11:00:00Z"}}`)
			if tc.deleted {
				_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`, event)
				require.NoError(t, err)
			} else {
				makeMessage(notesSource.ID, "note", "meeting_transcript", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"event"},"transcript":[{"text":"Notes"}]}`)
			}
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
				timeline, err := engine.RelationshipTimeline(t.Context(), query.RelationshipTimelineRequest{CanonicalID: person, Timezone: "UTC"})
				require.NoError(t, err)
				for _, row := range timeline.Rows {
					assert.Greater(t, row.OccurredAt.Year(), 1)
				}
				return detail.MeetingCount
			}
			if tc.deleted {
				assert.Zero(t, count())
				_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET deleted_from_source_at=NULL WHERE id=?`, event)
				require.NoError(t, err)
				assert.True(t, cacheNeedsBuild(dbPath, analytics).NeedsBuild)
				_, err = buildCache(t.Context(), dbPath, analytics, false)
				require.NoError(t, err)
				assert.EqualValues(t, 1, count())
				return
			}
			assert.EqualValues(t, 1, count())
			_, err = st.DB().ExecContext(t.Context(), `UPDATE sources SET sync_config='{"calendar_id":"other@example.com","time_zone":"UTC"}' WHERE id=?`, source.ID)
			require.NoError(t, err)
			assert.True(t, cacheNeedsBuild(dbPath, analytics).NeedsBuild)
			result, err := buildCache(t.Context(), dbPath, analytics, false)
			require.NoError(t, err)
			assert.False(t, result.Skipped)
			assert.EqualValues(t, 2, count())
		})
	}
}
