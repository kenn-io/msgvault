package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Watermark literals used by the tests that need a KNOWN ordering instead of
// whatever the trigger stamped. They are written in the canonical form the
// SQLite trigger emits (strftime('%Y-%m-%d %H:%M:%f')) because SQLite compares
// these values lexically: a literal in any other shape (a "+00" suffix, say)
// would sort below an equal instant written by the trigger and the fixture
// would be testing the fixture rather than the query. PostgreSQL parses the
// same literal into a TIMESTAMPTZ and compares instants, so one spelling works
// on both backends.
const (
	watermarkEarly  = "2001-02-03 04:05:06.000"
	watermarkMiddle = "2002-02-03 04:05:06.000"
	watermarkLate   = "2003-02-03 04:05:06.000"
)

// seedFeedMessage inserts one message with a distinct source_message_id and the
// given sent_at, through the same UpsertMessage path importers use, so the
// INSERT trigger stamps content_changed_at exactly as it would in production.
// A zero sentAt is stored as NULL.
func seedFeedMessage(t *testing.T, st *store.Store, n int, sentAt time.Time) int64 {
	t.Helper()
	id, err := insertFeedMessage(st, n, sentAt)
	require.NoErrorf(t, err, "seed feed message %d", n)
	return id
}

// insertFeedMessage is seedFeedMessage without the assertions, for the importer
// that runs on a goroutine of its own. require's failure path is t.FailNow,
// which is runtime.Goexit; testing documents that as unsupported anywhere but
// the test's own goroutine, and it would unwind past any recover(). Returning
// the error lets the test assert on it where asserting is legal.
func insertFeedMessage(st *store.Store, n int, sentAt time.Time) (int64, error) {
	src, err := st.GetOrCreateSource("gmail", "feed@example.com")
	if err != nil {
		return 0, fmt.Errorf("GetOrCreateSource: %w", err)
	}
	convID, err := st.EnsureConversationWithType(
		src.ID, fmt.Sprintf("feed-conv-%d", n), "email_thread", "Feed thread")
	if err != nil {
		return 0, fmt.Errorf("EnsureConversationWithType: %w", err)
	}
	id, err := st.UpsertMessage(&store.Message{
		SourceID:        src.ID,
		SourceMessageID: fmt.Sprintf("feed-msg-%d", n),
		ConversationID:  convID,
		MessageType:     "email",
		Subject:         sql.NullString{String: fmt.Sprintf("feed subject %d", n), Valid: true},
		Snippet:         sql.NullString{String: fmt.Sprintf("feed snippet %d", n), Valid: true},
		SentAt:          sql.NullTime{Time: sentAt, Valid: !sentAt.IsZero()},
		SizeEstimate:    int64(1000 + n),
	})
	if err != nil {
		return 0, fmt.Errorf("UpsertMessage: %w", err)
	}
	return id, nil
}

// setWatermark forces content_changed_at to an exact value for the given
// messages. The statement names only content_changed_at, so no UPDATE OF list
// matches and the trigger does not overwrite it (see content_changed_at_test.go).
func setWatermark(t *testing.T, st *store.Store, value any, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		_, err := st.DB().Exec(
			st.Rebind(`UPDATE messages SET content_changed_at = ? WHERE id = ?`), value, id)
		require.NoErrorf(t, err, "set content_changed_at for message %d", id)
	}
}

// setWatermarkAt forces content_changed_at to an exact INSTANT rather than a
// fixed literal, binding it the way each backend compares it: SQLite stores the
// trigger's textual format and compares lexically, PostgreSQL parses a real
// timestamptz. Tests that have to place a watermark relative to the database
// clock need this; tests that only need a known ordering use the literals above.
func setWatermarkAt(t *testing.T, st *store.Store, when time.Time, ids ...int64) {
	t.Helper()
	var value any = when.UTC()
	if !st.IsPostgreSQL() {
		value = when.UTC().Format(store.SQLiteTimestampLayout)
	}
	setWatermark(t, st, value, ids...)
}

// feedFarFuture is a cursor no watermark can reach, so a page requested with it
// is always empty and carries nothing but the database clock reading.
var feedFarFuture = time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)

