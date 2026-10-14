package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/duckdbutil"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestCalendarEventDedupUndoRebuildsLinkedMeetingCounts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st, err := store.OpenForTest(path)
	requirements.NoError(err)
	requirements.NoError(st.InitSchema())
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	when := sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	human, err := st.EnsureParticipant("human@example.org", "Example Human", "example.org")
	requirements.NoError(err)
	persist := func(kind, identifier, format, raw string) int64 {
		source, err := st.GetOrCreateSource(kind, identifier+"@example.org")
		requirements.NoError(err)
		sentAt := when
		if kind == "calendar_event" {
			// Calendar visibility must invalidate meeting counts even when the
			// provider did not supply a message timestamp.
			sentAt = sql.NullTime{}
		}
		id, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: identifier, MessageType: kind, SentAt: sentAt},
			Conversation: &store.ConversationPersistData{SourceConversationID: identifier, ConversationType: "meeting", Title: "Planning"},
			Metadata:     &sql.NullString{String: `{"calendar_id":"synthetic"}`, Valid: true},
			RawMIME:      []byte(raw), RawFormat: format,
		})
		requirements.NoError(err)
		requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{human}, []string{"Example Human"}))
		return id
	}
	event := persist("calendar_event", "calendar", "gcal_json", `{"id":"event","iCalUID":"planning@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	first := persist("meeting_transcript", "first", "granola_json", `{"calendar_event":{"calendar_event_id":"event","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
	second := persist("meeting_transcript", "second", "granola_json", `{"calendar_event":{"calendar_event_id":"event","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
	ref := fmt.Sprintf("evt:%d", event)
	requirements.NoError(st.LinkMeetingContext(t.Context(), ref, first))
	requirements.NoError(st.LinkMeetingContext(t.Context(), ref, second))
	cache := filepath.Join(t.TempDir(), "analytics")
	_, err = buildCache(path, cache, true)
	requirements.NoError(err)
	readCount := func() int64 {
		engine, err := query.NewDuckDBEngine(cache, "", nil)
		requirements.NoError(err)
		defer func() { requirements.NoError(engine.Close()) }()
		person, err := engine.GetPerson(t.Context(), human, query.Context{}, nil)
		requirements.NoError(err)
		requirements.NotNil(person)
		return person.MeetingCount
	}
	assertions.Equal(int64(1), readCount(), "linked transcripts count as one meeting before the hide")

	const batchID = "hidden-calendar-event"
	result, err := st.DB().Exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP, delete_batch_id = ? WHERE id = ?`, batchID, event)
	requirements.NoError(err)
	affected, err := result.RowsAffected()
	requirements.NoError(err)
	assertions.Equal(int64(1), affected)
	hiddenBuild, err := buildCache(path, cache, true)
	requirements.NoError(err)
	assertions.False(hiddenBuild.Skipped)
	assertions.Equal(int64(2), readCount(), "a cache rebuilt while the event is hidden counts transcript fallbacks separately")

	restored, err := st.UndoDedup(batchID)
	requirements.NoError(err)
	assertions.Equal(int64(1), restored)
	restoredBuild, err := buildCache(path, cache, false)
	requirements.NoError(err)
	assertions.False(restoredBuild.Skipped, "restoring event visibility must rebuild a cache stamped while it was hidden")
	assertions.Equal(int64(1), readCount(), "the restored event links both transcripts into one meeting again")
}

func TestCountsFollowLinkChangeWithoutNewMessages(t *testing.T) {
	for _, csv := range []bool{false, true} {
		name := "scanner"
		if csv {
			name = "csv"
		}
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			if csv {
				t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "1")
			} else {
				t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "")
			}
			path := filepath.Join(t.TempDir(), "archive.db")
			st, err := store.OpenForTest(path)
			requirements.NoError(err)
			requirements.NoError(st.InitSchema())
			t.Cleanup(func() { requirements.NoError(st.Close()) })
			when := sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
			human, err := st.EnsureParticipant("human@example.org", "Example Human", "example.org")
			requirements.NoError(err)
			persist := func(kind, identifier, format, raw string) int64 {
				source, err := st.GetOrCreateSource(kind, identifier+"@example.org")
				requirements.NoError(err)
				id, err := st.PersistMessage(&store.MessagePersistData{
					Message:      &store.Message{SourceID: source.ID, SourceMessageID: identifier, MessageType: kind, SentAt: when},
					Conversation: &store.ConversationPersistData{SourceConversationID: identifier, ConversationType: "meeting", Title: "Planning"},
					Metadata:     &sql.NullString{String: `{"calendar_id":"synthetic"}`, Valid: true},
					RawMIME:      []byte(raw), RawFormat: format,
				})
				requirements.NoError(err)
				requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{human}, []string{"Example Human"}))
				return id
			}
			event := persist("calendar_event", "calendar", "gcal_json", `{"id":"event","iCalUID":"planning@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
			first := persist("meeting_transcript", "first", "meeting_json", `{"summary_text":"Planning"}`)
			second := persist("meeting_transcript", "second", "meeting_json", `{"summary_text":"Planning"}`)
			cache := filepath.Join(t.TempDir(), "analytics")
			_, err = buildCache(path, cache, true)
			requirements.NoError(err)
			readCount := func() int64 {
				engine, err := query.NewDuckDBEngine(cache, "", nil)
				requirements.NoError(err)
				defer func() { requirements.NoError(engine.Close()) }()
				person, err := engine.GetPerson(t.Context(), human, query.Context{}, nil)
				requirements.NoError(err)
				requirements.NotNil(person)
				return person.MeetingCount
			}
			assertions.Equal(int64(2), readCount())
			ref := fmt.Sprintf("evt:%d", event)
			requirements.NoError(st.LinkMeetingContext(t.Context(), ref, first))
			requirements.NoError(st.LinkMeetingContext(t.Context(), ref, second))
			result, err := buildCache(path, cache, false)
			requirements.NoError(err)
			assertions.False(result.Skipped, "link-only edits must rebuild the published keys")
			assertions.Equal(int64(1), readCount())
			requirements.NoError(st.UnlinkMeetingContext(t.Context(), ref, second))
			_, err = buildCache(path, cache, false)
			requirements.NoError(err)
			assertions.Equal(int64(2), readCount())

			derivedRefreshBeforeSnapshotHook = func() { requirements.NoError(st.LinkMeetingContext(t.Context(), ref, second)) }
			t.Cleanup(func() {
				derivedRefreshBeforeSnapshotHook = nil
				buildCacheAfterSnapshotHook = nil
				buildCacheBeforeMessagesExportHook = nil
			})
			_, err = buildCacheDerivedOnly(path, cache)
			derivedRefreshBeforeSnapshotHook = nil
			requirements.ErrorIs(err, ErrDerivedRefreshRequiresFullBuild, "evidence changing before snapshot must reject derived reuse")
			assertions.Equal(int64(2), readCount(), "rejected refresh preserves the publication")
			_, err = buildCache(path, cache, false)
			requirements.NoError(err)
			assertions.Equal(int64(1), readCount())
			buildCacheAfterSnapshotHook = func() { requirements.NoError(st.UnlinkMeetingContext(t.Context(), ref, second)) }
			_, err = buildCache(path, cache, true)
			buildCacheAfterSnapshotHook = nil
			requirements.NoError(err)
			assertions.Equal(int64(1), readCount(), "writes after the pinned snapshot are excluded")
			state, err := query.ReadCacheSyncState(cache)
			requirements.NoError(err)
			live, err := st.DerivedDataRevision()
			requirements.NoError(err)
			assertions.Less(state.DerivedDataRevision, live)
			_, err = buildCache(path, cache, false)
			requirements.NoError(err)
			assertions.Equal(int64(2), readCount())
			requirements.NoError(st.LinkMeetingContext(t.Context(), ref, second))
			failure := errors.New("synthetic export failure")
			buildCacheBeforeMessagesExportHook = func() error { return failure }
			_, err = buildCache(path, cache, false)
			buildCacheBeforeMessagesExportHook = nil
			requirements.ErrorIs(err, failure)
			assertions.Equal(int64(2), readCount(), "failed export leaves the last complete publication")
			_, err = buildCache(path, cache, false)
			requirements.NoError(err)
			assertions.Equal(int64(1), readCount())
		})
	}
}

func TestMeetingKeySnapshotScannerAndCSVMatchNative(t *testing.T) {
	for _, mode := range []string{"scanner", "csv"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			if mode == "csv" {
				t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "1")
			} else {
				t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "")
			}
			path := filepath.Join(t.TempDir(), "archive.db")
			st, err := store.OpenForTest(path)
			requirements.NoError(err)
			requirements.NoError(st.InitSchema())
			t.Cleanup(func() { requirements.NoError(st.Close()) })
			source, err := st.GetOrCreateSource("gcal", "owner@example.org")
			requirements.NoError(err)
			event, err := st.PersistMessage(&store.MessagePersistData{
				Conversation: &store.ConversationPersistData{SourceConversationID: "calendar", ConversationType: "calendar_event"},
				Message:      &store.Message{SourceID: source.ID, SourceMessageID: "calendar", MessageType: "calendar_event", SentAt: sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}},
				Metadata:     &sql.NullString{String: `{"calendar_id":"synthetic"}`, Valid: true},
				RawMIME:      []byte(`{"id":"calendar","iCalUID":"planning@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"resource@example.org","resource":true,"self":false}]}`), RawFormat: "gcal_json",
			})
			requirements.NoError(err)
			transcript, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: "transcript", MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}}, Conversation: &store.ConversationPersistData{SourceConversationID: "transcript", ConversationType: "meeting"}})
			requirements.NoError(err)
			requirements.NoError(st.LinkMeetingContext(t.Context(), fmt.Sprintf("evt:%d", event), transcript))
			native, err := st.LogicalMeetingKeysContext(t.Context(), []int64{transcript})
			requirements.NoError(err)

			persistCalendar := func(id, raw string, sentAt time.Time) {
				_, err := st.PersistMessage(&store.MessagePersistData{
					Message:      &store.Message{SourceID: source.ID, SourceMessageID: id, MessageType: "calendar_event", SentAt: sql.NullTime{Time: sentAt, Valid: true}},
					Conversation: &store.ConversationPersistData{SourceConversationID: id, ConversationType: "calendar_event"},
					Metadata:     &sql.NullString{String: `{"calendar_id":"synthetic"}`, Valid: true},
					RawMIME:      []byte(raw), RawFormat: "gcal_json",
				})
				requirements.NoError(err)
			}
			persistCalendar("series", `{"id":"series","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T10:00:00Z","timeZone":"UTC"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`, time.Date(2027, 3, 7, 10, 0, 0, 0, time.UTC))
			persistCalendar("edited", `{"id":"edited","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`, time.Date(2027, 3, 14, 12, 0, 0, 0, time.UTC))
			persistTranscript := func(id, raw string, sentAt time.Time) int64 {
				transcriptID, err := st.PersistMessage(&store.MessagePersistData{
					Message:      &store.Message{SourceID: source.ID, SourceMessageID: id, MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: sentAt, Valid: true}},
					Conversation: &store.ConversationPersistData{SourceConversationID: id, ConversationType: "meeting"},
					Metadata:     &sql.NullString{String: `{"calendar_id":"synthetic"}`, Valid: true},
					RawMIME:      []byte(raw), RawFormat: "granola_json",
				})
				requirements.NoError(err)
				return transcriptID
			}
			firstRecurringCopy := persistTranscript("first-recurring-copy", `{"calendar_event":{"calendar_event_id":"series_20270314T100000Z","scheduled_start_time":"2027-03-14T10:00:00Z"}}`, time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC))
			movedRecurringCopy := persistTranscript("moved-recurring-copy", `{"calendar_event":{"calendar_event_id":"edited","scheduled_start_time":"2027-03-14T12:00:00Z"}}`, time.Date(2027, 3, 14, 12, 0, 0, 0, time.UTC))
			laterRecurringCopy := persistTranscript("later-recurring-copy", `{"calendar_event":{"calendar_event_id":"series_20270321T100000Z","scheduled_start_time":"2027-03-21T10:00:00Z"}}`, time.Date(2027, 3, 21, 10, 0, 0, 0, time.UTC))
			recurringIDs := []int64{firstRecurringCopy, movedRecurringCopy, laterRecurringCopy}
			nativeRecurring, err := st.LogicalMeetingKeysContext(t.Context(), recurringIDs)
			requirements.NoError(err)
			assertions.Equal(map[int64]string{
				firstRecurringCopy: "ical:weekly@example.org@20270314T100000Z",
				movedRecurringCopy: "ical:weekly@example.org@20270314T100000Z",
				laterRecurringCopy: "ical:weekly@example.org@20270321T100000Z",
			}, nativeRecurring)
			db, err := duckdbutil.Open(t.Context(), duckdbutil.BuilderPolicy(filepath.Join(t.TempDir(), "duckdb-tmp")))
			requirements.NoError(err)
			t.Cleanup(func() { requirements.NoError(db.Close()) })
			snapshot, err := openCacheSourceSnapshot(db, path)
			requirements.NoError(err)
			defer func() { requirements.NoError(snapshot.Close()) }()
			got, err := st.ExportLogicalMeetingKeysContext(t.Context(), snapshot, []int64{transcript})
			requirements.NoError(err)
			assertions.Equal(native, got)
			snapshotRecurring, err := st.ExportLogicalMeetingKeysContext(t.Context(), snapshot, recurringIDs)
			requirements.NoError(err)
			assertions.Equal(nativeRecurring, snapshotRecurring)
			var original, start, end sql.NullTime
			var bounds, allDay bool
			requirements.NoError(snapshot.QueryRowContext(t.Context(), `SELECT original_start_at,start_at,end_at,has_bounds,all_day FROM calendar_events WHERE message_id=?`, event).Scan(&original, &start, &end, &bounds, &allDay))
			assertions.False(original.Valid)
			requirements.True(start.Valid)
			requirements.True(end.Valid)
			assertions.True(bounds)
			assertions.False(allDay)
			assertions.Equal(time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), start.Time.UTC())
		})
	}
}

