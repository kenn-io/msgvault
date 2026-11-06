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
)

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
	for _, csv := range []bool{false, true} {
		t.Run(fmt.Sprintf("csv_%t", csv), func(t *testing.T) {
			t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "")
			if csv {
				t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "1")
			}
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
		})
	}
}

func TestMeetingCountsCalendarEdits(t *testing.T) {
	const eventRaw = `{"id":"series","iCalUID":%q,"start":{"dateTime":"2027-03-14T10:00:00","timeZone":%q},"end":{"dateTime":"2027-03-14T11:00:00","timeZone":%q},"recurrence":%s}`
	original := fmt.Sprintf(eventRaw, "series@example.test", "UTC", "UTC", `["RRULE:FREQ=WEEKLY;COUNT=3"]`)
	excluded := fmt.Sprintf(eventRaw, "series@example.test", "UTC", "UTC", `["RRULE:FREQ=WEEKLY;COUNT=3","EXDATE:20270321T100000Z"]`)
	zoned := fmt.Sprintf(eventRaw, "series@example.test", "America/New_York", "America/New_York", `["RRULE:FREQ=WEEKLY;COUNT=3"]`)
	for _, step := range []struct {
		name, beforeRaw, raw, wantKey string
	}{
		{"baseline", "", original, "uid:series@example.test/20270321T100000Z"},
		{"projection removed", "", `{}`, ""},
		{"restored after projection removal", `{}`, original, "uid:series@example.test/20270321T100000Z"},
		{"EXDATE", "", excluded, ""},
		{"restored after EXDATE", excluded, original, "uid:series@example.test/20270321T100000Z"},
		{"zone", "", zoned, ""},
		{"restored after zone", zoned, original, "uid:series@example.test/20270321T100000Z"},
		{"UID", "", fmt.Sprintf(eventRaw, "different@example.test", "UTC", "UTC", `["RRULE:FREQ=WEEKLY;COUNT=3"]`), ""},
	} {
		t.Run(step.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			path := filepath.Join(t.TempDir(), "archive.db")
			st, err := store.OpenForTest(path)
			requirements.NoError(err)
			t.Cleanup(func() { requirements.NoError(st.Close()) })
			requirements.NoError(st.InitSchema())
			calendarSource := meetingCountCalendarSource(t, st, "calendar@example.test")
			source, err := st.GetOrCreateSource("twenty", "owner@example.test")
			requirements.NoError(err)
			person, err := st.EnsureParticipant("person@example.test", "Person", "example.test")
			requirements.NoError(err)
			event, err := st.PersistMessage(&store.MessagePersistData{
				Message:      &store.Message{SourceID: calendarSource, SourceMessageID: "series", MessageType: "calendar_event", SentAt: sql.NullTime{Time: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), Valid: true}},
				Conversation: &store.ConversationPersistData{SourceConversationID: "series", ConversationType: "calendar"},
				RawFormat:    "gcal_json", RawMIME: []byte(original),
				Metadata: meetingCountCalendarMetadata(t, "calendar@example.test", original),
			})
			requirements.NoError(err)
			var notes []int64
			for _, id := range []string{"first", "second"} {
				note, err := st.PersistMessage(&store.MessagePersistData{
					Message:      &store.Message{SourceID: source.ID, SourceMessageID: id, MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), Valid: true}},
					Conversation: &store.ConversationPersistData{SourceConversationID: id, ConversationType: "meeting"},
					RawFormat:    "twenty_json", RawMIME: []byte(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"series@example.test","startsAt":"2027-03-21T10:00:00Z"}}`),
					Recipients: []store.RecipientSet{{Type: "to", ParticipantIDs: []int64{person}}},
				})
				requirements.NoError(err)
				notes = append(notes, note)
			}
			cache := filepath.Join(t.TempDir(), "analytics")
			readKeys := func(wantKey string) {
				t.Helper()
				want := make(map[int64][]string)
				if wantKey != "" {
					for _, id := range notes {
						want[id] = []string{wantKey}
					}
				}
				assertions.Equal(want, readCachedMeetingKeys(t, cache))
			}
			_, err = buildCache(path, cache, true)
			requirements.NoError(err)
			readKeys("uid:series@example.test/20270321T100000Z")
			if step.beforeRaw != "" {
				requirements.NoError(st.UpsertMessageRawWithFormat(event, []byte(step.beforeRaw), "gcal_json"))
				requirements.NoError(st.SetMessageMetadata(event, *meetingCountCalendarMetadata(t, "calendar@example.test", step.beforeRaw)))
				_, err = buildCache(path, cache, false)
				requirements.NoError(err)
				readKeys("")
			}
			requirements.NoError(st.UpsertMessageRawWithFormat(event, []byte(step.raw), "gcal_json"))
			requirements.NoError(st.SetMessageMetadata(event, *meetingCountCalendarMetadata(t, "calendar@example.test", step.raw)))
			assertions.False(cacheNeedsBuild(path, cache).FullRebuild)
			_, err = buildCache(path, cache, false)
			requirements.NoError(err)
			readKeys(step.wantKey)
			assertions.False(cacheNeedsBuild(path, cache).NeedsBuild)
		})
	}
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
