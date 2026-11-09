package cmd

import (
	"database/sql"
	"encoding/json/v2"
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

type meetingCountSeriesFixture struct {
	st                    *store.Store
	path, cache           string
	source, person, event int64
	notes                 []int64
}

func newMeetingCountSeriesFixture(t *testing.T, copies int) meetingCountSeriesFixture {
	t.Helper()
	root := t.TempDir()
	f := meetingCountSeriesFixture{path: filepath.Join(root, "archive.db"), cache: filepath.Join(root, "analytics")}
	f.st = testutil.NewSQLiteTestStoreAt(t, f.path)
	source, err := f.st.GetOrCreateSource("meeting_import", "owner@example.org")
	require.NoError(t, err)
	f.source = source.ID
	owner, err := f.st.EnsureParticipant("owner@example.org", "Owner", "example.org")
	require.NoError(t, err)
	require.NoError(t, f.st.AddAccountIdentity(source.ID, "owner@example.org", "explicit"))
	f.person, err = f.st.EnsureParticipant("person@example.org", "Person", "example.org")
	require.NoError(t, err)
	calendarSource := meetingCountCalendarSource(t, f.st, "calendar@example.org")
	raw := `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2026-03-14T10:00:00Z"},"end":{"dateTime":"2026-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`
	f.event, err = f.st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: calendarSource, SourceMessageID: "series", MessageType: "calendar_event", SentAt: sql.NullTime{Time: time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "series", ConversationType: "calendar"},
		RawFormat:    "gcal_json", RawMIME: []byte(raw), Metadata: meetingCountCalendarMetadata(t, "calendar@example.org", raw),
	})
	require.NoError(t, err)
	for i := range copies {
		key := fmt.Sprintf("note-%d", i)
		note, err := f.st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: key, MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: time.Date(2026, 3, 28, 10, 0, 0, 0, time.UTC), Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting"},
			RawFormat:    "plaud_json", RawMIME: []byte(`{}`),
			Recipients: []store.RecipientSet{{Type: "to", ParticipantIDs: []int64{owner, f.person}, DisplayNames: []string{"Owner", "Person"}}},
		})
		require.NoError(t, err)
		f.notes = append(f.notes, note)
	}
	return f
}

func meetingCountCalendarSource(t *testing.T, st *store.Store, calendar string) int64 {
	t.Helper()
	source, err := st.GetOrCreateSource("gcal", calendar)
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(source.ID, fmt.Sprintf(`{"calendar_id":%q,"time_zone":"UTC"}`, calendar)))
	return source.ID
}