// databaseClock returns the database server's current time as the feed reads
// it. Every page carries it, empty ones included, so asking for a page from a
// cursor nothing can match is the cheapest way to read the clock alone.
func databaseClock(t *testing.T, st *store.Store) time.Time {
	t.Helper()
	page, err := st.ListChangedMessages(context.Background(), store.ChangedMessagesFrom(feedFarFuture), 1)
	require.NoError(t, err, "read the database clock")
	require.False(t, page.ServerTime.IsZero(), "every page carries the database clock")
	return page.ServerTime
}

// feedCompleteThrough returns the instant the feed is complete through — its
// page bound, not its clock. It is what decides whether a row is publishable
// yet, so it is what a test that has just written one has to wait on.
func feedCompleteThrough(t *testing.T, st *store.Store) time.Time {
	t.Helper()
	page, err := st.ListChangedMessages(context.Background(), store.ChangedMessagesFrom(feedFarFuture), 1)
	require.NoError(t, err, "read the feed's commit bound")
	return page.CompleteThrough
}

// waitForDatabaseClockPast blocks until the database CLOCK has moved strictly
// past when. That is a weaker condition than the feed being complete through
// that instant, and only two tests want it: the ones that hold a write
// transaction open on purpose and need the clock to leave an instant the feed
// is deliberately refusing to publish.
func waitForDatabaseClockPast(t *testing.T, st *store.Store, when time.Time) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !databaseClock(t, st).After(when) {
		if time.Now().After(deadline) {
			require.Failf(t, "database clock stalled",
				"the database clock never moved past %s", when)
			return
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// waitForFeedPast blocks until the feed is complete strictly past when. The
// feed withholds the instant it is bounded at — that instant can still receive
// commits — so a row is publishable only once the bound has left it.
func waitForFeedPast(t *testing.T, st *store.Store, when time.Time) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !feedCompleteThrough(t, st).After(when) {
		if time.Now().After(deadline) {
			require.Failf(t, "the change feed stopped advancing",
				"complete_through never moved past %s; something is holding a write "+
					"transaction open", when)
			return
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// settleFeedClock blocks until every row written so far is publishable. Tests
// that seed rows through the triggers and then expect to see them need it: the
// seed and the poll can land in the same millisecond on SQLite, and the feed
// deliberately holds that millisecond back. Tests that stamp their own
// watermarks in the past do not.
func settleFeedClock(t *testing.T, st *store.Store) {
	t.Helper()
	waitForFeedPast(t, st, databaseClock(t, st))
}

// setMessageTimestamp writes a lifecycle timestamp column directly. These are
// content columns, so the write also bumps the watermark -- which is the point:
// a deletion must show up in the feed.
func setMessageTimestamp(t *testing.T, st *store.Store, id int64, col string, value time.Time) {
	t.Helper()
	_, err := st.DB().Exec(
		st.Rebind(fmt.Sprintf(`UPDATE messages SET %s = ? WHERE id = ?`, col)), value, id)
	require.NoErrorf(t, err, "set messages.%s for message %d", col, id)
}

// drainChangedMessages pages the feed from the caller's cursor until it comes
// back empty, advancing that cursor in place exactly as a consumer would, and
// returns the ids in the order the feed produced them -- duplicates included,
// so callers can assert exact-once delivery rather than mere membership. It
// fails rather than hangs if the cursor stops advancing.
func drainChangedMessages(
	t *testing.T, st *store.Store, limit int, cursor *store.ChangedMessagesCursor,
) []int64 {
	t.Helper()
	var ids []int64
	for page := 0; ; page++ {
		require.Lessf(t, page, 200,
			"the feed did not terminate after %d pages: the cursor is not advancing", page)
		got, err := st.ListChangedMessages(context.Background(), *cursor, limit)
		require.NoError(t, err, "ListChangedMessages")
		require.False(t, got.ServerTime.IsZero(), "every page must carry the database clock")
		require.LessOrEqual(t, len(got.Messages), limit, "a page must not exceed the limit")
		if len(got.Messages) == 0 {
			return ids
		}
		for _, m := range got.Messages {
			ids = append(ids, m.ID)
		}
		last := got.Messages[len(got.Messages)-1]
		*cursor = store.ChangedMessagesAfter(last.ContentChangedAt, last.ID)
	}
}

// walkChangedMessages drains the entire feed from a first-run cursor. It settles
// the clock first, so rows the triggers stamped moments ago are not still inside
// the instant the feed withholds.
func walkChangedMessages(t *testing.T, st *store.Store, limit int) []int64 {
	t.Helper()
	settleFeedClock(t, st)
	var cursor store.ChangedMessagesCursor
	return drainChangedMessages(t, st, limit, &cursor)
}

// TestListChangedMessages_SameInstantBlockPagesExactlyOnce is why the cursor is
// composite. Rapid writes share a millisecond, so paging on the timestamp alone
// either loses the rows after the cursor within that instant (`>`) or returns
// that instant forever (`>=`). Every row of one same-instant block must come
// back exactly once across a walk whose pages are smaller than the block.
func TestListChangedMessages_SameInstantBlockPagesExactlyOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	const total = 25
	want := make([]int64, 0, total)
	for i := 1; i <= total; i++ {
		want = append(want, seedFeedMessage(t, st, i, time.Time{}))
	}
	setWatermark(t, st, watermarkMiddle, want...)

	got := walkChangedMessages(t, st, 5)

	require.Len(got, total,
		"a walk over %d rows sharing one watermark returned %d rows: a composite "+
			"cursor must deliver each row exactly once", total, len(got))
	assert.Equal(want, got,
		"the walk must return every same-instant row exactly once, in id order")
}

// TestListChangedMessages_CursorSurvivesInstantBoundary pins the parameter
// ENCODING, not just the SQL. A time.Time bound straight through compares below
// an equal stored value on SQLite (the driver serialises it with a "+00:00"
// suffix while the column holds "...05.000"), so rows sharing the cursor's
// instant are skipped -- silently, and only at page boundaries. The watermarks
// here are the ones the trigger actually wrote, and the walk uses a page size of
// one so every single row is a page boundary.
func TestListChangedMessages_CursorSurvivesInstantBoundary(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	const total = 11
	want := make([]int64, 0, total)
	for i := 1; i <= total; i++ {
		want = append(want, seedFeedMessage(t, st, i, time.Time{}))
	}
	// These rows carry the watermarks the trigger really wrote, so the clock has
	// to leave the instant the last of them landed in before the feed will
	// publish it.
	settleFeedClock(t, st)

	// A row whose watermark EQUALS the cursor must still be returned: that is
	// the comparison the encoding breaks, and asserting it directly says so
	// even when the walk below happens to have no ties.
	all, err := st.ListChangedMessages(context.Background(), store.ChangedMessagesCursor{}, total)
	require.NoError(err)
	require.Len(all.Messages, total)
	mid := all.Messages[total/2]
	atCursor, err := st.ListChangedMessages(context.Background(), store.ChangedMessagesFrom(mid.ContentChangedAt), total)
	require.NoError(err)
	ids := make([]int64, 0, len(atCursor.Messages))
	for _, m := range atCursor.Messages {
		ids = append(ids, m.ID)
	}
	assert.Contains(ids, mid.ID,
		"a row whose content_changed_at equals the cursor must still be returned; "+
			"binding the cursor as a raw time.Time drops it on SQLite")

	assert.Equal(want, walkChangedMessages(t, st, 1),
		"a page-size-1 walk must return all %d rows exactly once: every page "+
			"boundary re-encodes the cursor", total)
}

// feedOpenInstantLead is how far ahead of the database clock the regression test
// below places its watermark. It has to outlast the poll that follows it -- a
// handful of small queries -- and the test says so explicitly rather than
// failing obscurely if the machine is slower than that.
const feedOpenInstantLead = 500 * time.Millisecond

// TestListChangedMessages_ChangeInTheCursorsOwnInstantIsNotLost is the
// data-loss regression, and the reason the page query has an upper bound.
//
// SQLite stamps at millisecond resolution, so two writes sharing an instant are
// the common case rather than an exotic one. Once a consumer's cursor stands at
// (T, 40), a row stamped later at exactly T with an id below 40 fails BOTH arms
// of `content_changed_at >= T AND (content_changed_at > T OR id > 40)` on every
// future request: it is unreachable forever. Measured naturally at 19 permanent
// losses in 400 write/poll/write cycles on SQLite before the fix.
//
// The fixture pins the race instead of running it: the watermark is placed just
// ahead of the database clock, which is what one millisecond of that clock looks
// like from inside -- an instant that has not closed yet and can still take
// writes. The feed must refuse to hand out a cursor from it.
func TestListChangedMessages_ChangeInTheCursorsOwnInstantIsNotLost(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	low := seedFeedMessage(t, st, 1, time.Time{})
	high := seedFeedMessage(t, st, 2, time.Time{})
	require.Less(low, high, "the late write has to land on the LOWER of the two ids")

	openInstant := databaseClock(t, st).Add(feedOpenInstantLead)
	setWatermarkAt(t, st, openInstant, high)

	// The consumer polls to exhaustion and keeps the cursor it was handed.
	var cursor store.ChangedMessagesCursor
	firstPass := drainChangedMessages(t, st, 10, &cursor)
	require.Truef(databaseClock(t, st).Before(openInstant),
		"the poll outlasted the %s lead, so the instant closed before the second "+
			"write landed in it: the fixture is too slow for this machine, which is "+
			"not the same thing as the feed being correct", feedOpenInstantLead)
	stored := cursor

	// The same instant now takes a second write, on the lower id.
	setWatermarkAt(t, st, openInstant, low)

	// Once the clock has left the instant, the consumer polls again from the
	// cursor it stored.
	waitForDatabaseClockPast(t, st, openInstant)
	secondPass := drainChangedMessages(t, st, 10, &cursor)

	storedID, _ := stored.AfterID()
	assert.Containsf(secondPass, low,
		"message %d changed at %s, an instant the cursor (%s, id %d) already stood "+
			"in, and never came back: a feed that publishes a watermark from an "+
			"instant still open for writes loses those writes permanently. First "+
			"pass delivered %v, second pass %v",
		low, openInstant, stored.At(), storedID, firstPass, secondPass)
	assert.Containsf(append(append([]int64{}, firstPass...), secondPass...), high,
		"message %d must reach the consumer too", high)
}

// TestListChangedMessages_SurfacesBackfilledOldMessage covers what the existing
// message-date filters cannot: a message imported now carrying a years-old
// sent_at. Ordering by message date hides it behind everything newer; ordering
// by change time puts it in the very next page.
func TestListChangedMessages_SurfacesBackfilledOldMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	recent := seedFeedMessage(t, st, 1, time.Date(2026, 5, 4, 3, 2, 1, 0, time.UTC))
	settleFeedClock(t, st)
	first, err := st.ListChangedMessages(context.Background(), store.ChangedMessagesCursor{}, 10)
	require.NoError(err)
	require.Len(first.Messages, 1)
	require.Equal(recent, first.Messages[0].ID)
	cursor := first.Messages[0]

	// Now import a message that was sent long before everything already in the
	// archive -- a backfill, a restored mailbox, an old export.
	oldSentAt := time.Date(2005, 6, 7, 8, 9, 10, 0, time.UTC)
	backfilled := seedFeedMessage(t, st, 2, oldSentAt)
	settleFeedClock(t, st)

	next, err := st.ListChangedMessages(context.Background(),
		store.ChangedMessagesAfter(cursor.ContentChangedAt, cursor.ID), 10)
	require.NoError(err)
	require.Len(next.Messages, 1,
		"the incremental page must contain exactly the newly imported message")
	got := next.Messages[0]
	assert.Equal(backfilled, got.ID,
		"a message imported now must appear in the feed however old its sent_at is")
	require.NotNil(got.SentAt, "sent_at must be reported")
	assert.WithinDuration(oldSentAt, *got.SentAt, time.Second,
		"the feed reports the message's real sent_at, not its change time")
}

// TestListChangedMessages_OrdersByWatermarkThenID pins the ordering the cursor
// depends on: watermark first, id only as the tiebreak. Ids are assigned in
// insert order here and the watermarks deliberately are not, so an
// ORDER BY that fell back to id would produce a different sequence.
func TestListChangedMessages_OrdersByWatermarkThenID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	ids := make([]int64, 0, 5)
	for i := 1; i <= 5; i++ {
		ids = append(ids, seedFeedMessage(t, st, i, time.Time{}))
	}
	setWatermark(t, st, watermarkMiddle, ids[0], ids[2])
	setWatermark(t, st, watermarkEarly, ids[1], ids[3])
	setWatermark(t, st, watermarkLate, ids[4])

	page, err := st.ListChangedMessages(context.Background(), store.ChangedMessagesCursor{}, 10)
	require.NoError(err)
	got := make([]int64, 0, len(page.Messages))
	for _, m := range page.Messages {
		got = append(got, m.ID)
	}

	assert.Equal([]int64{ids[1], ids[3], ids[0], ids[2], ids[4]}, got,
		"the feed must order by (content_changed_at, id): watermark first, id only "+
			"to break ties within one instant")
}

// TestListChangedMessages_IncludesDeletedAndDedupHidden proves the feed applies
// no visibility filter and reports both lifecycle timestamps. A change feed that
// hid disappearances could not be used to mirror the archive, and a row filtered
// out after the cursor passed it is indistinguishable from the end of a page.
func TestListChangedMessages_IncludesDeletedAndDedupHidden(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	live := seedFeedMessage(t, st, 1, time.Time{})
	hidden := seedFeedMessage(t, st, 2, time.Time{})
	removed := seedFeedMessage(t, st, 3, time.Time{})

	hiddenAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	removedAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	setMessageTimestamp(t, st, hidden, "deleted_at", hiddenAt)
	setMessageTimestamp(t, st, removed, "deleted_from_source_at", removedAt)
	settleFeedClock(t, st)

	page, err := st.ListChangedMessages(context.Background(), store.ChangedMessagesCursor{}, 10)
	require.NoError(err)
	byID := make(map[int64]store.ChangedMessage, len(page.Messages))
	for _, m := range page.Messages {
		byID[m.ID] = m
	}

	require.Contains(byID, live, "a live message must appear in the feed")
	require.Contains(byID, hidden,
		"a dedup-hidden message (deleted_at) must appear: a consumer that never "+
			"learns it was hidden mirrors a message the archive no longer shows")
	require.Contains(byID, removed,
		"a source-deleted message (deleted_from_source_at) must appear: removals "+
			"are changes")

	assert.Nil(byID[live].DeletedAt, "a live message carries no deleted_at")
	assert.Nil(byID[live].DeletedFromSourceAt, "a live message carries no deleted_from_source_at")
	require.NotNil(byID[hidden].DeletedAt, "deleted_at must be reported, not just implied")
	assert.WithinDuration(hiddenAt, *byID[hidden].DeletedAt, time.Second)
	require.NotNil(byID[removed].DeletedFromSourceAt, "deleted_from_source_at must be reported")
	assert.WithinDuration(removedAt, *byID[removed].DeletedFromSourceAt, time.Second)
}

// TestListChangedMessages_ZeroCursorReturnsEverything covers the first-run path
// and the projection: a consumer starting from nothing gets every message, in
// cursor order, with the message fields the watermark actually covers.
func TestListChangedMessages_ZeroCursorReturnsEverything(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	sentAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	want := make([]int64, 0, 4)
	for i := 1; i <= 4; i++ {
		want = append(want, seedFeedMessage(t, st, i, sentAt))
	}
	settleFeedClock(t, st)

	page, err := st.ListChangedMessages(context.Background(), store.ChangedMessagesCursor{}, 10)
	require.NoError(err)
	require.Len(page.Messages, len(want),
		"a zero cursor is the first-run case: it must return the whole archive")

	got := make([]int64, 0, len(page.Messages))
	newest := time.Time{}
	for _, m := range page.Messages {
		got = append(got, m.ID)
		require.False(m.ContentChangedAt.IsZero(),
			"message %d has no watermark, so a consumer could never resume past it", m.ID)
		if m.ContentChangedAt.After(newest) {
			newest = m.ContentChangedAt
		}
	}
	assert.Equal(want, got, "a zero cursor returns every message in (watermark, id) order")

	first := page.Messages[0]
	assert.Equal("feed subject 1", first.Subject)
	assert.Equal("feed snippet 1", first.Snippet)
	assert.Equal("email", first.MessageType)
	assert.Equal("feed-msg-1", first.SourceMessageID)
	assert.Equal(int64(1001), first.SizeEstimate)
	require.NotNil(first.SentAt, "sent_at must be reported")
	assert.WithinDuration(sentAt, *first.SentAt, time.Second)

	assert.True(newest.Before(page.ServerTime),
		"server_time comes from the database clock and the page stops strictly "+
			"below it, so the newest watermark returned must precede it")
}

// TestListChangedMessages_UnreadableWatermarkDoesNotRewindTheCursor pins the
// floor on the reported watermark. nullableTimestamp maps a value it cannot
// parse to the zero time, and a consumer handed 0001-01-01 as its next cursor
// re-reads the entire archive on every poll, forever. No write path here
// produces such a value — direct SQL against the column can — so the defence is
// that a page can never report a watermark below the cursor it was read from.
//
// SQLite always, whatever backend the run targets: PostgreSQL's
// content_changed_at is a typed TIMESTAMPTZ and cannot hold a value its driver
// refuses to parse, so there is nothing to pin there.
func TestListChangedMessages_UnreadableWatermarkDoesNotRewindTheCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)

	id := seedFeedMessage(t, st, 1, time.Time{})
	settleFeedClock(t, st)

	// Lexically this sits inside the page's range — SQLite compares the column
	// as text — so the row is selected and only the Go-side conversion fails.
	setWatermark(t, st, "1999-13-45 99:99:99.999", id)

	since := time.Date(1995, 6, 7, 8, 9, 10, 0, time.UTC)
	page, err := st.ListChangedMessages(context.Background(), store.ChangedMessagesFrom(since), 10)
	require.NoError(err)
	require.Len(page.Messages, 1,
		"the row is still reported: dropping it would hide a real change")

	assert.Equal(since, page.Messages[0].ContentChangedAt,
		"a watermark the scanner cannot read must be floored at the cursor the "+
			"page was read from; reporting the zero time sends the consumer back to "+
			"year 1 and it replays the archive on every poll")
}