func TestCacheSourceSnapshotSkipsNestedTimestampProjectionRewrite(t *testing.T) {
	query := `SELECT COALESCE((SELECT scheduled_at FROM meeting_link_keys WHERE member_message_id = ?), start_at) AS scheduled_at FROM calendar_events WHERE message_id = ?`
	statement := (&cacheSourceSnapshot{}).sourceQuery(query, nil)
	assert.NotContains(t, statement, "SELECT * REPLACE")
}

func TestCacheRoomEvidenceAndCuratedHumanContactRefreshWithoutMessages(t *testing.T) {
	for _, mode := range []string{"scanner", "csv"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			if mode == "csv" {
				t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "1")
			} else {
				t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "")
			}
			path := filepath.Join(t.TempDir(), "archive.db")
			st, err := store.OpenForTest(path)
			requirements.NoError(err)
			requirements.NoError(st.InitSchema())
			t.Cleanup(func() { requirements.NoError(st.Close()) })
			source, err := st.GetOrCreateSource("gcal", "owner@example.org")
			requirements.NoError(err)
			resource, err := st.EnsureParticipant("resource@example.org", "Example Resource", "example.org")
			requirements.NoError(err)
			person, _, err := st.CreatePersonFromParticipantContext(t.Context(), resource)
			requirements.NoError(err)
			message, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: "meeting", MessageType: "calendar_event", SentAt: sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}}, Conversation: &store.ConversationPersistData{SourceConversationID: "meeting", ConversationType: "calendar_event"}, Metadata: &sql.NullString{String: `{"calendar_id":"synthetic"}`, Valid: true}, RawMIME: []byte(`{"id":"meeting","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"resource@example.org","resource":true}]}`), RawFormat: "gcal_json"})
			requirements.NoError(err)
			requirements.NoError(st.ReplaceMessageRecipients(message, "to", []int64{resource}, []string{"Example Resource"}))
			cache := filepath.Join(t.TempDir(), "analytics")
			_, err = buildCache(path, cache, true)
			requirements.NoError(err)
			read := func() *query.PersonSummary {
				engine, err := query.NewDuckDBEngine(cache, "", nil)
				requirements.NoError(err)
				defer func() { requirements.NoError(engine.Close()) }()
				result, err := engine.GetPerson(t.Context(), resource, query.Context{}, nil)
				requirements.NoError(err)
				return result
			}
			assertions.Nil(read())
			human, err := st.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "human@example.org", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
			requirements.NoError(err)
			result, err := buildCache(path, cache, false)
			requirements.NoError(err)
			assertions.False(result.Skipped)
			requirements.NotNil(read())
			requirements.NoError(st.SupersedePersonContactPointContext(t.Context(), person.ID, human.Envelope.ID, nil))
			_, err = buildCache(path, cache, false)
			requirements.NoError(err)
			assertions.Nil(read())
			_, err = st.DB().Exec(`DELETE FROM calendar_event_attendees WHERE message_id=?`, message)
			requirements.NoError(err)
			_, err = buildCache(path, cache, false)
			requirements.NoError(err)
			requirements.NotNil(read())
		})
	}
}
