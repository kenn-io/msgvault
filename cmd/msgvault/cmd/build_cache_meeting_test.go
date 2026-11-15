package cmd

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestBuildCache_IncludesMeetingTranscripts verifies meeting messages and
// their related searchable data are exported to Parquet.
func TestBuildCache_IncludesMeetingTranscripts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	analyticsDir := filepath.Join(tmp, "analytics")

	st := testutil.NewSQLiteTestStoreAt(t, dbPath)

	// An ordinary email: sender alice → recipient carol.
	emailSrc, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	emailConvID, err := st.EnsureConversationWithType(emailSrc.ID, "thread-1", "email_thread", "Hi")
	require.NoError(err)
	aliceID, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err)
	carolID, err := st.EnsureParticipant("carol@example.com", "Carol", "example.com")
	require.NoError(err)
	emailID, err := st.UpsertMessage(&store.Message{
		ConversationID:  emailConvID,
		SourceID:        emailSrc.ID,
		SourceMessageID: "m1",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC), Valid: true},
		SenderID:        sql.NullInt64{Int64: aliceID, Valid: true},
		Subject:         sql.NullString{String: "Hi", Valid: true},
	})
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(emailID, "from", []int64{aliceID}, []string{""}))
	require.NoError(st.ReplaceMessageRecipients(emailID, "to", []int64{carolID}, []string{""}))

	// A meeting transcript: organizer dave, attendee bob.
	meetSrc, err := st.GetOrCreateSource("granola", "dave@example.com")
	require.NoError(err)
	meetConvID, err := st.EnsureConversationWithType(meetSrc.ID, "meeting:not_abc", "meeting", "Planning")
	require.NoError(err)
	daveID, err := st.EnsureParticipant("dave@example.com", "Dave", "example.com")
	require.NoError(err)
	bobID, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err)
	meetingID, err := st.UpsertMessage(&store.Message{
		ConversationID:  meetConvID,
		SourceID:        meetSrc.ID,
		SourceMessageID: "not_abc",
		MessageType:     "meeting_transcript",
		SentAt:          sql.NullTime{Time: time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC), Valid: true},
		SenderID:        sql.NullInt64{Int64: daveID, Valid: true},
		Subject:         sql.NullString{String: "Planning", Valid: true},
	})
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(meetingID, "from", []int64{daveID}, []string{""}))
	require.NoError(st.ReplaceMessageRecipients(meetingID, "to", []int64{bobID}, []string{""}))
	meetingLabelID, err := st.EnsureLabel(t.Context(), meetSrc.ID, "MEETING", "Meeting", "system")
	require.NoError(err)
	require.NoError(st.ReplaceMessageLabels(meetingID, []int64{meetingLabelID}))
	require.NoError(st.UpsertAttachment(t.Context(), meetingID, "transcript.txt", "text/plain", "", "", 42))

	require.NoError(st.Close())

	result, err := buildCache(t.Context(), dbPath, analyticsDir, false)
	require.NoError(err, "buildCache")
	require.False(result.Skipped, "buildCache unexpectedly skipped")

	duckdb, err := sql.Open("duckdb", "")
	require.NoError(err)
	defer func() { _ = duckdb.Close() }()

	// Both messages are searchable from the cache.
	msgPattern := filepath.Join(analyticsDir, "messages", "**", "*.parquet")
	var msgCount int
	require.NoError(duckdb.QueryRowContext(
		t.Context(),
		`SELECT COUNT(*) FROM read_parquet(?, hive_partitioning=true)`, msgPattern).Scan(&msgCount))
	assert.Equal(2, msgCount, "email and meeting transcript should be exported")

	var meetCount int64
	var cachedConversationID sql.NullInt64
	require.NoError(duckdb.QueryRowContext(
		t.Context(),
		`SELECT COUNT(*), MAX(conversation_id) FROM read_parquet(?, hive_partitioning=true) WHERE message_type = 'meeting_transcript'`,
		msgPattern).Scan(&meetCount, &cachedConversationID))
	assert.Equal(int64(1), meetCount, "meeting_transcript row in messages Parquet")
	assert.Equal(sql.NullInt64{Int64: meetConvID, Valid: true}, cachedConversationID, "meeting message keeps its conversation junction")

	// Meeting attendee and email recipient junction rows are both searchable.
	recPattern := filepath.Join(analyticsDir, "message_recipients", "*.parquet")
	var bobRows int
	require.NoError(duckdb.QueryRowContext(
		t.Context(),
		`SELECT COUNT(*) FROM read_parquet(?) WHERE participant_id = ?`, recPattern, bobID).Scan(&bobRows))
	assert.Equal(1, bobRows, "meeting attendee should be exported")

	var carolRows int
	require.NoError(duckdb.QueryRowContext(
		t.Context(),
		`SELECT COUNT(*) FROM read_parquet(?) WHERE participant_id = ?`, recPattern, carolID).Scan(&carolRows))
	assert.Equal(1, carolRows, "email recipient must still be exported")

	var meetingConversationRows int
	require.NoError(duckdb.QueryRowContext(
		t.Context(),
		`SELECT COUNT(*) FROM read_parquet(?) WHERE id = ? AND conversation_type = 'meeting'`,
		filepath.Join(analyticsDir, "conversations", "*.parquet"), meetConvID).Scan(&meetingConversationRows))
	assert.Equal(1, meetingConversationRows, "meeting conversation should be exported")

	var meetingLabelRows int
	require.NoError(duckdb.QueryRowContext(
		t.Context(),
		`SELECT COUNT(*) FROM read_parquet(?) WHERE message_id = ? AND label_id = ?`,
		filepath.Join(analyticsDir, "message_labels", "*.parquet"), meetingID, meetingLabelID).Scan(&meetingLabelRows))
	assert.Equal(1, meetingLabelRows, "meeting label junction should be exported")

	var meetingAttachmentRows int
	require.NoError(duckdb.QueryRowContext(
		t.Context(),
		`SELECT COUNT(*) FROM read_parquet(?) WHERE message_id = ?`,
		filepath.Join(analyticsDir, "attachments", "*.parquet"), meetingID).Scan(&meetingAttachmentRows))
	assert.Equal(1, meetingAttachmentRows, "meeting attachment junction should be exported")
}