// TestListChangedMessages_ReportsTheStoredWatermarkNotTheRequestCursor keeps
// content_changed_at a property of the row.
//
// The floor that protects the cursor from an unreadable watermark must not
// rewrite a watermark the scanner READ perfectly well. A readable watermark can
// legitimately sort below the cursor on SQLite: SQLiteDialect.TimestampParam
// truncates the cursor to the millisecond the column stores, so a cursor
// carrying finer resolution — one derived from a client's own clock, or replayed
// from a PostgreSQL deployment, which stamps microseconds — selects rows below
// itself by design. Rewriting those rows publishes a change time that never
// happened, and two consumers polling with different cursors see different
// change times for the same row.
//
// SQLite only: PostgreSQL compares real timestamps, so no row it returns can be
// below the cursor and there is nothing to rewrite.
func TestListChangedMessages_ReportsTheStoredWatermarkNotTheRequestCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)

	id := seedFeedMessage(t, st, 1, time.Time{})
	stored := time.Date(2026, 7, 26, 10, 0, 0, 731_000_000, time.UTC)
	setWatermarkAt(t, st, stored, id)

	// A coarse cursor reads the row's real watermark: the truth to compare to.
	truth, err := st.ListChangedMessages(context.Background(), store.ChangedMessagesCursor{}, 10)
	require.NoError(err)
	require.Len(truth.Messages, 1)
	require.Equal(stored, truth.Messages[0].ContentChangedAt.UTC())

	// The same row, asked for with a cursor 400 microseconds later. SQLite
	// truncates that cursor to the stored millisecond, so the row still comes
	// back — and its watermark must be unchanged.
	finer := stored.Add(400 * time.Microsecond)
	page, err := st.ListChangedMessages(context.Background(), store.ChangedMessagesFrom(finer), 10)
	require.NoError(err)
	require.Len(page.Messages, 1, "the truncated cursor still selects the row")

	assert.Equal(stored, page.Messages[0].ContentChangedAt.UTC(),
		"content_changed_at is when the row changed, not what the caller asked for")
}

