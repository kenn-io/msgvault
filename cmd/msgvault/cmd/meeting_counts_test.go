package cmd

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

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
	for _, identifier := range []string{"first", "second", "calendar"} {
		kind := "meeting_transcript"
		if identifier == "calendar" {
			kind = "calendar_event"
		}
		source, err := st.GetOrCreateSource(kind, identifier+"@example.org")
		requirements.NoError(err)
		owners[identifier], err = st.EnsureParticipant(identifier+"@example.org", "Owner", "example.org")
		requirements.NoError(err)
		requirements.NoError(st.AddAccountIdentity(source.ID, identifier+"@example.org", "explicit"))
	}
	persist := func(kind, identifier, format, raw string) int64 {
		at := time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)
		if identifier == "second" {
			at = at.Add(24 * time.Hour)
		}
		source, err := st.GetOrCreateSource(kind, identifier+"@example.org")
		requirements.NoError(err)
		id, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: identifier, MessageType: kind, SentAt: sql.NullTime{Time: at, Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: identifier, ConversationType: "meeting", Title: "Planning"},
			RawMIME:      []byte(raw), RawFormat: format,
		})
		requirements.NoError(err)
		requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{human, owners[identifier]}, []string{"Example Human", "Owner"}))
		return id
	}
	persist("meeting_transcript", "first", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"planning@example.org"}}`)
	second := persist("meeting_transcript", "second", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"planning@example.org"}}`)
	cache := filepath.Join(t.TempDir(), "analytics")
	var preservedShards map[string]string
	readCount := func(want int64) {
		engine, err := query.NewDuckDBEngine(cache, "", nil)
		requirements.NoError(err)
		defer func() { requirements.NoError(engine.Close()) }()
		people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{Query: "human@example.org"})
		requirements.NoError(err)
		requirements.Len(people.Rows, 1)
		assertions.Equal(want, people.Rows[0].MeetingCount)
		person, err := engine.GetPerson(t.Context(), human, query.Context{}, nil)
		requirements.NoError(err)
		requirements.NotNil(person)
		assertions.Equal(want, person.MeetingCount)
		db, err := sql.Open("duckdb", "")
		requirements.NoError(err)
		defer func() { requirements.NoError(db.Close()) }()
		var scored int64
		requirements.NoError(db.QueryRow(`SELECT sum(meeting_count) FROM read_parquet(?) WHERE canonical_id = ?`, filepath.Join(cache, identityindex.DatasetTemperatureContributions, "*.parquet"), human).Scan(&scored))
		assertions.Equal(want, scored, "temperature counts each linked occurrence once")
		relationships, err := engine.Relationships(t.Context(), query.RelationshipsRequest{Context: query.Context{ParticipantIDs: []int64{human}}, Now: time.Date(2026, 3, 16, 10, 0, 0, 0, time.UTC)})
		requirements.NoError(err)
		requirements.Len(relationships.Rows, 1)
		assertions.Equal(want, relationships.Rows[0].Signals.MeetingCount)
		calendar, err := engine.RelationshipCalendar(t.Context(), query.RelationshipCalendarRequest{CanonicalID: human, Year: 2026, Timezone: "UTC"})
		requirements.NoError(err)
		var dailyMeetings int64
		for _, day := range calendar.Days {
			dailyMeetings += day.Meetings
		}
		assertions.Equal(want, dailyMeetings)
		var linked int
		requirements.NoError(db.QueryRow(`SELECT count(*) FROM read_parquet(?) WHERE message_id = ?`, filepath.Join(cache, "meeting_keys", "*.parquet"), second).Scan(&linked))
		if want == 1 {
			assertions.Equal(1, linked)
		} else {
			assertions.Zero(linked)
		}
		if preservedShards != nil {
			assertions.Equal(preservedShards, snapshotMessagesDatasetBytes(t, cache))
		}
	}
	_, err = buildCache(path, cache, true)
	requirements.NoError(err)
	readCount(2)
	event := persist("calendar_event", "calendar", "gcal_json", `{"id":"event","iCalUID":"planning@example.org","start":{"dateTime":"2026-03-14T10:00:00Z"},"end":{"dateTime":"2026-03-14T11:00:00Z"}}`)
	requirements.NoError(st.SetMessageMetadata(event, sql.NullString{String: `{"ical_uid":"planning@example.org"}`, Valid: true}))
	assertions.False(cacheNeedsBuild(path, cache).FullRebuild)
	incremental, err := buildCache(path, cache, false)
	requirements.NoError(err)
	assertions.Equal(int64(1), incremental.StagedCount)
	readCount(1)
	// A re-import changes automatic evidence while keeping the message ID.
	for _, tc := range []struct {
		uid   string
		count int64
	}{
		{"unmatched@example.org", 2}, {"planning@example.org", 1},
	} {
		requirements.NoError(st.UpsertMessageRawWithFormat(second, []byte(fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q}}`, tc.uid)), "twenty_json"))
		assertions.False(cacheNeedsBuild(path, cache).FullRebuild)
		_, err := buildCache(path, cache, false)
		requirements.NoError(err)
		readCount(tc.count)
	}
	preservedShards = snapshotMessagesDatasetBytes(t, cache)
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), second, store.OccurrenceRef{MessageID: event}))
	assertions.False(cacheNeedsBuild(path, cache).FullRebuild)
	_, err = buildCache(path, cache, false)
	requirements.NoError(err)
	readCount(2)
	requirements.NoError(st.LinkMeetingContext(t.Context(), second, store.OccurrenceRef{MessageID: event}))
	_, err = buildCache(path, cache, false)
	requirements.NoError(err)
	readCount(1)
	preservedShards = nil
	_, err = buildCache(path, cache, true)
	requirements.NoError(err)
	readCount(1)
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
			persist := func(id, kind, format, raw string) int64 {
				messageID, err := st.PersistMessage(&store.MessagePersistData{
					Message:      &store.Message{SourceID: source.ID, SourceMessageID: id, MessageType: kind, SentAt: sql.NullTime{Time: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), Valid: true}},
					Conversation: &store.ConversationPersistData{SourceConversationID: id, ConversationType: "meeting"},
					RawFormat:    format, RawMIME: []byte(raw),
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
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st, err := store.OpenForTest(path)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	requirements.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("twenty", "owner@example.test")
	requirements.NoError(err)
	person, err := st.EnsureParticipant("person@example.test", "Person", "example.test")
	requirements.NoError(err)
	const eventRaw = `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00","timeZone":%q},"end":{"dateTime":"2027-03-14T11:00:00","timeZone":%q},"recurrence":%s}`
	original := fmt.Sprintf(eventRaw, "UTC", "UTC", `["RRULE:FREQ=WEEKLY;COUNT=3"]`)
	event, err := st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "event", MessageType: "calendar_event", SentAt: sql.NullTime{Time: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "event", ConversationType: "calendar"},
		RawFormat:    "gcal_json", RawMIME: []byte(original),
		Metadata: &sql.NullString{String: `{"ical_uid":"series@example.test"}`, Valid: true},
	})
	requirements.NoError(err)
	for _, id := range []string{"first", "second"} {
		_, err = st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: id, MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: id, ConversationType: "meeting"},
			RawFormat:    "twenty_json", RawMIME: []byte(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"series@example.test","startsAt":"2027-03-14T10:00:00Z"}}`),
			Recipients: []store.RecipientSet{{Type: "to", ParticipantIDs: []int64{person}}},
		})
		requirements.NoError(err)
	}
	cache := filepath.Join(t.TempDir(), "analytics")
	_, err = buildCache(path, cache, true)
	requirements.NoError(err)
	for _, step := range []struct {
		name, raw, uid string
		want           int64
	}{
		{"baseline", original, "series@example.test", 1},
		{"projection removed", `{}`, "series@example.test", 2},
		{"restored after projection removal", original, "series@example.test", 1},
		{"EXDATE", fmt.Sprintf(eventRaw, "UTC", "UTC", `["RRULE:FREQ=WEEKLY;COUNT=3","EXDATE:20270314T100000Z"]`), "series@example.test", 2},
		{"restored after EXDATE", original, "series@example.test", 1},
		{"zone", fmt.Sprintf(eventRaw, "America/New_York", "America/New_York", `["RRULE:FREQ=WEEKLY;COUNT=3"]`), "series@example.test", 2},
		{"restored after zone", original, "series@example.test", 1},
		{"UID", original, "different@example.test", 2},
	} {
		t.Run(step.name, func(t *testing.T) {
			requirements := require.New(t)
			requirements.NoError(st.UpsertMessageRawWithFormat(event, []byte(step.raw), "gcal_json"))
			requirements.NoError(st.SetMessageMetadata(event, sql.NullString{String: fmt.Sprintf(`{"ical_uid":%q}`, step.uid), Valid: true}))
			assert.False(t, cacheNeedsBuild(path, cache).FullRebuild)
			_, err := buildCache(path, cache, false)
			requirements.NoError(err)
			engine, err := query.NewDuckDBEngine(cache, "", nil)
			requirements.NoError(err)
			defer func() { requirements.NoError(engine.Close()) }()
			people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{})
			requirements.NoError(err)
			requirements.Len(people.Rows, 1)
			assert.Equal(t, step.want, people.Rows[0].MeetingCount)
			assert.False(t, cacheNeedsBuild(path, cache).NeedsBuild)
		})
	}
}
