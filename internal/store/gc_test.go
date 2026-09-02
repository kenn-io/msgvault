package store_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestGCDeletesOnlySourceDeletedMessagesAndVacuumsSQLite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newSQLiteGCFixture(t)
	active := f.NewMessage().WithSourceMessageID("gc-active").Create(t, f.Store)
	dedupOnly := f.NewMessage().WithSourceMessageID("gc-dedup-only").Create(t, f.Store)
	sourceDeleted := f.NewMessage().WithSourceMessageID("gc-source-deleted").Create(t, f.Store)
	bothMarkers := f.NewMessage().WithSourceMessageID("gc-both-markers").Create(t, f.Store)

	_, err := f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE messages
		SET deleted_at = CURRENT_TIMESTAMP, delete_batch_id = 'gc-dedup-batch'
		WHERE id IN (?, ?)
	`), dedupOnly, bothMarkers)
	require.NoError(err, "mark dedup-hidden rows")
	require.NoError(f.Store.MarkMessageDeleted(
		f.Source.ID, "gc-source-deleted",
	), "mark source-deleted row")
	require.NoError(f.Store.MarkMessageDeleted(
		f.Source.ID, "gc-both-markers",
	), "mark both-markers row source-deleted")

	largeBody := strings.Repeat("source-deleted payload ", 100_000)
	require.NoError(f.Store.UpsertMessageBody(sourceDeleted,
		sql.NullString{String: largeBody, Valid: true}, sql.NullString{}),
		"store large body for compaction")
	_, err = f.Store.DB().Exec(`
		INSERT INTO messages_fts (
			rowid, message_id, subject, body, from_addr, to_addr, cc_addr
		) VALUES (?, ?, 'gc orphan sentinel', '', '', '', '')
	`, sourceDeleted, sourceDeleted)
	require.NoError(err, "index source-deleted message")

	plan, err := f.Store.PlanGCContext(t.Context())
	require.NoError(err, "PlanGCContext")
	revisionBefore, err := f.Store.DerivedDataRevision()
	require.NoError(err, "DerivedDataRevision before GC")
	assert.Equal(store.GCPlan{
		SourceDeleted:       2,
		DedupHiddenRetained: 1,
		SourceDeletedIDs:    []int64{sourceDeleted, bothMarkers},
	}, plan)

	deleted, err := f.Store.ExecuteGCContext(t.Context(), plan)
	require.NoError(err, "ExecuteGCContext")
	assert.Equal(int64(2), deleted)
	revisionAfter, err := f.Store.DerivedDataRevision()
	require.NoError(err, "DerivedDataRevision after GC")
	assert.Equal(revisionBefore+1, revisionAfter,
		"GC must invalidate caches containing purged messages")
	assert.ElementsMatch([]int64{active, dedupOnly}, remainingGCMessageIDs(t, f.Store))
	var staleFTSRows int64
	require.NoError(f.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM messages_fts WHERE rowid = ?`, sourceDeleted,
	).Scan(&staleFTSRows), "count stale FTS rows")
	assert.Zero(staleFTSRows, "GC must remove FTS rows for purged messages")

	var freeBefore int64
	require.NoError(f.Store.DB().QueryRow(`PRAGMA freelist_count`).Scan(&freeBefore),
		"read freelist before vacuum")
	require.Positive(freeBefore, "deleted payload should leave free SQLite pages")
	require.NoError(f.Store.VacuumContext(t.Context()), "VacuumContext")
	var freeAfter int64
	require.NoError(f.Store.DB().QueryRow(`PRAGMA freelist_count`).Scan(&freeAfter),
		"read freelist after vacuum")
	assert.Zero(freeAfter, "VACUUM should reclaim free pages")
}

func TestExecuteGCRejectsPlanMismatchWithoutDeleting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newSQLiteGCFixture(t)
	first := f.NewMessage().WithSourceMessageID("gc-planned").Create(t, f.Store)
	require.NoError(f.Store.MarkMessageDeleted(f.Source.ID, "gc-planned"),
		"mark planned message deleted")
	plan, err := f.Store.PlanGCContext(t.Context())
	require.NoError(err, "PlanGCContext")

	second := f.NewMessage().WithSourceMessageID("gc-added-after-plan").Create(t, f.Store)
	require.NoError(f.Store.MarkMessageDeleted(
		f.Source.ID, "gc-added-after-plan",
	), "mark later message deleted")

	deleted, err := f.Store.ExecuteGCContext(t.Context(), plan)
	require.ErrorContains(err, "GC plan changed")
	assert.Zero(deleted)
	assert.ElementsMatch([]int64{first, second}, remainingGCMessageIDs(t, f.Store),
		"optimistic mismatch must roll back every deletion")
}

