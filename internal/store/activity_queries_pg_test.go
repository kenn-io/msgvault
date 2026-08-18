package store_test

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/store"
)

var activityQueryPGSchemaSequence atomic.Uint64

func TestDayContextUsesOnePostgresSnapshotAcrossIndependentSubpages(t *testing.T) {
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(dbURL, "postgres://") &&
		!strings.HasPrefix(dbURL, "postgresql://") {
		t.Skip("PostgreSQL snapshot regression")
	}
	require := require.New(t)
	assert := assert.New(t)
	first, second, admin := newActivityQueryPostgresStores(t, dbURL)

	source, err := first.GetOrCreateSource("gmail", "synthetic@example.invalid")
	require.NoError(err)
	conversationID, err := first.EnsureConversation(
		source.ID, "snapshot-thread", "Synthetic snapshot thread")
	require.NoError(err)
	require.NoError(first.AddAccountIdentity(
		source.ID, "owner@example.invalid", "manual"))
	owner, err := first.EnsureParticipant(
		"owner@example.invalid", "Owner", "example.invalid")
	require.NoError(err)
	counterpart, err := first.EnsureParticipant(
		"counterpart@example.invalid", "Counterpart", "example.invalid")
	require.NoError(err)
	person, _, err := first.CreatePersonFromParticipant(counterpart)
	require.NoError(err)
	occurredAt := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	messageID, err := first.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: source.ID,
		SourceMessageID: "snapshot-message", MessageType: "email",
		SentAt: sql.NullTime{Time: occurredAt, Valid: true}, SizeEstimate: 1,
	})
	require.NoError(err)
	require.NoError(first.ReplaceMessageRecipients(
		messageID, "from", []int64{counterpart}, []string{"Counterpart"}))
	require.NoError(first.ReplaceMessageRecipients(
		messageID, "to", []int64{owner}, []string{"Owner"}))
	projector, err := activity.NewProjector(first, activity.Options{
		Timezone: "UTC", BatchSize: 1, MaxDirectCounterparts: 25,
	})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)

	days, err := first.PersonDaysContext(t.Context(), store.PersonDaysRequest{
		PersonID: person.ID,
		From:     "2026-07-30",
		To:       "2026-07-30",
		Limit:    10,
	})
	require.NoError(err)
	require.Len(days.Days, 1)
	assert.Equal(int64(1), days.TotalCount)

	blocker, err := first.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.ExecContext(t.Context(),
		`LOCK TABLE daily_note_entry_persons IN ACCESS EXCLUSIVE MODE`)
	require.NoError(err)

	type dayResult struct {
		page *store.DayPage
		err  error
	}
	result := make(chan dayResult, 1)
	go func() {
		page, err := first.DayContext(t.Context(), store.DayRequest{
			LocalDate: "2026-07-30", Limit: 10, EntryLimit: 10,
		})
		result <- dayResult{page: page, err: err}
	}()
	waitForBlockedDailyNoteRead(t, admin)

	writer, err := second.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	var ordinal int64
	err = writer.QueryRowContext(t.Context(), `
		INSERT INTO daily_note_day_sequences (local_date, last_ordinal)
		VALUES ($1, 1)
		ON CONFLICT(local_date) DO UPDATE
			SET last_ordinal = daily_note_day_sequences.last_ordinal + 1
		RETURNING last_ordinal
	`, "2026-07-30").Scan(&ordinal)
	require.NoError(err)
	_, err = writer.ExecContext(t.Context(), `
		INSERT INTO daily_note_entries (
			local_date, ordinal, body, author, source, source_ref, created_at, updated_at
		) VALUES ($1, $2, $3, $4, 'user', NULL, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`, "2026-07-30", ordinal, "Inserted after the activity snapshot", "Test User")
	require.NoError(err)
	require.NoError(writer.Commit())
	require.NoError(blocker.Commit())

	select {
	case got := <-result:
		require.NoError(got.err)
		require.NotNil(got.page)
		require.Len(got.page.Persons, 1)
		assert.Equal(person.ID, got.page.Persons[0].PersonID)
		assert.Empty(got.page.Entries)
		assert.Zero(got.page.EntryTotalCount)
	case <-time.After(5 * time.Second):
		require.FailNow("DayContext did not finish after releasing the table lock")
	}
}

func newActivityQueryPostgresStores(
	t *testing.T,
	dbURL string,
) (*store.Store, *store.Store, *sql.DB) {
	t.Helper()
	require := require.New(t)
	schema := fmt.Sprintf(
		"msgvault_activity_query_%d_%d",
		time.Now().UnixNano(),
		activityQueryPGSchemaSequence.Add(1),
	)
	admin, err := sql.Open("pgx", dbURL)
	require.NoError(err)
	_, err = admin.ExecContext(t.Context(), "CREATE SCHEMA "+schema)
	require.NoError(err)
	separator := "?"
	if strings.Contains(dbURL, "?") {
		separator = "&"
	}
	testURL := dbURL + separator + "search_path=" + schema
	first, err := store.Open(testURL)
	require.NoError(err)
	second, err := store.Open(testURL)
	require.NoError(err)
	require.NoError(first.InitSchema())
	t.Cleanup(func() {
		_ = first.Close()
		_ = second.Close()
		_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		_ = admin.Close()
	})
	return first, second, admin
}

func waitForBlockedDailyNoteRead(t *testing.T, admin *sql.DB) {
	t.Helper()
	require := require.New(t)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		err := admin.QueryRowContext(t.Context(), `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE datname = current_database()
				  AND state = 'active'
				  AND wait_event_type = 'Lock'
				  AND query LIKE '%daily_note_entry_persons%'
			)
		`).Scan(&blocked)
		require.NoError(err)
		if blocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.FailNow("daily-note subpage did not block on the table lock")
}
