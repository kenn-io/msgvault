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

type meetingCountArchive struct {
	st              *store.Store
	path, cache     string
	at              time.Time
	sources, owners map[string]int64
	person          int64
}

func newMeetingCountArchive(t *testing.T, at time.Time) meetingCountArchive {
	t.Helper()
	root := t.TempDir()
	f := meetingCountArchive{path: filepath.Join(root, "archive.db"), cache: filepath.Join(root, "analytics"), at: at, sources: make(map[string]int64), owners: make(map[string]int64)}
	f.st = testutil.NewSQLiteTestStoreAt(t, f.path)
	var err error
	f.person, err = f.st.EnsureParticipant("human@example.org", "Example Human", "example.org")
	require.NoError(t, err)
	for _, identifier := range []string{"first", "second", "calendar"} {
		kind := "meeting_import"
		if identifier == "calendar" {
			kind = "gcal"
		}
		email := identifier + "@example.org"
		source, err := f.st.GetOrCreateSource(kind, email)
		require.NoError(t, err)
		f.sources[identifier] = source.ID
		if kind == "gcal" {
			require.NoError(t, f.st.UpdateSourceSyncConfig(source.ID, fmt.Sprintf(`{"calendar_id":%q,"time_zone":"UTC"}`, email)))
		}
		f.owners[identifier], err = f.st.EnsureParticipant(email, "Owner", "example.org")
		require.NoError(t, err)
		require.NoError(t, f.st.AddAccountIdentity(source.ID, email, "explicit"))
	}
	return f
}

func (f meetingCountArchive) persist(t *testing.T, source, id, format, raw string) int64 {
	t.Helper()
	kind, conversation := "meeting_transcript", "meeting"
	var metadata *sql.NullString
	if source == "calendar" {
		kind, conversation = "calendar_event", "calendar"
		var event struct {
			ICalUID string `json:"iCalUID"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &event))
		fields := map[string]string{"calendar_id": "calendar@example.org"}
		if event.ICalUID != "" {
			fields["ical_uid"] = event.ICalUID
		}
		encoded, err := json.Marshal(fields)
		require.NoError(t, err)
		metadata = &sql.NullString{String: string(encoded), Valid: true}
	}
	at := f.at
	if source == "second" {
		at = at.Add(24 * time.Hour)
	}
	messageID, err := f.st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: f.sources[source], SourceMessageID: id, MessageType: kind, SentAt: sql.NullTime{Time: at, Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: id, ConversationType: conversation},
		RawMIME:      []byte(raw), RawFormat: format, Metadata: metadata,
		Recipients: []store.RecipientSet{{Type: "to", ParticipantIDs: []int64{f.person, f.owners[source]}, DisplayNames: []string{"Example Human", "Owner"}}},
	})
	require.NoError(t, err)
	return messageID
}

func TestMeetingCountsLinkedCopies(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newMeetingCountArchive(t, time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC))
	st, path, cache := f.st, f.path, f.cache
	first := f.persist(t, "first", "first", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"planning@example.org"}}`)
	second := f.persist(t, "second", "second", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"planning@example.org"}}`)
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
	_, err := buildCache(path, cache, true)
	requirements.NoError(err)
	readKeys()
	event := f.persist(t, "calendar", "calendar", "gcal_json", `{"id":"calendar","iCalUID":"planning@example.org","start":{"dateTime":"2026-03-14T10:00:00Z"},"end":{"dateTime":"2026-03-14T11:00:00Z"}}`)
	assertions.False(cacheNeedsBuild(path, cache).FullRebuild)
	incremental, err := buildCache(path, cache, false)
	requirements.NoError(err)
	assertions.Equal(int64(1), incremental.StagedCount)
	readKeys(first, second)
	preservedShards = snapshotMessagesDatasetBytes(t, cache)
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), second, store.OccurrenceRef{MessageID: event}))
	assertions.False(cacheNeedsBuild(path, cache).FullRebuild)
	_, err = buildCache(path, cache, false)
	requirements.NoError(err)
	readKeys(first)
	preservedShards = nil
	_, err = buildCache(path, cache, true)
	requirements.NoError(err)
	readKeys(first)

	t.Run("recurrence removed", func(t *testing.T) {
		require, assert := require.New(t), assert.New(t)
		f := newMeetingCountArchive(t, time.Date(2027, 3, 7, 10, 0, 0, 0, time.UTC))
		series := f.persist(t, "calendar", "series", "gcal_json", `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T10:00:00Z"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`)
		manual := f.persist(t, "first", "manual", "plaud_json", `{}`)
		automatic := f.persist(t, "second", "automatic", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-07T10:00:00Z"}}`)
		granola := f.persist(t, "first", "granola", "granola_json", `{"owner":{"email":"other@example.org"},"calendar_event":{"calendar_event_id":"series_20270307T100000Z","scheduled_start_time":"2027-03-07T10:00:00Z"}}`)
		require.NoError(f.st.LinkMeetingContext(t.Context(), manual, store.OccurrenceRef{MessageID: series, Key: "20270307T100000Z"}))
		_, err := buildCache(f.path, f.cache, true)
		require.NoError(err)
		require.NoError(f.st.UpsertMessageRawWithFormat(series, []byte(`{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T10:00:00Z"},"end":{"dateTime":"2027-03-07T11:00:00Z"}}`), "gcal_json"))
		assert.False(cacheNeedsBuild(f.path, f.cache).FullRebuild)
		_, err = buildCache(f.path, f.cache, false)
		require.NoError(err)
		assert.Equal(map[int64][]string{manual: {"uid:weekly@example.org/"}, automatic: {"uid:weekly@example.org/"}, granola: {"uid:weekly@example.org/"}}, readCachedMeetingKeys(t, f.cache))
	})
}

func TestMeetingKeysAckSnapshotBoundary(t *testing.T) {
	require := require.New(t)
	assertions := assert.New(t)
	f := newMeetingCountArchive(t, time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC))
	st, path, cache := f.st, f.path, f.cache
	event := f.persist(t, "calendar", "event", "gcal_json", `{"id":"event","start":{"dateTime":"2026-08-01T10:00:00Z"},"end":{"dateTime":"2026-08-01T11:00:00Z"}}`)
	other := f.persist(t, "calendar", "other", "gcal_json", `{"id":"other","start":{"dateTime":"2026-08-02T10:00:00Z"},"end":{"dateTime":"2026-08-02T11:00:00Z"}}`)
	note := f.persist(t, "first", "note", "plaud_json", `{}`)
	var decisionSeq, late int64
	buildCacheAfterSnapshotHook = func() {
		require.NoError(st.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: event}))
		require.NoError(st.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: other}))
		late = f.persist(t, "first", "late", "plaud_json", `{}`)
		require.NoError(st.LinkMeetingContext(t.Context(), late, store.OccurrenceRef{MessageID: event}))
		require.NoError(st.DB().QueryRow(`SELECT max(seq) FROM cache_related_change_journal`).Scan(&decisionSeq))
	}
	t.Cleanup(func() { buildCacheAfterSnapshotHook = nil })
	_, err := buildCache(path, cache, true)
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
