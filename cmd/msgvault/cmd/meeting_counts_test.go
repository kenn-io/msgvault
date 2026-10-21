package cmd

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonMergeRelatedDriftKeepsDerivedRefresh(t *testing.T) {
	for _, tc := range []struct {
		name             string
		newMail, updated bool
	}{
		{name: "labels only"},
		{name: "new mail", newMail: true},
		{name: "updated mail", updated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			root := t.TempDir()
			path, cache := filepath.Join(root, "test.db"), filepath.Join(root, "analytics")
			st, err := store.OpenForTest(path)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(st.Close()) })
			require.NoError(st.InitSchema())
			firstID, err := st.EnsureParticipant("first@example.test", "First", "example.test")
			require.NoError(err)
			secondID, err := st.EnsureParticipant("second@example.test", "Second", "example.test")
			require.NoError(err)
			source, err := st.GetOrCreateSource("gmail", "owner@example.test")
			require.NoError(err)
			messageID, err := st.PersistMessage(&store.MessagePersistData{
				Message:      &store.Message{SourceID: source.ID, SourceMessageID: "mail", MessageType: "email", SentAt: sql.NullTime{Time: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), Valid: true}},
				Conversation: &store.ConversationPersistData{SourceConversationID: "mail", ConversationType: "email"},
				Recipients:   []store.RecipientSet{{Type: "to", ParticipantIDs: []int64{firstID, secondID}}},
			})
			require.NoError(err)
			_, err = st.DB().Exec(`INSERT INTO labels (source_id, source_label_id, name) VALUES (?, 'unread', 'Unread')`, source.ID)
			require.NoError(err)
			_, err = st.DB().Exec(`INSERT INTO message_labels (message_id, label_id) SELECT ?, id FROM labels`, messageID)
			require.NoError(err)
			first, _, err := st.CreatePersonFromParticipant(firstID)
			require.NoError(err)
			second, _, err := st.CreatePersonFromParticipant(secondID)
			require.NoError(err)
			_, err = buildCache(path, cache, true)
			require.NoError(err)
			_, err = st.DB().Exec(`DELETE FROM message_labels WHERE message_id = (SELECT min(message_id) FROM message_labels)`)
			require.NoError(err)
			if tc.newMail {
				_, err = st.DB().Exec(`INSERT INTO messages (source_id, source_message_id, conversation_id, message_type, sent_at) SELECT source_id, 'new-mail', conversation_id, 'email', sent_at FROM messages LIMIT 1`)
				require.NoError(err)
			}
			if tc.updated {
				var sourceID int64
				require.NoError(st.DB().QueryRow(`SELECT min(id) FROM sources`).Scan(&sourceID))
				syncID, err := st.StartSync(sourceID, "incremental")
				require.NoError(err)
				require.NoError(st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{MessagesProcessed: 1, MessagesUpdated: 1}))
				require.NoError(st.CompleteSync(syncID, "cursor"))
			}
			assert.True(cacheNeedsBuild(path, cache).HasRelatedRowDrift)
			previous := executeBuildCacheSubprocessMode
			var modes []buildCacheMode
			executeBuildCacheSubprocessMode = func(_ context.Context, mode buildCacheMode) error {
				modes = append(modes, mode)
				if mode != buildCacheModeDerived {
					return errors.New("unexpected full cache build")
				}
				_, err := buildCacheDerivedOnly(path, cache)
				return err
			}
			t.Cleanup(func() { executeBuildCacheSubprocessMode = previous })
			srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st, analyticsDir: cache}, Logger: slog.New(slog.DiscardHandler)})
			request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/people/%d/merge", first.ID), strings.NewReader(fmt.Sprintf(`{"absorbed_person_id":%d}`, second.ID)))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("If-Match", fmt.Sprintf(`"person-%d-r%d", "person-%d-r%d"`, first.ID, first.Revision, second.ID, second.Revision))
			request.Header.Set("Idempotency-Key", "merge-related-drift")
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, request)
			require.Equal(http.StatusOK, response.Code, response.Body.String())
			var body struct {
				CacheState string `json:"cache_state"`
			}
			require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal("ready", body.CacheState)
			assert.Equal([]buildCacheMode{buildCacheModeDerived}, modes)
		})
	}
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
		assertions.Equal(want, scored, "temperature days count the linked occurrence once")
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
	otherCalendar, err := st.GetOrCreateSource("gcal", "other-calendar@example.org")
	requirements.NoError(err)
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: otherCalendar.ID, SourceMessageID: "sparse", MessageType: "calendar_event"},
		Conversation: &store.ConversationPersistData{SourceConversationID: "sparse", ConversationType: "calendar"},
		Metadata:     &sql.NullString{String: `{"ical_uid":"planning@example.org"}`, Valid: true},
		RawFormat:    "gcal_json", RawMIME: []byte(`{"id":"sparse","iCalUID":"planning@example.org","status":"cancelled"}`),
	})
	requirements.NoError(err)
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
		result, err := buildCacheDerivedOnly(path, cache)
		requirements.NoError(err)
		assertions.True(result.IdentityOnly)
		readCount(tc.count)
	}
	preservedShards = snapshotMessagesDatasetBytes(t, cache)
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), second, store.OccurrenceRef{MessageID: event}))
	assertions.False(cacheNeedsBuild(path, cache).FullRebuild)
	result, err := buildCacheDerivedOnly(path, cache)
	requirements.NoError(err)
	assertions.True(result.IdentityOnly)
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