// TestListChangedMessages_EmptyPageStillCarriesServerTime is why the method
// returns a page rather than a slice: a caught-up consumer gets no rows, and the
// database clock is the only thing that tells it how far "caught up" reaches.
func TestListChangedMessages_EmptyPageStillCarriesServerTime(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	seedFeedMessage(t, st, 1, time.Time{})

	future := time.Now().UTC().Add(24 * time.Hour)
	page, err := st.ListChangedMessages(context.Background(), store.ChangedMessagesFrom(future), 10)
	require.NoError(err)
	require.Empty(page.Messages, "a cursor in the future is caught up by definition")

	assert.False(page.ServerTime.IsZero(),
		"an empty page must still carry server_time: it cannot be derived from rows")
	assert.WithinDuration(time.Now().UTC(), page.ServerTime, time.Minute,
		"server_time must be the database's current clock reading")
}

// TestListChangedMessages_NonPositiveLimitReturnsEmptyPage: a limit of zero or
// less asks for no rows, which is not an error condition.
func TestListChangedMessages_NonPositiveLimitReturnsEmptyPage(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	seedFeedMessage(t, st, 1, time.Time{})

	for _, limit := range []int{0, -1} {
		page, err := st.ListChangedMessages(context.Background(), store.ChangedMessagesCursor{}, limit)
		require.NoErrorf(err, "limit %d must not be an error", limit)
		require.Emptyf(page.Messages, "limit %d must return no rows", limit)
	}
}