func TestExecuteGCRejectsSameCountDifferentPopulation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newSQLiteGCFixture(t)
	planned := f.NewMessage().WithSourceMessageID("gc-planned-swap").Create(t, f.Store)
	replacement := f.NewMessage().WithSourceMessageID("gc-replacement-swap").Create(t, f.Store)
	require.NoError(f.Store.MarkMessageDeleted(f.Source.ID, "gc-planned-swap"),
		"mark planned message deleted")
	plan, err := f.Store.PlanGCContext(t.Context())
	require.NoError(err, "PlanGCContext")

	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE messages SET deleted_from_source_at = NULL WHERE id = ?
	`), planned)
	require.NoError(err, "restore originally planned message")
	require.NoError(f.Store.MarkMessageDeleted(f.Source.ID, "gc-replacement-swap"),
		"mark replacement message deleted")

	deleted, err := f.Store.ExecuteGCContext(t.Context(), plan)
	require.ErrorContains(err, "GC plan changed")
	assert.Zero(deleted)
	assert.ElementsMatch([]int64{planned, replacement}, remainingGCMessageIDs(t, f.Store),
		"same-size population swap must not authorize a different deletion")
}

func TestExecuteGCRecomputesAffectedConversationStats(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newSQLiteGCFixture(t)
	firstAt := time.Date(2026, time.January, 2, 3, 4, 0, 0, time.UTC)
	retained := f.NewMessage().
		WithSourceMessageID("gc-conversation-retained").
		WithSnippet("retained preview").
		WithSentAt(firstAt).
		Create(t, f.Store)
	purged := f.NewMessage().
		WithSourceMessageID("gc-conversation-purged").
		WithSnippet("purged preview").
		WithSentAt(firstAt.Add(time.Hour)).
		Create(t, f.Store)
	require.NoError(f.Store.RecomputeConversationStats(f.Source.ID),
		"seed conversation stats")
	require.NoError(f.Store.MarkMessageDeleted(
		f.Source.ID, "gc-conversation-purged",
	), "mark latest conversation message deleted")

	plan, err := f.Store.PlanGCContext(t.Context())
	require.NoError(err, "PlanGCContext")
	deleted, err := f.Store.ExecuteGCContext(t.Context(), plan)
	require.NoError(err, "ExecuteGCContext")
	assert.Equal(int64(1), deleted)
	assert.ElementsMatch([]int64{retained}, remainingGCMessageIDs(t, f.Store))

	var messageCount int
	var lastMessageAt sql.NullTime
	var preview sql.NullString
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT message_count, last_message_at, last_message_preview
		FROM conversations WHERE id = ?
	`), f.ConvID).Scan(&messageCount, &lastMessageAt, &preview),
		"read recomputed conversation stats")
	assert.Equal(1, messageCount)
	assert.Equal(firstAt, lastMessageAt.Time)
	assert.Equal("retained preview", preview.String)
	assert.NotEqual(purged, retained)
}

func TestExecuteGCClearsSurvivingRepliesToSourceDeletedMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newSQLiteGCFixture(t)
	parent := f.NewMessage().WithSourceMessageID("gc-deleted-parent").Create(t, f.Store)
	reply := f.NewMessage().WithSourceMessageID("gc-surviving-reply").Create(t, f.Store)
	require.NoError(f.Store.SetReplyTo(
		f.Source.ID, "gc-surviving-reply", "gc-deleted-parent",
	), "link surviving reply to deleted parent")
	require.NoError(f.Store.MarkMessageDeleted(
		f.Source.ID, "gc-deleted-parent",
	), "mark reply parent source-deleted")

	plan, err := f.Store.PlanGCContext(t.Context())
	require.NoError(err, "PlanGCContext")
	deleted, err := f.Store.ExecuteGCContext(t.Context(), plan)
	require.NoError(err, "ExecuteGCContext")
	assert.Equal(int64(1), deleted)
	assert.ElementsMatch([]int64{reply}, remainingGCMessageIDs(t, f.Store))

	var replyTo sql.NullInt64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT reply_to_message_id FROM messages WHERE id = ?`), reply).Scan(&replyTo),
		"read surviving reply pointer")
	assert.False(replyTo.Valid, "GC must clear the pointer to the purged parent")
	assert.NotEqual(parent, replyTo.Int64)
}

func newSQLiteGCFixture(t *testing.T) *storetest.Fixture {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(t, err, "setup source")
	convID, err := st.EnsureConversation(source.ID, "default-thread", "Default Thread")
	require.NoError(t, err, "setup conversation")
	return &storetest.Fixture{T: t, Store: st, Source: source, ConvID: convID}
}

func remainingGCMessageIDs(t *testing.T, st *store.Store) []int64 {
	t.Helper()
	rows, err := st.DB().Query(`SELECT id FROM messages ORDER BY id`)
	require.NoError(t, err, "list remaining messages")
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id), "scan remaining message")
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err(), "iterate remaining messages")
	return ids
}