func TestBuildCacheMeetingCountsFollowSavedChoices(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.db")
	analytics := filepath.Join(dir, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	source, err := st.GetOrCreateSource("meeting_import", "owner@example.test")
	requirements.NoError(err)
	alice, err := st.EnsureParticipant("alice@example.test", "Alice", "example.test")
	requirements.NoError(err)
	bob, err := st.EnsureParticipant("bob@example.test", "Bob", "example.test")
	requirements.NoError(err)
	var refs []store.MeetingLinkRef
	for _, key := range []string{"first", "second"} {
		id, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: key, MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting"},
			RawMIME:      []byte(`{}`), RawFormat: "meeting_json",
		})
		requirements.NoError(err)
		requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{alice, bob}, []string{"", ""}))
		refs = append(refs, store.MeetingLinkRef{Kind: "msg", MessageID: id})
	}
	check := func(want, activity int64) {
		t.Helper()
		_, err := buildCache(t.Context(), path, analytics, false)
		requirements.NoError(err)
		engine, err := query.NewDuckDBEngine(t.Context(), analytics, path, st.DB())
		requirements.NoError(err)
		defer func() { _ = engine.Close() }()
		for _, name := range []string{"Alice", "Bob"} {
			after := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
			for _, filter := range []query.Context{{}, {SourceIDs: []int64{source.ID}}, {After: &after}} {
				people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{Query: name, Explore: query.ExploreRequest{Context: filter}})
				requirements.NoError(err)
				requirements.Len(people.Rows, 1)
				assertions.Equal(want, people.Rows[0].MeetingCount)
				assertions.Equal(activity, people.Rows[0].ActivityCount)
			}
		}
		selection, err := engine.ResolveExploreMeetings(t.Context(), query.ExploreSelectionRequest{}, 10)
		requirements.NoError(err)
		assertions.Equal(want, selection.MeetingCount)
		assertions.Len(selection.MessageIDs, int(activity))
	}
	check(2, 2)
	_, err = st.DecideMeetingLinkContext(t.Context(), refs[0], refs[1], "link")
	requirements.NoError(err)
	check(1, 2)
	_, err = st.DecideMeetingLinkContext(t.Context(), refs[0], refs[1], "unlink")
	requirements.NoError(err)
	check(2, 2)
	third, err := st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "third", MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC), Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "third", ConversationType: "meeting"},
		RawMIME:      []byte(`{}`), RawFormat: "meeting_json",
	})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(third, "to", []int64{alice, bob}, []string{"", ""}))
	_, err = st.DecideMeetingLinkContext(t.Context(), refs[0], store.MeetingLinkRef{Kind: "msg", MessageID: third}, "link")
	requirements.NoError(err)
	check(2, 3)
}