func meetingCountCalendarMetadata(t *testing.T, calendar, raw string) *sql.NullString {
	t.Helper()
	var event struct {
		ICalUID string `json:"iCalUID"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &event))
	metadata := map[string]string{"calendar_id": calendar}
	if event.ICalUID != "" {
		metadata["ical_uid"] = event.ICalUID
	}
	encoded, err := json.Marshal(metadata)
	require.NoError(t, err)
	return &sql.NullString{String: string(encoded), Valid: true}
}

func TestMeetingCountsLinkedCopies(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st, err := store.OpenForTest(path)
	requirements.NoError(err)
	requirements.NoError(st.InitSchema())
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	human, err := st.EnsureParticipant("human@example.org", "Example Human", "example.org")
	requirements.NoError(err)
	owners := make(map[string]int64)
	sources := make(map[string]int64)
	for _, identifier := range []string{"first", "second", "calendar"} {
		kind := "meeting_import"
		if identifier == "calendar" {
			kind = "gcal"
		}
		source, err := st.GetOrCreateSource(kind, identifier+"@example.org")
		requirements.NoError(err)
		sources[identifier] = source.ID
		if kind == "gcal" {
			sources[identifier] = meetingCountCalendarSource(t, st, identifier+"@example.org")
		}
		owners[identifier], err = st.EnsureParticipant(identifier+"@example.org", "Owner", "example.org")
		requirements.NoError(err)
		requirements.NoError(st.AddAccountIdentity(source.ID, identifier+"@example.org", "explicit"))
	}
	persist := func(kind, identifier, format, raw string) int64 {
		at := time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)
		if identifier == "second" {
			at = at.Add(24 * time.Hour)
		}
		var metadata *sql.NullString
		conversation := "meeting"
		if kind == "calendar_event" {
			metadata = meetingCountCalendarMetadata(t, identifier+"@example.org", raw)
			conversation = "calendar"
		}
		id, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: sources[identifier], SourceMessageID: identifier, MessageType: kind, SentAt: sql.NullTime{Time: at, Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: identifier, ConversationType: conversation, Title: "Planning"},
			RawMIME:      []byte(raw), RawFormat: format, Metadata: metadata,
		})
		requirements.NoError(err)
		requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{human, owners[identifier]}, []string{"Example Human", "Owner"}))
		return id
	}
	first := persist("meeting_transcript", "first", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"planning@example.org"}}`)
	second := persist("meeting_transcript", "second", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"planning@example.org"}}`)
	cache := filepath.Join(t.TempDir(), "analytics")
	var preservedShards map[string]string
	readKeys := func(wantIDs ...int64) {
		want := make(map[int64][]string)
		for _, id := range wantIDs {
			want[id] = []string{"uid:planning@example.org/"}
		}
		assertions.Equal(want, readCachedMeetingKeys(t, cache))
		if preservedShards != nil {
			assertions.Equal(preservedShards, snapshotMessagesDatasetBytes(t, cache))
		}
	}
	_, err = buildCache(path, cache, true)
	requirements.NoError(err)
	readKeys()
	event := persist("calendar_event", "calendar", "gcal_json", `{"id":"calendar","iCalUID":"planning@example.org","start":{"dateTime":"2026-03-14T10:00:00Z"},"end":{"dateTime":"2026-03-14T11:00:00Z"}}`)
	assertions.False(cacheNeedsBuild(path, cache).FullRebuild)
	incremental, err := buildCache(path, cache, false)
	requirements.NoError(err)
	assertions.Equal(int64(1), incremental.StagedCount)
	readKeys(first, second)
	// A re-import changes automatic evidence while keeping the message ID.
	for _, tc := range []struct {
		uid     string
		members []int64
	}{
		{"unmatched@example.org", []int64{first}}, {"planning@example.org", []int64{first, second}},
	} {
		requirements.NoError(st.UpsertMessageRawWithFormat(second, []byte(fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q}}`, tc.uid)), "twenty_json"))
		assertions.False(cacheNeedsBuild(path, cache).FullRebuild)
		_, err := buildCache(path, cache, false)
		requirements.NoError(err)
		readKeys(tc.members...)
	}
	preservedShards = snapshotMessagesDatasetBytes(t, cache)
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), second, store.OccurrenceRef{MessageID: event}))
	assertions.False(cacheNeedsBuild(path, cache).FullRebuild)
	_, err = buildCache(path, cache, false)
	requirements.NoError(err)
	readKeys(first)
	requirements.NoError(st.LinkMeetingContext(t.Context(), second, store.OccurrenceRef{MessageID: event}))
	_, err = buildCache(path, cache, false)
	requirements.NoError(err)
	readKeys(first, second)
	preservedShards = nil
	_, err = buildCache(path, cache, true)
	requirements.NoError(err)
	readKeys(first, second)
}