func TestMeetingSyncsStayIncremental(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	root := t.TempDir()
	path, cache := filepath.Join(root, "test.db"), filepath.Join(root, "analytics")
	st, err := store.OpenForTest(path)
	requirements.NoError(err)
	defer func() { requirements.NoError(st.Close()) }()
	requirements.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "baseline@example.org")
	requirements.NoError(err)
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "baseline", MessageType: "email", SentAt: sql.NullTime{Time: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "baseline", ConversationType: "email"},
	})
	requirements.NoError(err)
	_, err = buildCache(path, cache, true)
	requirements.NoError(err)
	var eventID int64
	checks := []struct {
		name  string
		write func()
	}{
		{"phone participant", func() {
			id, err := st.EnsureParticipant("new@example.org", "New Person", "example.org")
			requirements.NoError(err)
			requirements.NoError(st.SetParticipantIdentifier(id, "phone", "+15550100001"))
		}},
		{"calendar event", func() {
			source, err := st.GetOrCreateSource("gcal", "calendar@example.org")
			requirements.NoError(err)
			eventID, err = st.PersistMessage(&store.MessagePersistData{
				Message:      &store.Message{SourceID: source.ID, SourceMessageID: "event", MessageType: "calendar_event", SentAt: sql.NullTime{Time: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), Valid: true}},
				Conversation: &store.ConversationPersistData{SourceConversationID: "event", ConversationType: "calendar"},
				RawMIME:      []byte(`{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`), RawFormat: "gcal_json",
			})
			requirements.NoError(err)
		}},
		{"transcript import", func() {
			source, err := st.GetOrCreateSource("meeting_import", "meeting@example.org")
			requirements.NoError(err)
			_, err = st.PersistMessage(&store.MessagePersistData{
				Message:      &store.Message{SourceID: source.ID, SourceMessageID: "note", MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), Valid: true}},
				Conversation: &store.ConversationPersistData{SourceConversationID: "note", ConversationType: "meeting"},
				RawMIME:      []byte(`{"summary_text":"Planning"}`), RawFormat: "meeting_json",
			})
			requirements.NoError(err)
		}},
		{"unchanged event sync", func() {
			requirements.NoError(st.UpsertMessageRawWithFormat(eventID, []byte(`{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`), "gcal_json"))
		}},
	}
	for _, tc := range checks {
		assertions.False(cacheNeedsBuild(path, cache).NeedsBuild, tc.name)
		tc.write()
		assertions.False(cacheNeedsBuild(path, cache).FullRebuild, tc.name)
		assertions.False(cacheNeedsBuild(path, cache).HasRelatedRowDrift, tc.name)
		_, err := buildCache(path, cache, false)
		requirements.NoError(err)
	}
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
			note := persist("note", "meeting_transcript", "plaud_json", `{}`)
			var decisionSeq, late int64
			buildCacheAfterSnapshotHook = func() {
				require.NoError(st.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: event}))
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
			assertions.Equal(1, count, "the later key read may include the decision")
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
		{"zone", fmt.Sprintf(eventRaw, "America/New_York", "America/New_York", `["RRULE:FREQ=WEEKLY;COUNT=3"]`), "series@example.test", 2},
		{"EXDATE", fmt.Sprintf(eventRaw, "UTC", "UTC", `["RRULE:FREQ=WEEKLY;COUNT=3","EXDATE:20270314T100000Z"]`), "series@example.test", 2},
		{"projection removed", `{}`, "series@example.test", 2},
		{"restored", original, "series@example.test", 1},
		{"UID", original, "different@example.test", 2},
	} {
		t.Run(step.name, func(t *testing.T) {
			requirements := require.New(t)
			requirements.NoError(st.UpsertMessageRawWithFormat(event, []byte(step.raw), "gcal_json"))
			requirements.NoError(st.SetMessageMetadata(event, sql.NullString{String: fmt.Sprintf(`{"ical_uid":%q}`, step.uid), Valid: true}))
			assert.False(t, cacheNeedsBuild(path, cache).FullRebuild)
			_, err := buildCacheDerivedOnly(path, cache)
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

func TestMeetingLinkDecisionRefreshesCounts(t *testing.T) {
	for _, tc := range []struct {
		name                                                string
		movedCounters, newTranscript, newEmail, labelChange bool
		wantState                                           string
		wantCount                                           int64
	}{
		{name: "older only", wantState: "ready", wantCount: 1},
		{name: "label change and link", labelChange: true, wantState: "stale", wantCount: 2},
		{name: "changed population", movedCounters: true, wantState: "stale", wantCount: 2},
		{name: "transcript above boundary", newTranscript: true, wantState: "stale", wantCount: 1},
		{name: "related drift and new rows", movedCounters: true, newEmail: true, wantState: "stale", wantCount: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "archive.db")
			st, err := store.OpenForTest(path)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(st.Close()) })
			require.NoError(st.InitSchema())
			source, err := st.GetOrCreateSource("meeting_import", "owner@example.test")
			require.NoError(err)
			person, err := st.EnsureParticipant("person@example.test", "Person", "example.test")
			require.NoError(err)
			persist := func(id, kind, raw, format string) int64 {
				messageID, err := st.PersistMessage(&store.MessagePersistData{
					Message:      &store.Message{SourceID: source.ID, SourceMessageID: id, MessageType: kind, SentAt: sql.NullTime{Time: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), Valid: true}},
					Conversation: &store.ConversationPersistData{SourceConversationID: id, ConversationType: "meeting"},
					RawFormat:    format, RawMIME: []byte(raw),
					Recipients: []store.RecipientSet{{Type: "to", ParticipantIDs: []int64{person}}},
				})
				require.NoError(err)
				return messageID
			}
			event := persist("event", "calendar_event", `{"id":"event","start":{"dateTime":"2026-08-01T10:00:00Z"},"end":{"dateTime":"2026-08-01T11:00:00Z"}}`, "gcal_json")
			first := persist("first", "meeting_transcript", `{}`, "plaud_json")
			var second int64
			if !tc.newTranscript {
				second = persist("second", "meeting_transcript", `{}`, "plaud_json")
			}
			require.NoError(st.LinkMeetingContext(t.Context(), first, store.OccurrenceRef{MessageID: event}))
			cache := filepath.Join(t.TempDir(), "analytics")
			_, err = buildCache(path, cache, true)
			require.NoError(err)
			if tc.labelChange {
				labelID, err := st.EnsureLabel(source.ID, "reviewed", "Reviewed", "user")
				require.NoError(err)
				require.NoError(st.ReplaceMessageLabels(first, []int64{labelID}))
			}
			if tc.newTranscript {
				second = persist("second", "meeting_transcript", `{}`, "plaud_json")
			}
			if tc.newEmail {
				persist("email", "email", `{}`, "")
			}
			previousExecute := executeBuildCacheSubprocessMode
			var modes []buildCacheMode
			var derivedErr error
			executeBuildCacheSubprocessMode = func(_ context.Context, mode buildCacheMode) error {
				modes = append(modes, mode)
				if mode != buildCacheModeDerived {
					return errors.New("unexpected full cache build")
				}
				_, derivedErr = buildCacheDerivedOnly(path, cache)
				return derivedErr
			}
			t.Cleanup(func() { executeBuildCacheSubprocessMode = previousExecute })
			if tc.movedCounters {
				syncID, err := st.StartSync(source.ID, "incremental")
				require.NoError(err)
				checkpoint := &store.Checkpoint{MessagesProcessed: 1, MessagesUpdated: 1}
				if tc.newEmail {
					checkpoint.MessagesAdded, checkpoint.MessagesUpdated = 1, 0
				}
				require.NoError(st.UpdateSyncCheckpoint(syncID, checkpoint))
				require.NoError(st.CompleteSync(syncID, "cursor"))
			}
			srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st, analyticsDir: cache}, Logger: slog.New(slog.DiscardHandler)})
			request := httptest.NewRequest(http.MethodPost, "/api/v1/meetings/links/decisions", strings.NewReader(fmt.Sprintf(`{"message_id":%d,"event":"evt:%d","decision":"link"}`, second, event)))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, request)
			require.Equal(http.StatusOK, response.Code, response.Body.String())
			var body struct {
				CacheState string `json:"cache_state"`
			}
			require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal(tc.wantState, body.CacheState)
			wantModes := []buildCacheMode{buildCacheModeDerived}
			if tc.movedCounters && !tc.newEmail {
				wantModes = append(wantModes, buildCacheModeFull)
			}
			assert.Equal(wantModes, modes)
			if tc.newEmail || tc.labelChange {
				require.ErrorIs(derivedErr, ErrDerivedRefreshNeedsAppend)
			}
			links, err := st.MeetingLinksContext(t.Context(), second)
			require.NoError(err)
			require.Len(links, 1, "decision remains saved even when the cache needs a full rebuild")
			engine, err := query.NewDuckDBEngine(cache, "", nil)
			require.NoError(err)
			defer func() { require.NoError(engine.Close()) }()
			people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{})
			require.NoError(err)
			require.Len(people.Rows, 1)
			assert.Equal(tc.wantCount, people.Rows[0].MeetingCount)
			if tc.wantState == "ready" {
				db, err := sql.Open("duckdb", "")
				require.NoError(err)
				defer func() { require.NoError(db.Close()) }()
				var key string
				require.NoError(db.QueryRow(`SELECT meeting_key FROM read_parquet(?) WHERE message_id = ?`, filepath.Join(cache, "meeting_keys", "*.parquet"), second).Scan(&key))
				assert.Equal(fmt.Sprintf("evt:%d/", event), key)
			}
		})
	}
}