func TestBuildCacheMeetingCountsAcrossWeeklyOccurrences(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.db")
	analytics := filepath.Join(dir, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	calendar, err := st.GetOrCreateSource("gcal", "owner@example.test")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(t.Context(), calendar.ID, `{"calendar_id":"owner@example.test","time_zone":"UTC"}`))
	participant, err := st.EnsureParticipant("alice@example.test", "Alice", "example.test")
	requirements.NoError(err)
	start := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	event, err := st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: calendar.ID, SourceMessageID: "series", MessageType: "calendar_event", SentAt: sql.NullTime{Time: start, Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "series", ConversationType: "calendar"},
		RawMIME:      []byte(`{"id":"series","iCalUID":"weekly@example.test","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`), RawFormat: "gcal_json",
		Metadata: &sql.NullString{String: `{"calendar_id":"owner@example.test"}`, Valid: true},
	})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(event, "to", []int64{participant}, []string{""}))
	notes, err := st.GetOrCreateSource("meeting_import", "owner@example.test")
	requirements.NoError(err)
	for _, entry := range []struct{ key, format, raw string }{
		{"first", "granola_json", `{"owner":{"email":"owner@example.test"},"calendar_event":{"calendar_event_id":"series_20270314T100000Z"}}`},
		{"copy", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.test","startsAt":"2027-03-14T10:00:00Z"}}`},
		{"next", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.test","startsAt":"2027-03-21T10:00:00Z"}}`},
	} {
		id, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: notes.ID, SourceMessageID: entry.key, MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: start, Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: entry.key, ConversationType: "meeting"},
			RawMIME:      []byte(entry.raw), RawFormat: entry.format,
		})
		requirements.NoError(err)
		requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{participant}, []string{""}))
	}
	_, err = buildCache(t.Context(), path, analytics, true)
	requirements.NoError(err)
	engine, err := query.NewDuckDBEngine(t.Context(), analytics, path, st.DB())
	requirements.NoError(err)
	t.Cleanup(func() { _ = engine.Close() })
	people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{Query: "Alice"})
	requirements.NoError(err)
	requirements.Len(people.Rows, 1)
	assertions.Equal(int64(2), people.Rows[0].MeetingCount)
	assertions.Equal(int64(4), people.Rows[0].ActivityCount)
	calendarOnly, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{Query: "Alice", Explore: query.ExploreRequest{Context: query.Context{SourceIDs: []int64{calendar.ID}}}})
	requirements.NoError(err)
	requirements.Len(calendarOnly.Rows, 1)
	assertions.Zero(calendarOnly.Rows[0].MeetingCount)
	requirements.NoError(engine.Close())
	copyID, err := st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: notes.ID, SourceMessageID: "later-copy", MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: start, Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "later-copy", ConversationType: "meeting"},
		RawMIME:      []byte(`{"owner":{"email":"owner@example.test"},"calendar_event":{"calendar_event_id":"series_20270314T100000Z"}}`), RawFormat: "granola_json",
	})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(copyID, "to", []int64{participant}, []string{""}))
	_, err = buildCache(t.Context(), path, analytics, false)
	requirements.NoError(err)
	updated, err := query.NewDuckDBEngine(t.Context(), analytics, path, st.DB())
	requirements.NoError(err)
	t.Cleanup(func() { _ = updated.Close() })
	people, err = updated.SearchPeople(t.Context(), query.PersonSearchRequest{Query: "Alice"})
	requirements.NoError(err)
	requirements.Len(people.Rows, 1)
	assertions.Equal(int64(2), people.Rows[0].MeetingCount)
	assertions.Equal(int64(5), people.Rows[0].ActivityCount)
}
