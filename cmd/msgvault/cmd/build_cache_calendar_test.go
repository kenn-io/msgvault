package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calsync"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/store"
)

// TestBuildCache_ExcludesCalendarEvents is the Parquet-leak regression: calendar
// events (and their attendee junction rows) must be excluded from the email
// analytics Parquet, or attendees would surface in Sender/Recipient/Domain
// aggregates and per-view counts would stop reconciling with the email-gated
// stats header.
func TestBuildCache_ExcludesCalendarEvents(t *testing.T) {
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

	// messages Parquet: only the email, never the calendar event.
	msgPattern := filepath.Join(analyticsDir, "messages", "**", "*.parquet")
	var msgCount int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?, hive_partitioning=true)`, msgPattern).Scan(&msgCount))
	assert.Equal(1, msgCount, "only the email message should be exported")

	var calCount int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?, hive_partitioning=true) WHERE message_type = 'calendar_event'`,
		msgPattern).Scan(&calCount))
	assert.Equal(0, calCount, "no calendar_event rows in the messages Parquet")

	// message_recipients Parquet: carol (email) present, bob (attendee) absent.
	recPattern := filepath.Join(analyticsDir, "message_recipients", "*.parquet")
	var bobRows int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?) WHERE participant_id = ?`, recPattern, bobID).Scan(&bobRows))
	assert.Equal(0, bobRows, "calendar attendee must not leak into the recipients Parquet")

	var carolRows int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?) WHERE participant_id = ?`, recPattern, carolID).Scan(&carolRows))
	assert.Equal(1, carolRows, "email recipient must still be exported")

	var calendarConversationRows int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?) WHERE conversation_type = 'calendar'`,
		filepath.Join(analyticsDir, "conversations", "*.parquet")).Scan(&calendarConversationRows))
	assert.Equal(0, calendarConversationRows, "calendar conversation must not leak into conversations Parquet")
}

func TestBuildCache_AllCalendarEventsWritesEmptyCacheState(t *testing.T) {
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
	require.NoError(err, "buildCache should accept an archive with no exportable email messages")
	require.False(result.Skipped, "calendar-only database still advances cache state")
	assert.Equal(int64(0), result.ExportedCount, "calendar events are intentionally excluded from email analytics")

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
