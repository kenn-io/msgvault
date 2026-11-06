package cmd

import (
	"context"
	"database/sql"
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

func TestIndependentMeetingHTTPDecisionsRefreshPeople(t *testing.T) {
	// serial: replaces the cache subprocess entry point with the real in-process builder.
	for _, decision := range []string{"link", "unlink"} {
		t.Run(decision, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newMeetingCountSeriesFixture(t, 2)
			ref := store.OccurrenceRef{MessageID: f.event, Key: "20260314T100000Z"}
			requirements.NoError(f.st.LinkMeetingContext(t.Context(), f.notes[0], ref))
			want := int64(1)
			if decision == "unlink" {
				requirements.NoError(f.st.LinkMeetingContext(t.Context(), f.notes[1], ref))
				want = 2
			}
			_, err := buildCache(f.path, f.cache, true)
			requirements.NoError(err)
			previous := executeBuildCacheSubprocessMode
			executeBuildCacheSubprocessMode = func(_ context.Context, mode buildCacheMode) error {
				requirements.Equal(buildCacheModeDerived, mode)
				_, err := buildCacheDerivedOnly(f.path, f.cache)
				return err
			}
			t.Cleanup(func() { executeBuildCacheSubprocessMode = previous })
			srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: f.st, analyticsDir: f.cache}, Logger: slog.New(slog.DiscardHandler)})
			engine, err := query.NewDuckDBEngine(f.cache, "", nil)
			requirements.NoError(err)
			t.Cleanup(func() { requirements.NoError(engine.Close()) })
			request := httptest.NewRequest(http.MethodPost, "/api/v1/meetings/links/decisions", strings.NewReader(fmt.Sprintf(`{"message_id":%d,"event":"evt:%d@20260314T100000Z","decision":%q}`, f.notes[1], f.event, decision)))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, request)
			requirements.Equal(http.StatusOK, response.Code, response.Body.String())
			people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{Query: "person@example.org"})
			requirements.NoError(err)
			requirements.Len(people.Rows, 1)
			assertions.Equal(want, people.Rows[0].MeetingCount)
		})
	}
}

func TestMeetingCountsCancelledSeriesKeepsLinkedCopiesTogether(t *testing.T) {
	requirements := require.New(t)
	f := newMeetingCountSeriesFixture(t, 2)
	for _, note := range f.notes {
		requirements.NoError(f.st.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: f.event, Key: "20260314T100000Z"}))
	}
	_, err := buildCache(f.path, f.cache, true)
	requirements.NoError(err)
	requirements.NoError(f.st.UpsertMessageRawWithFormat(f.event, []byte(`{"id":"series","iCalUID":"weekly@example.org","status":"cancelled","start":{"dateTime":"2026-03-14T10:00:00Z"},"end":{"dateTime":"2026-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`), "gcal_json"))
	_, err = buildCache(f.path, f.cache, false)
	requirements.NoError(err)
	engine, err := query.NewDuckDBEngine(f.cache, "", nil)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{Query: "person@example.org"})
	requirements.NoError(err)
	requirements.Len(people.Rows, 1)
	assert.Equal(t, int64(1), people.Rows[0].MeetingCount)
}

func TestDerivedRefreshWithPendingMailRequiresMeetingRepair(t *testing.T) {
	for _, decision := range []string{"link", "unlink", "unchanged"} {
		t.Run(decision, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newMeetingCountSeriesFixture(t, 2)
			mailSource, err := f.st.GetOrCreateSource("gmail", "owner@example.org")
			requirements.NoError(err)
			ref := store.OccurrenceRef{MessageID: f.event, Key: "20260314T100000Z"}
			requirements.NoError(f.st.LinkMeetingContext(t.Context(), f.notes[0], ref))
			if decision == "unlink" {
				requirements.NoError(f.st.LinkMeetingContext(t.Context(), f.notes[1], ref))
			}
			_, err = buildCache(f.path, f.cache, true)
			requirements.NoError(err)
			before, err := query.ReadCacheSyncState(f.cache)
			requirements.NoError(err)
			_, err = f.st.PersistMessage(&store.MessagePersistData{
				Message:      &store.Message{SourceID: mailSource.ID, SourceMessageID: "pending-mail", MessageType: "email", SentAt: sql.NullTime{Time: time.Date(2026, 3, 29, 10, 0, 0, 0, time.UTC), Valid: true}},
				Conversation: &store.ConversationPersistData{SourceConversationID: "pending-mail", ConversationType: "email"},
			})
			requirements.NoError(err)
			switch decision {
			case "link":
				requirements.NoError(f.st.LinkMeetingContext(t.Context(), f.notes[1], ref))
			case "unlink":
				requirements.NoError(f.st.UnlinkMeetingContext(t.Context(), f.notes[1], ref))
			}
			result, err := buildCacheDerivedOnly(f.path, f.cache)
			if decision == "unchanged" {
				requirements.NoError(err)
				assertions.True(result.Skipped)
			} else {
				requirements.ErrorIs(err, ErrDerivedRefreshRequiresFullBuild)
			}
			after, err := query.ReadCacheSyncState(f.cache)
			requirements.NoError(err)
			assertions.Equal(before, after, "leave pending facts and meeting changes unacknowledged")
		})
	}
}

