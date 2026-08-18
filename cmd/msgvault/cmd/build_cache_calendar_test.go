package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calsync"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/store"
	"golang.org/x/oauth2"
)

// TestBuildCache_IncludesCalendarEventsInModalityNeutralCache verifies calendar
// rows and attendees are available to the common analytical read model. Legacy
// email aggregates remain email-scoped in query code rather than by dropping
// non-email rows during publication.
func TestBuildCache_IncludesCalendarEventsInModalityNeutralCache(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	analyticsDir := filepath.Join(tmp, "analytics")

	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(st.InitSchema(), "init schema")

	// An ordinary email: sender alice → recipient carol.
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	convID, err := st.EnsureConversationWithType(src.ID, "thread-1", "email_thread", "Hi")
	require.NoError(err)
	aliceID, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err)
	carolID, err := st.EnsureParticipant("carol@example.com", "Carol", "example.com")
	require.NoError(err)
	emailID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m1",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC), Valid: true},
		SenderID:        sql.NullInt64{Int64: aliceID, Valid: true},
		Subject:         sql.NullString{String: "Hi", Valid: true},
	})
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(emailID, "from", []int64{aliceID}, []string{""}))
	require.NoError(st.ReplaceMessageRecipients(emailID, "to", []int64{carolID}, []string{""}))

	// A calendar event via calsync: organizer dave, attendee bob.
	mock := gcal.NewMockAPI()
	mock.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	mock.FullEvents["primary"] = [][]gcal.Event{{{
		ID:        "ev1",
		Status:    gcal.StatusConfirmed,
		Summary:   "Planning",
		Organizer: gcal.Person{Email: "dave@example.com", DisplayName: "Dave"},
		Start:     gcal.EventDateTime{DateTime: time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC)},
		End:       gcal.EventDateTime{DateTime: time.Date(2024, 5, 2, 9, 30, 0, 0, time.UTC)},
		Attendees: []gcal.Attendee{{Email: "bob@example.com", DisplayName: "Bob"}},
	}}}
	mock.FullSyncToken["primary"] = "T1"
	_, err = calsync.New(mock, st, calsync.Options{AccountEmail: "dave@example.com"}).Full(context.Background())
	require.NoError(err)

	bobID, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err)

	require.NoError(st.Close())

	result, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "buildCache")
	require.False(result.Skipped, "buildCache unexpectedly skipped")

	duckdb, err := sql.Open("duckdb", "")
	require.NoError(err)
	defer func() { _ = duckdb.Close() }()

	// messages Parquet is modality-neutral: email and calendar event coexist.
	msgPattern := filepath.Join(analyticsDir, "messages", "**", "*.parquet")
	var msgCount int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?, hive_partitioning=true)`, msgPattern).Scan(&msgCount))
	assert.Equal(2, msgCount, "email and calendar event should be exported")

	var calCount int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?, hive_partitioning=true) WHERE message_type = 'calendar_event'`,
		msgPattern).Scan(&calCount))
	assert.Equal(1, calCount, "calendar_event row in the messages Parquet")

	// Both email recipients and event attendees are analytical participants.
	recPattern := filepath.Join(analyticsDir, "message_recipients", "*.parquet")
	var bobRows int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?) WHERE participant_id = ?`, recPattern, bobID).Scan(&bobRows))
	assert.Equal(1, bobRows, "calendar attendee should be exported")

	var carolRows int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?) WHERE participant_id = ?`, recPattern, carolID).Scan(&carolRows))
	assert.Equal(1, carolRows, "email recipient must still be exported")

	var calendarConversationRows int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?) WHERE conversation_type = 'calendar'`,
		filepath.Join(analyticsDir, "conversations", "*.parquet")).Scan(&calendarConversationRows))
	assert.Equal(1, calendarConversationRows, "calendar conversation should be exported")
}

func TestBuildCache_AllCalendarEventsWritesAnalyticalCacheState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	analyticsDir := filepath.Join(tmp, "analytics")

	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(st.InitSchema(), "init schema")

	mock := gcal.NewMockAPI()
	mock.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	mock.FullEvents["primary"] = [][]gcal.Event{{{
		ID:        "ev1",
		Status:    gcal.StatusConfirmed,
		Summary:   "Planning",
		Organizer: gcal.Person{Email: "alice@example.com", DisplayName: "Alice"},
		Start:     gcal.EventDateTime{DateTime: time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC)},
		End:       gcal.EventDateTime{DateTime: time.Date(2024, 5, 2, 9, 30, 0, 0, time.UTC)},
	}}}
	mock.FullSyncToken["primary"] = "T1"
	_, err = calsync.New(mock, st, calsync.Options{AccountEmail: "alice@example.com"}).Full(context.Background())
	require.NoError(err)
	require.NoError(st.Close())

	result, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "buildCache should accept a calendar-only archive")
	require.False(result.Skipped, "calendar-only database still advances cache state")
	assert.Equal(int64(1), result.ExportedCount, "calendar events are part of the modality-neutral cache")

	data, err := os.ReadFile(filepath.Join(analyticsDir, "_last_sync.json"))
	require.NoError(err, "cache state should be written")
	var state syncState
	require.NoError(json.Unmarshal(data, &state), "decode cache state")
	assert.Equal(result.MaxMessageID, state.LastMessageID, "state records the covered calendar-event watermark")

	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	require.False(staleness.NeedsBuild, "calendar-only cache state should not request repeated rebuilds: %+v", staleness)

	result2, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "second buildCache should accept stable calendar-only state")
	assert.True(result2.Skipped, "calendar-only cache should be skipped on the second build")
}

const (
	cacheCalendarBoundaryAccount = "alice@example.com"
	cacheCalendarBoundaryEventID = "utf8-boundary-event"
)

func TestBuildCache_PreservesCalsyncUTF8ByteBoundary(t *testing.T) {
	boundarySummary := strings.Repeat("a", 198) + "\u2014after-boundary"
	boundarySnippet := strings.Repeat("a", 198)

	runInitial := func(t *testing.T, forceCSV bool) {
		t.Helper()
		require := require.New(t)

		if forceCSV {
			t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "1")
		} else {
			t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "")
		}

		tmp := t.TempDir()
		dbPath := filepath.Join(tmp, "msgvault.db")
		analyticsDir := filepath.Join(tmp, "analytics")

		_, stored := syncCalendarBoundaryEvent(t, dbPath, boundarySummary, "TOKEN1")
		require.Equal(boundarySnippet, stored, "exact SQLite snippet")

		_, err := buildCache(dbPath, analyticsDir, false)
		require.NoError(err, "initial cache publication")
		assertCalendarBoundarySnippetPublished(t, analyticsDir, boundarySnippet)
	}

	t.Run("initial default publication", func(t *testing.T) {
		runInitial(t, false)
	})

	t.Run("initial forced CSV publication", func(t *testing.T) {
		runInitial(t, true)
	})

	t.Run("explicit full publication replaces the same event", func(t *testing.T) {
		require := require.New(t)
		t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "")

		tmp := t.TempDir()
		dbPath := filepath.Join(tmp, "msgvault.db")
		analyticsDir := filepath.Join(tmp, "analytics")
		oldSummary := strings.Repeat("b", 200) + "old-tail"
		oldSnippet := strings.Repeat("b", 200)

		oldID, stored := syncCalendarBoundaryEvent(t, dbPath, oldSummary, "TOKEN1")
		require.Equal(oldSnippet, stored, "initial SQLite snippet")

		_, err := buildCache(dbPath, analyticsDir, false)
		require.NoError(err, "initial cache publication")
		assertCalendarBoundarySnippetPublished(t, analyticsDir, oldSnippet)

		updatedID, stored := syncCalendarBoundaryEvent(t, dbPath, boundarySummary, "TOKEN2")
		require.Equal(oldID, updatedID, "full sync must update the existing event row")
		require.Equal(boundarySnippet, stored, "updated SQLite snippet")

		_, err = buildCache(dbPath, analyticsDir, true)
		require.NoError(err, "explicit full cache publication")
		assertCalendarBoundarySnippetPublished(t, analyticsDir, boundarySnippet)
	})
}

func syncCalendarBoundaryEvent(
	t *testing.T,
	dbPath, summary, syncToken string,
) (int64, string) {
	t.Helper()

	calendarListJSON, err := json.Marshal(map[string]any{
		"items": []map[string]any{
			{
				"id":         "primary",
				"accessRole": "owner",
			},
		},
	})
	require.NoError(t, err, "marshal calendar list response")

	eventsJSON, err := json.Marshal(map[string]any{
		"items": []map[string]any{
			{
				"id":      cacheCalendarBoundaryEventID,
				"status":  gcal.StatusConfirmed,
				"summary": summary,
				"organizer": map[string]any{
					"email": cacheCalendarBoundaryAccount,
					"self":  true,
				},
				"start": map[string]any{
					"dateTime": "2024-05-02T09:00:00Z",
				},
				"end": map[string]any{
					"dateTime": "2024-05-02T09:30:00Z",
				},
			},
		},
		"nextSyncToken": syncToken,
	})
	require.NoError(t, err, "marshal events response")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/users/me/calendarList":
			_, _ = w.Write(calendarListJSON)
		case "/calendars/primary/events":
			_, _ = w.Write(eventsJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	st, err := store.Open(dbPath)
	require.NoError(t, err, "open store")
	defer func() {
		assert.NoError(t, st.Close(), "close store")
	}()
	require.NoError(t, st.InitSchema(), "init schema")

	tokenSource := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"})
	client := gcal.NewClient(
		tokenSource,
		gcal.WithBaseURL(srv.URL),
		gcal.WithHTTPClient(srv.Client()),
	)
	defer func() {
		assert.NoError(t, client.Close(), "close calendar client")
	}()

	_, err = calsync.New(client, st, calsync.Options{
		AccountEmail: cacheCalendarBoundaryAccount,
	}).Full(context.Background())
	require.NoError(t, err, "full calendar sync")

	var messageID int64
	var stored sql.NullString
	err = st.DB().QueryRow(`
		SELECT m.id, m.snippet
		FROM messages m
		JOIN sources s ON s.id = m.source_id
		WHERE s.source_type = ?
		  AND s.identifier = ?
		  AND m.source_message_id = ?
		  AND m.message_type = ?`,
		gcal.SourceType,
		cacheCalendarBoundaryAccount+"/primary",
		cacheCalendarBoundaryEventID,
		gcal.MessageTypeCalendarEvent,
	).Scan(&messageID, &stored)
	require.NoError(t, err, "read stored calendar snippet")
	require.True(t, stored.Valid, "calendar snippet must be non-NULL")
	return messageID, stored.String
}

func assertCalendarBoundarySnippetPublished(
	t *testing.T,
	analyticsDir, want string,
) {
	t.Helper()
	assert := assert.New(t)
	got := readCalendarBoundarySnippet(t, analyticsDir)

	assert.Equal(want, got)
	assert.LessOrEqual(len(got), 200)
	assert.True(utf8.ValidString(got))
}

func readCalendarBoundarySnippet(t *testing.T, analyticsDir string) string {
	t.Helper()
	require := require.New(t)

	duckDB, err := sql.Open("duckdb", "")
	require.NoError(err, "open DuckDB")
	defer func() {
		assert.NoError(t, duckDB.Close(), "close DuckDB")
	}()

	pattern := filepath.Join(analyticsDir, "messages", "**", "*.parquet")
	var count int
	require.NoError(duckDB.QueryRow(`
		SELECT COUNT(*)
		FROM read_parquet(?, hive_partitioning=true)
		WHERE source_message_id = ? AND message_type = ?`,
		pattern,
		cacheCalendarBoundaryEventID,
		gcal.MessageTypeCalendarEvent,
	).Scan(&count), "count boundary event in Parquet")
	require.Equal(1, count, "boundary event must have exactly one Parquet row")

	var got sql.NullString
	require.NoError(duckDB.QueryRow(`
		SELECT snippet
		FROM read_parquet(?, hive_partitioning=true)
		WHERE source_message_id = ? AND message_type = ?`,
		pattern,
		cacheCalendarBoundaryEventID,
		gcal.MessageTypeCalendarEvent,
	).Scan(&got), "read boundary snippet from Parquet")
	require.True(got.Valid, "Parquet snippet must be non-NULL")
	return got.String
}