func TestMeetingKeysAckSnapshotBoundary(t *testing.T) {
	require := require.New(t)
	assertions := assert.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st, err := store.OpenForTest(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("meeting_import", "notes@example.test")
	require.NoError(err)
	calendarSource := meetingCountCalendarSource(t, st, "calendar@example.test")
	persist := func(id, kind, format, raw string) int64 {
		sourceID, conversation := source.ID, "meeting"
		var metadata *sql.NullString
		if kind == "calendar_event" {
			sourceID, conversation = calendarSource, "calendar"
			metadata = meetingCountCalendarMetadata(t, "calendar@example.test", raw)
		}
		messageID, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: sourceID, SourceMessageID: id, MessageType: kind, SentAt: sql.NullTime{Time: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: id, ConversationType: conversation},
			RawFormat:    format, RawMIME: []byte(raw), Metadata: metadata,
		})
		require.NoError(err)
		return messageID
	}
	event := persist("event", "calendar_event", "gcal_json", `{"id":"event","start":{"dateTime":"2026-08-01T10:00:00Z"},"end":{"dateTime":"2026-08-01T11:00:00Z"}}`)
	other := persist("other", "calendar_event", "gcal_json", `{"id":"other","start":{"dateTime":"2026-08-02T10:00:00Z"},"end":{"dateTime":"2026-08-02T11:00:00Z"}}`)
	note := persist("note", "meeting_transcript", "plaud_json", `{}`)
	var decisionSeq, late int64
	buildCacheAfterSnapshotHook = func() {
		require.NoError(st.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: event}))
		require.NoError(st.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: other}))
		late = persist("late", "meeting_transcript", "plaud_json", `{}`)
		require.NoError(st.LinkMeetingContext(t.Context(), late, store.OccurrenceRef{MessageID: event}))
		require.NoError(st.DB().QueryRow(`SELECT max(seq) FROM cache_related_change_journal`).Scan(&decisionSeq))
	}
	t.Cleanup(func() { buildCacheAfterSnapshotHook = nil })
	cache := filepath.Join(t.TempDir(), "analytics")
	_, err = buildCache(path, cache, true)
	require.NoError(err)
	buildCacheAfterSnapshotHook = nil
	state, err := query.ReadCacheSyncState(cache)
	require.NoError(err)
	assertions.Less(state.LastRelatedChangeSeq, decisionSeq)
	assertions.True(cacheNeedsBuild(path, cache).HasRelatedRowDrift)
	db, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { require.NoError(db.Close()) })
	var count int
	require.NoError(db.QueryRow(`SELECT count(*) FROM read_parquet(?) WHERE message_id = ?`, filepath.Join(cache, "meeting_keys", "*.parquet"), note).Scan(&count))
	assertions.Equal(2, count, "the later key read includes both decisions")
	require.NoError(db.QueryRow(`SELECT count(*) FROM read_parquet(?) WHERE message_id = ?`, filepath.Join(cache, "meeting_keys", "*.parquet"), late).Scan(&count))
	assertions.Zero(count, "keys exclude messages above the source snapshot boundary")
	result, err := buildCache(path, cache, false)
	require.NoError(err)
	assertions.Equal(int64(1), result.StagedCount)
	state, err = query.ReadCacheSyncState(cache)
	require.NoError(err)
	assertions.Equal(decisionSeq, state.LastRelatedChangeSeq)
	assertions.False(cacheNeedsBuild(path, cache).NeedsBuild)
}

func readCachedMeetingKeys(t *testing.T, cache string) map[int64][]string {
	t.Helper()
	requirements := require.New(t)
	db, err := sql.Open("duckdb", "")
	requirements.NoError(err)
	defer func() { requirements.NoError(db.Close()) }()
	rows, err := db.Query(`SELECT message_id, meeting_key FROM read_parquet(?)`, filepath.Join(cache, "meeting_keys", "*.parquet"))
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()
	keys := make(map[int64][]string)
	for rows.Next() {
		var id int64
		var key string
		requirements.NoError(rows.Scan(&id, &key))
		keys[id] = append(keys[id], key)
	}
	requirements.NoError(rows.Err())
	return keys
}

func TestMeetingCountsAutomaticRemovedOccurrence(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	f := newMeetingCountSeriesFixture(t, 2)
	require.NoError(f.st.UpsertMessageRawWithFormat(f.notes[0], []byte(`{"owner":{"email":"calendar@example.org"},"calendar_event":{"calendar_event_id":"series_20260321T100000Z","scheduled_start_time":"2026-03-21T10:00:00Z"}}`), "granola_json"))
	require.NoError(f.st.UpsertMessageRawWithFormat(f.notes[1], []byte(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2026-03-21T10:00:00Z"}}`), "twenty_json"))
	_, err := buildCache(f.path, f.cache, true)
	require.NoError(err)
	want := map[int64][]string{f.notes[0]: {"uid:weekly@example.org/20260321T100000Z"}, f.notes[1]: {"uid:weekly@example.org/20260321T100000Z"}}
	assert.Equal(want, readCachedMeetingKeys(t, f.cache))
	require.NoError(f.st.UpsertMessageRawWithFormat(f.event, []byte(`{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2026-03-14T10:00:00Z"},"end":{"dateTime":"2026-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=1"]}`), "gcal_json"))
	_, err = buildCache(f.path, f.cache, false)
	require.NoError(err)
	assert.Equal(want, readCachedMeetingKeys(t, f.cache))
	require.NoError(f.st.UpsertMessageRawWithFormat(f.notes[1], []byte(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"different@example.org","startsAt":"2026-03-21T10:00:00Z"}}`), "twenty_json"))
	_, err = buildCache(f.path, f.cache, false)
	require.NoError(err)
	assert.Equal(map[int64][]string{f.notes[0]: {"uid:weekly@example.org/20260321T100000Z"}}, readCachedMeetingKeys(t, f.cache))
}