func TestDerivedRefreshWithUnpublishedMeetingDecisionRequiresFullBuild(t *testing.T) {
	requirements := require.New(t)
	f := newMeetingCountSeriesFixture(t, 1)
	_, err := buildCache(f.path, f.cache, true)
	requirements.NoError(err)
	before, err := query.ReadCacheSyncState(f.cache)
	requirements.NoError(err)
	note, err := f.st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: f.source, SourceMessageID: "pending-note", MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: time.Date(2026, 3, 29, 10, 0, 0, 0, time.UTC), Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "pending-note", ConversationType: "meeting"},
		RawFormat:    "plaud_json", RawMIME: []byte(`{}`),
		Recipients: []store.RecipientSet{{Type: "to", ParticipantIDs: []int64{f.person}, DisplayNames: []string{"Person"}}},
	})
	requirements.NoError(err)
	requirements.Greater(note, before.LastMessageID)
	requirements.NoError(f.st.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: f.event, Key: "20260314T100000Z"}))
	_, err = buildCacheDerivedOnly(f.path, f.cache)
	requirements.ErrorIs(err, ErrDerivedRefreshRequiresFullBuild)
	after, err := query.ReadCacheSyncState(f.cache)
	requirements.NoError(err)
	assert.Equal(t, before, after, "leave the unpublished meeting decision pending")
}

func TestIndependentSeparateOccurrencesKeepTheirDays(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newMeetingCountSeriesFixture(t, 1)
	for _, key := range []string{"20260314T100000Z", "20260321T100000Z"} {
		requirements.NoError(f.st.LinkMeetingContext(t.Context(), f.notes[0], store.OccurrenceRef{MessageID: f.event, Key: key}))
	}
	_, err := buildCache(f.path, f.cache, true)
	requirements.NoError(err)
	engine, err := query.NewDuckDBEngine(f.cache, "", nil)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	calendar, err := engine.RelationshipCalendar(t.Context(), query.RelationshipCalendarRequest{CanonicalID: f.person, Year: 2026, Timezone: "UTC"})
	requirements.NoError(err)
	db, err := sql.Open("duckdb", "")
	requirements.NoError(err)
	defer func() { requirements.NoError(db.Close()) }()
	var first, second, transcript int64
	requirements.NoError(db.QueryRow(`SELECT coalesce(sum(meeting_count) FILTER (WHERE event_date = DATE '2026-03-14'),0), coalesce(sum(meeting_count) FILTER (WHERE event_date = DATE '2026-03-21'),0), coalesce(sum(meeting_count) FILTER (WHERE event_date = DATE '2026-03-28'),0) FROM read_parquet(?) WHERE canonical_id=?`, filepath.Join(f.cache, "relationship_temperature_contributions", "*.parquet"), f.person).Scan(&first, &second, &transcript))
	assertions.Zero(first)
	assertions.Zero(second)
	assertions.Equal(int64(2), transcript)

	t.Run("date-filtered drill-down", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		requirements.NotEmpty(calendar.Days)
		var meetingCount int64
		for _, day := range calendar.Days {
			meetingCount += day.Meetings
			if day.Meetings == 0 {
				continue
			}
			start, err := time.Parse(time.DateOnly, day.Date)
			requirements.NoError(err)
			scope := query.Context{ParticipantIDs: []int64{f.person}, After: &start, Before: new(start.AddDate(0, 0, 1))}
			relationships, err := engine.Relationships(t.Context(), query.RelationshipsRequest{Context: scope, Now: time.Date(2026, 3, 28, 10, 0, 0, 0, time.UTC)})
			requirements.NoError(err)
			assertions.Len(relationships.Rows, 1, "calendar day %s must have a relationship", day.Date)
			if len(relationships.Rows) == 1 {
				assertions.Equal(day.Meetings, relationships.Rows[0].Signals.MeetingCount)
			}
			timeline, err := engine.RelationshipTimeline(t.Context(), query.RelationshipTimelineRequest{CanonicalID: f.person, Context: scope, Timezone: "UTC"})
			requirements.NoError(err)
			assertions.Len(timeline.Rows, 1, "calendar day %s must open its transcript", day.Date)
		}
		assertions.Equal(int64(2), meetingCount)
	})
}
