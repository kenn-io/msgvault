package cmd

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"
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

}

func TestMeetingCountCalendarRefresh(t *testing.T) {
	for _, kind := range []string{"occurrence dates", "all day", "time", "uid", "event zone", "calendar zone", "recurrence"} {
		t.Run(kind, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newMeetingCountArchive(t, time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC))
			raw := `{"id":"event","iCalUID":"weekly@example.org","start":{"dateTime":"2026-03-07T10:00:00Z"},"end":{"dateTime":"2026-03-07T11:00:00Z"}}`
			if kind == "occurrence dates" || kind == "recurrence" {
				raw = strings.TrimSuffix(raw, "}") + `,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`
			}
			if kind == "all day" {
				raw = `{"id":"event","iCalUID":"weekly@example.org","start":{"date":"2026-03-07"},"end":{"date":"2026-03-08"},"recurrence":["RRULE:FREQ=DAILY;COUNT=2"]}`
			}
			if kind == "event zone" {
				raw = strings.ReplaceAll(raw, `"dateTime":`, `"timeZone":"UTC","dateTime":`)
			}
			event := f.persist(t, "calendar", "event", "gcal_json", raw)
			noteFormat, noteRaw := "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2026-03-07T10:00:00Z"}}`
			if kind == "all day" {
				noteFormat, noteRaw = "plaud_json", `{}`
			}
			note := f.persist(t, "first", "note", noteFormat, noteRaw)
			if kind == "occurrence dates" || kind == "all day" {
				keys := []string{"20260307T100000Z", "20260314T100000Z"}
				if kind == "all day" {
					keys = []string{"20260308"}
				}
				for _, key := range keys {
					requirements.NoError(f.st.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: event, Key: key}))
				}
			}
			_, err := buildCache(f.path, f.cache, true)
			requirements.NoError(err)
			var before int64
			requirements.NoError(f.st.DB().QueryRow(`SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&before))
			switch kind {
			case "time":
				requirements.NoError(f.st.UpsertMessageRawWithFormat(event, []byte(strings.ReplaceAll(raw, "2026-03-07T", "2026-03-08T")), "gcal_json"))
			case "uid":
				requirements.NoError(f.st.SetMessageMetadataContext(t.Context(), event, sql.NullString{String: `{"calendar_id":"calendar@example.org","ical_uid":"second@example.org"}`, Valid: true}))
			case "event zone":
				requirements.NoError(f.st.UpsertMessageRawWithFormat(event, []byte(strings.ReplaceAll(raw, `"timeZone":"UTC"`, `"timeZone":"America/New_York"`)), "gcal_json"))
			case "calendar zone":
				requirements.NoError(f.st.UpdateSourceSyncConfig(f.sources["calendar"], `{"calendar_id":"calendar@example.org","time_zone":"America/New_York"}`))
			case "recurrence":
				requirements.NoError(f.st.UpsertMessageRawWithFormat(event, []byte(strings.Replace(raw, "COUNT=2", "COUNT=3", 1)), "gcal_json"))
			}
			if kind != "occurrence dates" && kind != "all day" {
				var after int64
				requirements.NoError(f.st.DB().QueryRow(`SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&after))
				assertions.Greater(after, before)
				_, err = buildCache(f.path, f.cache, false)
				requirements.NoError(err)
			}
			if kind == "uid" {
				assertions.Empty(readCachedMeetingKeys(t, f.cache)[note])
				return
			}
			db, err := sql.Open("duckdb", "")
			requirements.NoError(err)
			t.Cleanup(func() { requirements.NoError(db.Close()) })
			rows, err := db.Query(`SELECT occurrence_at, occurrence_date FROM read_parquet(?) WHERE message_id=? ORDER BY occurrence_at`, filepath.Join(f.cache, "meeting_keys", "*.parquet"), note)
			requirements.NoError(err)
			t.Cleanup(func() { requirements.NoError(rows.Close()) })
			var times []time.Time
			var dates []string
			for rows.Next() {
				var at, date sql.NullTime
				requirements.NoError(rows.Scan(&at, &date))
				if at.Valid {
					times = append(times, at.Time.UTC())
				}
				if date.Valid {
					dates = append(dates, date.Time.Format("2006-01-02"))
				}
			}
			requirements.NoError(rows.Err())
			switch kind {
			case "occurrence dates":
				assertions.Equal([]time.Time{time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC), time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)}, times)
			case "all day":
				assertions.Equal([]string{"2026-03-08"}, dates)
			case "time":
				assertions.Equal([]time.Time{time.Date(2026, 3, 8, 10, 0, 0, 0, time.UTC)}, times)
			}
		})
	}
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

func TestMeetingCountBuildWithManyCalendarCopies(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newMeetingCountArchive(t, time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC))
	for _, id := range []string{"first-note", "second-note"} {
		f.persist(t, "first", id, "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"crowded@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	}
	for i := range 201 {
		id := fmt.Sprintf("event-%d", i)
		f.persist(t, "calendar", id, "gcal_json", fmt.Sprintf(`{"id":%q,"iCalUID":"crowded@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, id))
	}
	_, err := buildCache(f.path, f.cache, true)
	requirements.NoError(err)
	count := func() int64 {
		engine, err := query.NewDuckDBEngine(f.cache, f.path, f.st.DB())
		requirements.NoError(err)
		defer func() { requirements.NoError(engine.Close()) }()
		person, err := engine.GetPerson(t.Context(), f.person, query.Context{}, nil)
		requirements.NoError(err)
		requirements.NotNil(person)
		return person.MeetingCount
	}
	assertions.Equal(int64(1), count())
	f.persist(t, "first", "third-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"crowded@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	_, err = buildCache(f.path, f.cache, false)
	requirements.NoError(err)
	assertions.Equal(int64(1), count())
}

func TestMeetingCountsKeepSourceEvidenceWithSavedLink(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newMeetingCountArchive(t, time.Date(2027, 3, 7, 10, 0, 0, 0, time.UTC))
	raw := `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T10:00:00Z"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`
	event := f.persist(t, "calendar", "series", "gcal_json", raw)
	note := f.persist(t, "first", "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-21T10:00:00Z"}}`)
	first := store.OccurrenceRef{MessageID: event, Key: "20270307T100000Z"}
	second := store.OccurrenceRef{MessageID: event, Key: "20270321T100000Z"}
	requirements.NoError(f.st.LinkMeetingContext(t.Context(), note, first))
	requirements.NoError(f.st.UpsertMessageRawWithFormat(event, []byte(strings.Replace(raw, "COUNT=6", "COUNT=1", 1)), "gcal_json"))
	_, err := buildCache(f.path, f.cache, true)
	requirements.NoError(err)
	count := func() int64 {
		engine, err := query.NewDuckDBEngine(f.cache, f.path, f.st.DB())
		requirements.NoError(err)
		defer func() { requirements.NoError(engine.Close()) }()
		person, err := engine.GetPerson(t.Context(), f.person, query.Context{}, nil)
		requirements.NoError(err)
		requirements.NotNil(person)
		return person.MeetingCount
	}
	assertions.Equal(int64(2), count())
	requirements.NoError(f.st.UpsertMessageRawWithFormat(event, []byte(raw), "gcal_json"))
	requirements.NoError(f.st.LinkMeetingContext(t.Context(), note, second))
	requirements.NoError(f.st.UpsertMessageRawWithFormat(event, []byte(strings.Replace(raw, "COUNT=6", "COUNT=1", 1)), "gcal_json"))
	requirements.NoError(f.st.UnlinkMeetingContext(t.Context(), note, second))
	_, err = buildCache(f.path, f.cache, false)
	requirements.NoError(err)
	assertions.Equal(int64(1), count())
}

func TestMeetingCountsRefreshAfterEvidenceChange(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newMeetingCountArchive(t, time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC))
	raw := `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"first@example.org","startsAt":"2027-03-14T10:00:00Z"},"summary":"one"}`
	f.persist(t, "first", "first-note", "twenty_json", raw)
	note := f.persist(t, "first", "second-note", "twenty_json", raw)
	_, err := buildCache(f.path, f.cache, true)
	requirements.NoError(err)
	var seq, after int64
	requirements.NoError(f.st.DB().QueryRow(`SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&seq))
	requirements.NoError(f.st.UpsertMessageRawWithFormat(note, []byte(strings.Replace(raw, `"summary":"one"`, `"summary":"two"`, 1)), "twenty_json"))
	requirements.NoError(f.st.DB().QueryRow(`SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&after))
	assertions.Equal(seq, after)
	requirements.NoError(f.st.UpsertMessageRawWithFormat(note, []byte(strings.Replace(raw, "first@example.org", "second@example.org", 1)), "twenty_json"))
	requirements.NoError(f.st.DB().QueryRow(`SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal WHERE dataset='meeting_links'`).Scan(&after))
	assertions.Greater(after, seq)
	_, err = buildCache(f.path, f.cache, false)
	requirements.NoError(err)
	engine, err := query.NewDuckDBEngine(f.cache, f.path, f.st.DB())
	requirements.NoError(err)
	defer func() { requirements.NoError(engine.Close()) }()
	person, err := engine.GetPerson(t.Context(), f.person, query.Context{}, nil)
	requirements.NoError(err)
	requirements.NotNil(person)
	assertions.Equal(int64(2), person.MeetingCount)
}
