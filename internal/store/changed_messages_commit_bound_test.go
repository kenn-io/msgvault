package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sqliteutil"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// These tests are about ONE property: a change that is stamped before a page is
// read but committed after it must still reach the consumer.
//
// It is the property the feed did not have. Both backends stamp
// content_changed_at when the statement runs and publish the row when its
// transaction commits, so bounding a page at the database clock lets the cursor
// settle above a change that has not appeared yet — and the keyset lower bound
// (`>= since AND (> since OR id > since_id)`) then excludes it on every future
// request. Measured before the fix: 40 of 40 tombstones lost from one
// PostgreSQL run of MarkMessagesDeletedFromReader, and rows lost on SQLite
// wherever a committed write shared a millisecond with an uncommitted one on a
// higher id.
//
// The page is now bounded below the oldest write that could still commit
// instead, which the store publishes as ChangedMessagePage.CompleteThrough.

// changeFeedPollLimit is the page size these tests poll with. Large enough that
// no scenario here is split across pages, so a missing row means a missing row
// rather than an unfinished walk.
const changeFeedPollLimit = 200

// changeFeedConsumer is a consumer of the feed: a cursor plus what it has been
// told. It advances the cursor exactly as internal/api's handler does, floor
// included, so a loss these tests report is a loss a real client would see.
type changeFeedConsumer struct {
	cursor store.ChangedMessagesCursor
	// subjects is the latest subject the feed reported for each message.
	subjects map[int64]string
	// tombstoned records the messages the feed reported as deleted at source.
	tombstoned map[int64]bool
	mu         sync.Mutex
	pages      int
	rows       int
	// failure is the first thing a background poller found wrong. Assertions
	// belong on the test's own goroutine, so a poller running alongside the
	// writers records instead of asserting and the test checks it at the end.
	failure error
	// watching turns on recording of every complete_through the feed publishes,
	// so a test can prove after the fact that the bound never entered a range
	// of stamps that had not committed yet. Losing a row needs luck; the bound
	// crossing into that range is the thing that MAKES the loss possible, and
	// it can be checked on every page.
	watching atomic.Bool
	watched  []time.Time
}

func newChangeFeedConsumer() *changeFeedConsumer {
	return &changeFeedConsumer{
		subjects:   map[int64]string{},
		tombstoned: map[int64]bool{},
	}
}

// poll reads one page and advances the cursor. It asserts nothing, so a
// background goroutine can call it; every invariant it checks comes back as an
// error instead.
func (c *changeFeedConsumer) poll(st *store.Store) (store.ChangedMessagePage, error) {
	c.mu.Lock()
	since := c.cursor
	c.mu.Unlock()

	page, err := st.ListChangedMessages(context.Background(), since, changeFeedPollLimit)
	if err != nil {
		return page, fmt.Errorf("ListChangedMessages: %w", err)
	}
	if len(page.Messages) > changeFeedPollLimit {
		return page, fmt.Errorf("a page of %d rows exceeds the limit of %d",
			len(page.Messages), changeFeedPollLimit)
	}
	if page.CompleteThrough.After(page.ServerTime) {
		return page, fmt.Errorf(
			"complete_through %s is after server_time %s: the feed cannot be complete "+
				"through an instant the database clock has not reached",
			page.CompleteThrough, page.ServerTime)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.pages++
	c.rows += len(page.Messages)
	if c.watching.Load() {
		c.watched = append(c.watched, page.CompleteThrough)
	}
	for _, m := range page.Messages {
		c.subjects[m.ID] = m.Subject
		if m.DeletedFromSourceAt != nil {
			c.tombstoned[m.ID] = true
		}
	}
	if len(page.Messages) > 0 {
		last := page.Messages[len(page.Messages)-1]
		next := last.ContentChangedAt
		if next.Before(since.At()) {
			next = since.At() // the handler's floor
		}
		c.cursor = store.ChangedMessagesAfter(next, last.ID)
	}
	return page, nil
}

// cursorID is the id tiebreak the consumer's cursor carries, for the failure
// messages below. A cursor standing at the start of an instant carries none and
// reports 0.
func (c *changeFeedConsumer) cursorID() int64 {
	id, _ := c.cursor.AfterID()
	return id
}

// pollInBackground polls until stop closes, recording the first failure for the
// test's own goroutine to assert on. gap is how long it waits between polls.
func (c *changeFeedConsumer) pollInBackground(st *store.Store, stop <-chan struct{}, gap func(int) time.Duration) {
	for i := 0; ; i++ {
		select {
		case <-stop:
			return
		default:
		}
		if _, err := c.poll(st); err != nil {
			c.record(err)
			return
		}
		time.Sleep(gap(i))
	}
}

// record keeps the first failure a background goroutine saw.
func (c *changeFeedConsumer) record(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failure == nil {
		c.failure = err
	}
}

// mustPoll is poll on the test's own goroutine, where a failure should stop it.
func (c *changeFeedConsumer) mustPoll(t *testing.T, st *store.Store) store.ChangedMessagePage {
	t.Helper()
	page, err := c.poll(st)
	require.NoError(t, err, "poll the change feed")
	return page
}

// drain polls until a page comes back empty, which is what a consumer does
// between sleeps.
func (c *changeFeedConsumer) drain(t *testing.T, st *store.Store) store.ChangedMessagePage {
	t.Helper()
	for page := 0; ; page++ {
		require.Lessf(t, page, 500,
			"the feed did not terminate after %d pages: the cursor is not advancing", page)
		got := c.mustPoll(t, st)
		if len(got.Messages) == 0 {
			return got
		}
	}
}

// changeFeedCatchUpBudget is how long drainUntil waits for the feed to deliver
// what the database holds. It is generous on purpose and costs nothing when the
// feed is healthy: the failure these tests exist to catch is PERMANENT — a row
// the cursor has stepped over never arrives, at any deadline — so the only
// thing a short budget can add is a false alarm on a loaded machine.
const changeFeedCatchUpBudget = time.Minute

// drainUntil polls until done reports true or the budget runs out. The feed is
// allowed to take a moment — CompleteThrough advances only once the write that
// held it back has finished — but not forever.
func (c *changeFeedConsumer) drainUntil(t *testing.T, st *store.Store, done func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(changeFeedCatchUpBudget)
	for {
		c.drain(t, st)
		if done() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (c *changeFeedConsumer) subject(id int64) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.subjects[id]
}

// boundsPast returns every recorded complete_through that reached STRICTLY past
// instant — i.e. every page that claimed completeness through a stamp that had
// not committed when the page was read.
//
// Strictly: the page predicate is `content_changed_at < complete_through`, so a
// row stamped exactly at the bound is excluded and is not at risk. The
// distinction is not academic on SQLite, where stamps are milliseconds and the
// bound routinely lands in the same millisecond as the write that follows it.
func (c *changeFeedConsumer) boundsPast(instant time.Time) []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Time
	for _, at := range c.watched {
		if at.After(instant) {
			out = append(out, at)
		}
	}
	return out
}

// watchBounds records complete_through for the duration of fn, which is the
// window in which some transaction is known to be stamping rows without
// committing them.
func (c *changeFeedConsumer) watchBounds(fn func()) {
	c.mu.Lock()
	c.watched = nil
	c.mu.Unlock()
	c.watching.Store(true)
	defer c.watching.Store(false)
	fn()
}

// oldestWatermark returns the earliest content_changed_at among the given
// messages — the bottom of the stamp range a batched writer produced.
func oldestWatermark(t *testing.T, st *store.Store, ids []int64) time.Time {
	t.Helper()
	oldest := time.Time{}
	for _, id := range ids {
		var stamp nullableFeedTime
		require.NoError(t, st.DB().QueryRow(
			st.Rebind(`SELECT content_changed_at FROM messages WHERE id = ?`), id).Scan(&stamp),
			"read a watermark")
		require.Truef(t, stamp.valid, "message %d has no watermark", id)
		if oldest.IsZero() || stamp.at.Before(oldest) {
			oldest = stamp.at
		}
	}
	return oldest
}

// nullableFeedTime scans a watermark from either backend: PostgreSQL hands
// back a time.Time, SQLite the text the trigger wrote.
type nullableFeedTime struct {
	at    time.Time
	valid bool
}

func (n *nullableFeedTime) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		return nil
	case time.Time:
		n.at, n.valid = v.UTC(), true
		return nil
	case string:
		return n.parse(v)
	case []byte:
		return n.parse(string(v))
	default:
		return fmt.Errorf("unexpected watermark type %T", src)
	}
}

func (n *nullableFeedTime) parse(raw string) error {
	parsed, err := time.Parse(store.SQLiteTimestampLayout, raw)
	if err != nil {
		return fmt.Errorf("parse watermark %q: %w", raw, err)
	}
	n.at, n.valid = parsed.UTC(), true
	return nil
}

// writeSubject writes a message's subject on the pooled handle: an ordinary
// autocommit write that the triggers stamp and publish together. It returns the
// error rather than asserting, so a background writer can use it.
func writeSubject(st *store.Store, id int64, subject string) error {
	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages SET subject = ? WHERE id = ?`), subject, id)
	if err != nil {
		return fmt.Errorf("set subject for message %d: %w", id, err)
	}
	return nil
}

// setSubject is writeSubject on the test's own goroutine.
func setSubject(t *testing.T, st *store.Store, id int64, subject string) {
	t.Helper()
	require.NoError(t, writeSubject(st, id, subject))
}

// staggered spreads repeated background work over a range of microseconds
// without a random source: the point is only that the writers and the poller do
// not fall into lockstep, and a fixed stride is reproducible where a seeded RNG
// is merely repeatable.
func staggered(i, base, spread int) time.Duration {
	return time.Duration(base+(i*137)%spread) * time.Microsecond
}

// readWatermarkInTx reads a message's watermark from inside an uncommitted
// transaction — the only place a stamp that has not been published yet is
// visible.
func readWatermarkInTx(t *testing.T, st *store.Store, tx *sql.Tx, id int64) time.Time {
	t.Helper()
	var stamp any
	require.NoError(t, tx.QueryRow(
		st.Rebind(`SELECT content_changed_at FROM messages WHERE id = ?`), id).Scan(&stamp),
		"read the uncommitted watermark")
	switch v := stamp.(type) {
	case time.Time:
		return v.UTC()
	case string:
		parsed, err := time.Parse(store.SQLiteTimestampLayout, v)
		require.NoErrorf(t, err, "parse watermark %q", v)
		return parsed.UTC()
	case []byte:
		parsed, err := time.Parse(store.SQLiteTimestampLayout, string(v))
		require.NoErrorf(t, err, "parse watermark %q", v)
		return parsed.UTC()
	default:
		require.Failf(t, "unreadable watermark", "unexpected type %T", stamp)
		return time.Time{}
	}
}

// skipUnlessSecondWriterCanCommit skips a test whose arrangement needs one
// connection to commit while another holds an open write transaction. SQLite
// serialises writers, so the second write blocks until the first finishes and
// the scenario cannot be built there at all — a different thing from the feed
// behaving differently.
func skipUnlessSecondWriterCanCommit(t *testing.T, st *store.Store) {
	t.Helper()
	if !st.IsPostgreSQL() {
		t.Skip("SQLite serialises writers: a second writer cannot commit while a " +
			"write transaction is open, so this arrangement is unreachable")
	}
}

// TestListChangedMessages_SameInstantUncommittedChangeIsNotStranded is
// SQLite's deterministic form of the loss.
//
// Two rows share one watermark — ordinary at SQLite's millisecond resolution.
// The higher id is committed; the lower id is still inside an open transaction.
// A page bounded at the database clock happily returns the committed row (the
// clock left that instant long ago) and parks the cursor at (W, high). When the
// other row commits it is at (W, low): `content_changed_at >= W AND
// (content_changed_at > W OR id > high)` fails on both arms, on every request,
// forever.
//
// The bound is what has to fix this. Nothing about the row, the trigger, or the
// cursor encoding is wrong; the page simply must not publish a cursor from an
// instant that still has writes pending.
//
// SQLite only. PostgreSQL stamps microseconds, so two writes sharing an instant
// is not its route to this loss; a transaction that outlives a poll is, and
// TestListChangedMessages_LaterCommitDoesNotStrandAnEarlierPendingChange is
// that. The arrangement here would also misrepresent PostgreSQL's bound, which
// is the oldest open transaction's START: pinning a watermark to an instant
// before its own transaction began is something no writer does — every real
// stamp comes from the database clock while the transaction is running.
func TestListChangedMessages_SameInstantUncommittedChangeIsNotStranded(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "millisecond stamps make same-instant ties ordinary on SQLite alone")
	st := testutil.NewTestStore(t)

	low := seedFeedMessage(t, st, 1, time.Time{})
	high := seedFeedMessage(t, st, 2, time.Time{})
	require.Less(low, high, "the pending change has to land on the LOWER of the two ids")
	settleFeedClock(t, st)

	consumer := newChangeFeedConsumer()
	consumer.drain(t, st)

	// One instant, above everything the consumer has seen. Both writes set the
	// subject AND the watermark in one statement, which is how the instant is
	// pinned: naming content_changed_at makes OLD and NEW differ, so the
	// content trigger's WHEN guard declines and does not restamp the row with
	// its own reading.
	shared := databaseClock(t, st)
	var sharedParam any = shared.UTC()
	if !st.IsPostgreSQL() {
		sharedParam = shared.UTC().Format(store.SQLiteTimestampLayout)
	}
	const pinned = `UPDATE messages SET subject = ?, content_changed_at = ? WHERE id = ?`
	_, err := st.DB().Exec(st.Rebind(pinned), "high-changed", sharedParam, high)
	require.NoError(err, "the committed change")

	// The same instant takes a second change on the lower id, which stays
	// uncommitted while the consumer polls. On SQLite this is the write holding
	// the single writer slot; on PostgreSQL it is one transaction among many.
	tx, err := st.DB().BeginTx(context.Background(), nil)
	require.NoError(err, "begin the pending write")
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(st.Rebind(pinned), "low-changed", sharedParam, low)
	require.NoError(err, "the pending change")

	// The consumer polls while it is pending, then again once it lands.
	waitForDatabaseClockPast(t, st, shared)
	consumer.drain(t, st)
	require.NoError(tx.Commit(), "commit the pending change")

	delivered := consumer.drainUntil(t, st, func() bool {
		return consumer.subject(low) == "low-changed"
	})
	assert.Equal("high-changed", consumer.subject(high),
		"the committed row of the pair must reach the consumer")

	assert.Truef(delivered,
		"message %d changed at %s, the same instant as message %d, and committed "+
			"after the consumer had already been served that instant. It never came "+
			"back: a page that publishes a cursor from an instant still open for "+
			"commits loses every change still pending in it. Cursor is now (%s, %d)",
		low, shared, high, consumer.cursor.At(), consumer.cursorID())
}

// TestListChangedMessages_LaterCommitDoesNotStrandAnEarlierPendingChange is the
// shape that cost 40 of 40 tombstones: a batched writer holds one transaction
// open across a whole run (MarkMessagesDeletedFromReader streams its input, so
// its transaction lives as long as the network does) while ordinary autocommit
// traffic keeps committing alongside it. Every one of the batch's stamps is
// older than the traffic the consumer is being served, so the cursor climbs
// straight over the lot.
//
// PostgreSQL only, and not because SQLite behaves differently: SQLite has one
// writer, so no second connection can commit anything while the transaction is
// open, and the arrangement cannot be built at all. SQLite's version of this
// loss is the same-instant test above.
func TestListChangedMessages_LaterCommitDoesNotStrandAnEarlierPendingChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	skipUnlessSecondWriterCanCommit(t, st)

	pending := seedFeedMessage(t, st, 1, time.Time{})
	traffic := seedFeedMessage(t, st, 2, time.Time{})
	settleFeedClock(t, st)

	consumer := newChangeFeedConsumer()
	consumer.drain(t, st)

	tx, err := st.DB().BeginTx(context.Background(), nil)
	require.NoError(err, "begin the batched write")
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(
		st.Rebind(`UPDATE messages SET subject = ? WHERE id = ?`), "batched-change", pending)
	require.NoError(err, "stamp the batched change")
	pendingAt := readWatermarkInTx(t, st, tx, pending)

	// Ordinary traffic commits while the batch is still open, so its watermark
	// is strictly newer than the batch's.
	setSubject(t, st, traffic, "ordinary-traffic")
	waitForDatabaseClockPast(t, st, pendingAt)

	page := consumer.mustPoll(t, st)
	require.Falsef(page.CompleteThrough.After(pendingAt),
		"the feed reported itself complete through %s while a change stamped %s "+
			"was still uncommitted", page.CompleteThrough, pendingAt)

	consumer.drain(t, st)
	require.NoError(tx.Commit(), "commit the batched write")

	delivered := consumer.drainUntil(t, st, func() bool {
		return consumer.subject(pending) == "batched-change"
	})

	assert.Truef(delivered,
		"message %d was stamped %s inside a transaction that committed after the "+
			"consumer had been served newer traffic. The feed reported %q for it "+
			"and never corrected itself: an entire batched write is lost this way. "+
			"Cursor is now (%s, %d)",
		pending, pendingAt, consumer.subject(pending), consumer.cursor.At(), consumer.cursorID())
}

// TestListChangedMessages_CompleteThroughHoldsBelowAPendingChange pins what the
// page publishes about itself, which is the whole liveness contract.
//
// The feed's guarantee is not "you have everything", it is "you have everything
// committed before complete_through". While a write transaction is open the
// feed cannot advance past the instant that transaction began, and a consumer
// has no other way to tell that state apart from being caught up: both answer
// with no rows. So complete_through must actually stop below a pending change,
// and must actually move once it lands.
func TestListChangedMessages_CompleteThroughHoldsBelowAPendingChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	id := seedFeedMessage(t, st, 1, time.Time{})
	settleFeedClock(t, st)
	consumer := newChangeFeedConsumer()
	caughtUp := consumer.drain(t, st)
	require.False(caughtUp.CompleteThrough.IsZero(),
		"a quiet database is complete through some instant: the feed has proof "+
			"every stamp in it has committed")

	tx, err := st.DB().BeginTx(context.Background(), nil)
	require.NoError(err, "begin the pending write")
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(
		st.Rebind(`UPDATE messages SET subject = ? WHERE id = ?`), "pending", id)
	require.NoError(err, "stamp the pending change")
	pendingAt := readWatermarkInTx(t, st, tx, id)

	held := consumer.drain(t, st)
	assert.Falsef(held.CompleteThrough.After(pendingAt),
		"complete_through reached %s while a change stamped %s was still "+
			"uncommitted: a consumer told the feed is complete through an instant "+
			"has no reason ever to look below it again", held.CompleteThrough, pendingAt)
	assert.NotEqual("pending", consumer.subject(id),
		"a change that has not committed must not be reported")

	require.NoError(tx.Commit(), "commit the pending change")
	require.True(consumer.drainUntil(t, st, func() bool {
		return consumer.subject(id) == "pending"
	}), "the change must arrive once it commits")

	final := consumer.drain(t, st)
	assert.Truef(final.CompleteThrough.After(pendingAt),
		"complete_through stayed at %s after the write finished: a feed whose "+
			"bound never recovers is stalled, not merely cautious", final.CompleteThrough)
}

// TestListChangedMessages_DeletionRunTombstonesAllArrive runs the production
// path that first exposed this, through the store's own public API only.
//
// MarkMessagesDeletedFromReader deliberately holds ONE transaction across a
// streamed deletion run, so its whole batch of tombstones is stamped early and
// published late. Racing it with an ordinary import loop and a consumer polling
// the way the HTTP handler does lost all 40 tombstones on PostgreSQL. A mirror
// built on this feed would keep 40 deleted messages forever and never learn
// otherwise.
//
// Two assertions, and the second is the sharper one. "No tombstone was lost"
// depends on the race actually being lost, which is luck. "complete_through
// never reached into the batch's stamp range while the batch was uncommitted"
// is the property that MAKES the loss possible, and it is checked on every page
// the consumer reads during the run. A bound that enters that range while the
// transaction is open is a defect whether or not a row happened to fall through
// it — and it is the only way the feed can deliver a PREFIX of one batch and
// strand the rest, which is the shape a loss of this kind takes.
func TestListChangedMessages_DeletionRunTombstonesAllArrive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	src, err := st.GetOrCreateSource("gmail", "deleterun@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(
		src.ID, "delete-run-conv", "email_thread", "Delete run")
	require.NoError(err, "EnsureConversationWithType")

	const doomedCount = 20
	doomed := make([]int64, 0, doomedCount)
	for i := range doomedCount {
		id, err := st.UpsertMessage(&store.Message{
			SourceID:        src.ID,
			SourceMessageID: fmt.Sprintf("doomed-%d", i),
			ConversationID:  convID,
			MessageType:     "email",
			SizeEstimate:    int64(100 + i),
		})
		require.NoError(err, "UpsertMessage")
		doomed = append(doomed, id)
	}
	settleFeedClock(t, st)

	consumer := newChangeFeedConsumer()
	consumer.drain(t, st)

	stop := make(chan struct{})
	var workers sync.WaitGroup

	workers.Go(func() {
		consumer.pollInBackground(st, stop, func(int) time.Duration { return 500 * time.Microsecond })
	})

	// Ordinary sync traffic arriving while the deletion run is open. On
	// PostgreSQL these commit alongside it; on SQLite they queue behind it.
	workers.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := st.UpsertMessage(&store.Message{
				SourceID:        src.ID,
				SourceMessageID: fmt.Sprintf("incoming-%d", i),
				ConversationID:  convID,
				MessageType:     "email",
				SizeEstimate:    10,
			}); err != nil {
				consumer.record(fmt.Errorf("incoming sync traffic: %w", err))
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	})

	// The deletion list streams in, which is why the transaction outlives the
	// consumer's poll interval.
	reader, writer := io.Pipe()
	go func() {
		for i := range doomedCount {
			_, _ = fmt.Fprintf(writer, "doomed-%d\n", i)
			time.Sleep(5 * time.Millisecond)
		}
		_ = writer.Close()
	}()
	// Record complete_through on every page read while the deletion transaction
	// is open. Everything it stamps is uncommitted for the whole of this call.
	consumer.watchBounds(func() {
		require.NoError(st.MarkMessagesDeletedFromReader(src.ID, reader, 4),
			"MarkMessagesDeletedFromReader")
	})

	close(stop)
	workers.Wait()
	require.NoError(consumer.failure, "a background worker failed")

	missing := func() []int64 {
		consumer.mu.Lock()
		defer consumer.mu.Unlock()
		var out []int64
		for _, id := range doomed {
			if !consumer.tombstoned[id] {
				out = append(out, id)
			}
		}
		return out
	}
	consumer.drainUntil(t, st, func() bool { return len(missing()) == 0 })

	assert.Emptyf(missing(),
		"the feed never reported these tombstones. MarkMessagesDeletedFromReader "+
			"holds one transaction across the whole run, so every tombstone is "+
			"stamped before the consumer's page and committed after it; a feed "+
			"bounded at the database clock steps over the lot. %d pages, %d rows.",
		consumer.pages, consumer.rows)

	// The stamps are only readable now that the run has committed, but they
	// were made during it, so any bound recorded above that reached them was
	// wrong when it was published.
	oldest := oldestWatermark(t, st, doomed)
	trespassing := consumer.boundsPast(oldest)
	assert.Emptyf(trespassing,
		"%d of %d pages read during the deletion run reported the feed complete "+
			"through an instant past %s, the oldest stamp that run made — "+
			"while every one of those stamps was still uncommitted. A consumer "+
			"handed such a cursor has no reason to look below it again, which is "+
			"how a batch is delivered in part and stranded in part. First "+
			"trespassing bound: %v",
		len(trespassing), consumer.pages, oldest, firstOrZero(trespassing))
}

// firstOrZero keeps a failure message readable when a long slice would not be.
func firstOrZero(times []time.Time) time.Time {
	if len(times) == 0 {
		return time.Time{}
	}
	return times[0]
}

// TestListChangedMessages_BoundNeverEntersABatchesStampRange is the
// deterministic form of the deletion run, and the one that does not need a race
// to go wrong.
//
// One transaction issues several stamping statements one at a time — the shape
// of a chunked batch reading its input from a pipe — while autocommit traffic
// commits alongside it and a consumer polls after every statement. The
// feed must never, on any of those pages, claim to be complete through an
// instant at or past the batch's FIRST stamp: everything from that stamp
// onwards is uncommitted for the whole window, so a cursor placed inside the
// range strands whatever sits above it.
//
// Unlike the tombstone count, this does not depend on the race being lost. A
// bound that steps into the range is wrong on every poll while the transaction
// remains open, so one poll after each statement observes it directly.
func TestListChangedMessages_BoundNeverEntersABatchesStampRange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	const batchSize = 10
	batch := make([]int64, 0, batchSize)
	for i := 1; i <= batchSize; i++ {
		batch = append(batch, seedFeedMessage(t, st, i, time.Time{}))
	}
	traffic := seedFeedMessage(t, st, batchSize+1, time.Time{})
	settleFeedClock(t, st)

	consumer := newChangeFeedConsumer()
	consumer.drain(t, st)

	tx, err := st.DB().BeginTx(context.Background(), nil)
	require.NoError(err, "begin the batch")
	defer func() { _ = tx.Rollback() }()

	stop := make(chan struct{})
	var workers sync.WaitGroup
	var trafficErr error
	if st.IsPostgreSQL() {
		// Autocommit traffic gives the cursor something to climb while the batch
		// is pending. On SQLite it cannot run at all — the batch holds the single
		// writer slot — and the equivalent there is the same-instant tie covered
		// by TestListChangedMessages_SameInstantUncommittedChangeIsNotStranded.
		workers.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if err := writeSubject(st, traffic, fmt.Sprintf("traffic-%d", i)); err != nil {
					trafficErr = err
					return
				}
				time.Sleep(time.Millisecond)
			}
		})
	}

	var firstStamp time.Time
	consumer.watchBounds(func() {
		for chunk, id := range batch {
			_, err := tx.Exec(st.Rebind(`UPDATE messages SET subject = ? WHERE id = ?`),
				fmt.Sprintf("batched-%d", chunk), id)
			require.NoError(err, "stamp chunk %d", chunk)
			if firstStamp.IsZero() {
				firstStamp = readWatermarkInTx(t, st, tx, id)
			}
			// Poll across the gap the way a consumer does between chunks.
			consumer.mustPoll(t, st)
		}
	})

	close(stop)
	workers.Wait()
	require.NoError(trafficErr, "the autocommit writer failed")

	trespassing := consumer.boundsPast(firstStamp)
	require.Emptyf(trespassing,
		"%d of the pages read while the batch was pending reported the feed "+
			"complete through an instant past %s, the batch's first stamp. "+
			"Everything from there up was uncommitted for the whole window, so a "+
			"consumer resuming from such a cursor never sees it. First trespassing "+
			"bound: %v", len(trespassing), firstStamp, firstOrZero(trespassing))

	require.NoError(tx.Commit(), "commit the batch")
	delivered := consumer.drainUntil(t, st, func() bool {
		for chunk, id := range batch {
			if consumer.subject(id) != fmt.Sprintf("batched-%d", chunk) {
				return false
			}
		}
		return true
	})
	var stranded []string
	for chunk, id := range batch {
		if got := consumer.subject(id); got != fmt.Sprintf("batched-%d", chunk) {
			stranded = append(stranded, fmt.Sprintf("id=%d want=batched-%d got=%q", id, chunk, got))
		}
	}
	assert.Truef(delivered,
		"the batch committed as one statement-by-statement transaction and part "+
			"of it never arrived: %v", stranded)
}

// TestListChangedMessages_ConcurrentTransactionalWritersLoseNothing is the
// unstaged version: writers, a poller, no fixture arranging anything. One
// writer batches (stamp, hold, commit) on the LOW ids and one writes plain
// autocommit statements on the high ids, which is the traffic mix an importer
// and a live sync produce together.
//
// It reaches the loss by both routes at once. On PostgreSQL the batched
// writer's transaction outlives a poll. On SQLite writers are serialised, so
// instead the autocommit writer commits in the same millisecond the batched
// writer has already stamped, on a higher id. Measured before the fix: rows
// lost on both backends.
//
// Every message's FINAL value must reach the consumer. Intermediate values may
// be missed — the feed reports that a message changed, not each change.
func TestListChangedMessages_ConcurrentTransactionalWritersLoseNothing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	const messages = 12
	ids := make([]int64, 0, messages)
	for i := 1; i <= messages; i++ {
		ids = append(ids, seedFeedMessage(t, st, i, time.Time{}))
	}
	settleFeedClock(t, st)
	batched, plain := ids[:6], ids[6:]

	consumer := newChangeFeedConsumer()
	consumer.drain(t, st)

	var wantMu sync.Mutex
	want := map[int64]string{}
	record := func(id int64, value string) {
		wantMu.Lock()
		defer wantMu.Unlock()
		want[id] = value
	}

	stop := make(chan struct{})
	var workers sync.WaitGroup

	workers.Go(func() {
		consumer.pollInBackground(st, stop, func(i int) time.Duration { return staggered(i, 200, 800) })
	})

	workers.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			id := plain[i%len(plain)]
			value := fmt.Sprintf("plain-%d", i)
			if err := writeSubject(st, id, value); err != nil {
				consumer.record(err)
				return
			}
			record(id, value)
			time.Sleep(staggered(i, 0, 90))
		}
	})

	const rounds = 200
	for round := range rounds {
		id := batched[round%len(batched)]
		value := fmt.Sprintf("batched-%d", round)
		tx, err := st.DB().BeginTx(context.Background(), nil)
		require.NoError(err, "begin batched write")
		_, err = tx.Exec(st.Rebind(`UPDATE messages SET subject = ? WHERE id = ?`), value, id)
		require.NoError(err, "batched write")
		time.Sleep(3 * time.Millisecond) // stamped, not yet published
		require.NoError(tx.Commit(), "commit batched write")
		record(id, value)
		time.Sleep(staggered(round, 0, 900))
	}

	close(stop)
	workers.Wait()
	require.NoError(consumer.failure, "a background worker failed")

	lost := func() []string {
		wantMu.Lock()
		defer wantMu.Unlock()
		var out []string
		for id, value := range want {
			if got := consumer.subject(id); got != value {
				out = append(out, fmt.Sprintf("message %d is %q, feed last said %q",
					id, value, got))
			}
		}
		return out
	}
	consumer.drainUntil(t, st, func() bool { return len(lost()) == 0 })

	// The two ways this can end look identical from the outside — the feed
	// produced nothing — so report the numbers that tell them apart. A row
	// stamped BELOW the cursor was stepped over and is lost; a row stamped
	// ABOVE complete_through has not been published yet and the drain simply
	// ran out of patience.
	final := consumer.mustPoll(t, st)
	assert.Emptyf(lost(),
		"the feed's last word on these messages is not what the database holds, "+
			"and it has stopped producing pages. %d pages, %d rows, %d messages "+
			"tracked; complete_through %s, clock %s, cursor (%s, %d); the rows "+
			"themselves are stamped %v",
		consumer.pages, consumer.rows, len(want),
		final.CompleteThrough, final.ServerTime, consumer.cursor.At(), consumer.cursorID(),
		storedWatermarks(t, st, ids))
}

// storedWatermarks reads what the database actually holds for each message, so
// a delivery failure can be pinned on the cursor, the bound, or neither.
func storedWatermarks(t *testing.T, st *store.Store, ids []int64) map[int64]string {
	t.Helper()
	out := make(map[int64]string, len(ids))
	for _, id := range ids {
		var stamp any
		if err := st.DB().QueryRow(
			st.Rebind(`SELECT content_changed_at FROM messages WHERE id = ?`), id,
		).Scan(&stamp); err != nil {
			out[id] = fmt.Sprintf("unreadable: %v", err)
			continue
		}
		out[id] = fmt.Sprint(stamp)
	}
	return out
}

// TestListChangedMessages_UnresolvableMessagesTableIsRefused pins the direction
// the bound query fails in.
//
// Every filter in that query NARROWS the set of transactions that hold the
// bound back, so a filter that stops matching does not raise an error — it
// quietly returns the database clock, which is the bound this whole mechanism
// exists to replace. `messages` is resolved by name through the session's
// search_path, so it is the filter most likely to stop matching in a real
// deployment (a search_path change, a rename, a schema-qualified deployment
// gone wrong). It must refuse rather than serve pages nobody can trust.
//
// PostgreSQL only: the SQLite bound is a write-lock probe, which cannot fail
// this way — it either takes the lock or it does not.
func TestListChangedMessages_UnresolvableMessagesTableIsRefused(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	skipUnlessSecondWriterCanCommit(t, st)

	seedFeedMessage(t, st, 1, time.Time{})
	settleFeedClock(t, st)
	_, err := st.ListChangedMessages(context.Background(), store.ChangedMessagesCursor{}, 10)
	require.NoError(err, "the feed works before the table moves out of reach")

	_, err = st.DB().Exec(`ALTER TABLE messages RENAME TO messages_moved`)
	require.NoError(err, "move the table out of the bound query's reach")
	defer func() {
		_, err := st.DB().Exec(`ALTER TABLE messages_moved RENAME TO messages`)
		require.NoError(err, "put the table back")
	}()

	_, err = st.ListChangedMessages(context.Background(), store.ChangedMessagesCursor{}, 10)
	require.Error(err,
		"a bound that cannot see which transactions are writing must refuse the "+
			"page; serving one bounded at the clock is the original data loss, "+
			"reached by a filter matching nothing instead of by a decision")
	assert.Contains(err.Error(), "search_path",
		"the error must name what went wrong, so an operator is not left with a "+
			"bare `relation does not exist` from a query they did not write")
}

// TestListChangedMessages_BlockedFirstProbePublishesNoBound pins the one state
// in which the feed has no bound at all rather than a stale one.
//
// SQLite's bound is a proof, not an observation: the dialect takes the write
// lock, reads the clock under it, and remembers that instant. A process that
// has never completed that probe — a server restarted while an import held the
// lock — has nothing to stand on, and the honest answer is that nothing is
// known to have committed. It publishes the zero time to say so.
//
// That value is deliberate and load-bearing in both directions. It must not
// become "now" (that would publish rows the probe never proved committed), and
// it must not become an error (a restart during a bulk import would then take
// the endpoint down for the length of the import). What it must do is carry no
// rows, leave the consumer's cursor exactly where it was, and resolve at the
// first quiet moment. Callers are told not to derive a lag from it, because
// subtracting it from the clock saturates time.Duration.
//
// SQLite only: PostgreSQL reads its bound from pg_stat_activity, which needs no
// lock and cannot be blocked this way.
func TestListChangedMessages_BlockedFirstProbePublishesNoBound(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "the SQLite bound is a write-lock probe; PostgreSQL has none")

	path := filepath.Join(t.TempDir(), "feed.db")
	seedStore, err := store.OpenForTest(path)
	require.NoError(err, "open the archive")
	require.NoError(seedStore.InitSchema(), "init schema")
	seeded := seedFeedMessage(t, seedStore, 1, time.Time{})
	settleFeedClock(t, seedStore)
	require.NoError(seedStore.Close(), "close the archive")

	// A restarted server: a new store, so a dialect that has never proved the
	// database quiescent.
	st, err := store.OpenForTest(path)
	require.NoError(err, "reopen the archive")
	defer func() { _ = st.Close() }()

	holder, err := sql.Open(sqliteutil.DriverName(), path+"?_busy_timeout=30000")
	require.NoError(err, "open the writer that holds the lock")
	defer func() { _ = holder.Close() }()
	held, err := holder.Conn(context.Background())
	require.NoError(err, "pin the holding connection")
	_, err = held.ExecContext(context.Background(), "BEGIN IMMEDIATE")
	require.NoError(err, "take the write lock")

	consumer := newChangeFeedConsumer()
	page := consumer.mustPoll(t, st)
	assert.True(page.CompleteThrough.IsZero(),
		"with the write lock held and no earlier proof, the feed knows of nothing "+
			"that has committed; publishing the clock here would hand out a bound "+
			"the probe never established")
	assert.Empty(page.Messages,
		"a feed complete through nothing must publish nothing")
	assert.True(consumer.cursor.At().IsZero(),
		"and it must leave the consumer's cursor alone: an empty page is not "+
			"evidence of being caught up")
	assert.False(page.ServerTime.IsZero(),
		"server_time still comes from the database, so a consumer can see that the "+
			"clock is moving while the bound is not")

	_, err = held.ExecContext(context.Background(), "ROLLBACK")
	require.NoError(err, "release the write lock")
	require.NoError(held.Close(), "return the holding connection")
	require.True(
		consumer.drainUntil(t, st, func() bool { return consumer.subject(seeded) != "" }),
		"the first quiet moment must establish the bound and release the archive: "+
			"a feed that never recovers from a busy start is not conservative, it is "+
			"broken")
}

// TestListChangedMessages_ConcurrentWithActiveImportMakesProgress drives the
// feed against the production import path rather than raw SQL. Every write here
// goes through UpsertMessage, so it fires trg_messages_last_modified and the
// content_changed_at trigger exactly as an import does — and the feed's
// commit-bound probe contends with those writes for SQLite's single write lock.
//
// The other contention tests in this file issue direct UPDATEs or hold the lock
// by hand. Neither shape can catch a defect that only appears when the real
// write path holds the lock across a trigger.
//
// Liveness here means the feed keeps DELIVERING, not merely that the call keeps
// returning. A build whose every BEGIN IMMEDIATE probe lost the race to the
// importer would still return a page on every poll — server_time is read from
// the clock unconditionally and a timed-out probe falls back rather than
// erroring — while complete_through stayed zero and every page stayed empty for
// the whole length of the import. That is the feature completely broken, and it
// is the shape a poll-count-only assertion cannot see. So this asserts that the
// bound gets established, that it advances, that pages carry rows, that those
// rows include messages the importer wrote DURING the run, and that the
// importer itself got writes in. Losing no row under contention is a different
// property, and ConcurrentTransactionalWritersLoseNothing owns it.
func TestListChangedMessages_ConcurrentWithActiveImportMakesProgress(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	// Seed so the feed has a non-empty starting point.
	var lastSeededID int64
	for i := 1; i <= 4; i++ {
		lastSeededID = seedFeedMessage(t, st, i, time.Time{})
	}
	settleFeedClock(t, st)

	stop := make(chan struct{})
	var workers sync.WaitGroup
	stopWorkers := sync.OnceFunc(func() {
		close(stop)
		workers.Wait()
	})
	t.Cleanup(stopWorkers)

	// Importer: the production write path, running continuously. It reports
	// through a channel and a counter rather than asserting, because require's
	// failure path is runtime.Goexit and that is not legal off the test's own
	// goroutine (see insertFeedMessage).
	importErrs := make(chan error, 1)
	var imported atomic.Int64
	workers.Go(func() {
		for i := 100; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := insertFeedMessage(st, i, time.Time{}); err != nil {
				importErrs <- fmt.Errorf("import feed message %d: %w", i, err)
				return
			}
			imported.Add(1)
		}
	})

	// Reader: drain the feed the way the handler does, cursor and all, while the
	// import runs. Each poll takes the write lock briefly for the commit bound.
	consumer := newChangeFeedConsumer()
	var (
		polls                       int
		firstBound                  time.Time
		lastBound                   time.Time
		deliveredFromImporterDuring bool
	)
	deadline := time.Now().Add(changeFeedCatchUpBudget)
	for {
		page, err := consumer.poll(st)
		require.NoError(err, "the feed must not error while an import holds the write lock")
		require.False(page.ServerTime.IsZero(), "a poll that returns must report a server time")
		polls++
		if !page.CompleteThrough.IsZero() {
			if firstBound.IsZero() {
				firstBound = page.CompleteThrough
			}
			if page.CompleteThrough.After(lastBound) {
				lastBound = page.CompleteThrough
			}
		}
		for _, message := range page.Messages {
			if message.ID > lastSeededID {
				deliveredFromImporterDuring = true
				break
			}
		}
		if imported.Load() > 0 && !firstBound.IsZero() &&
			lastBound.After(firstBound) && deliveredFromImporterDuring {
			break
		}
		if time.Now().After(deadline) {
			break
		}
	}
	stopWorkers()
	close(importErrs)

	require.NoError(<-importErrs, "the import path must not fail while the feed polls")
	require.Positive(imported.Load(),
		"the importer never completed a write before the timeout, so nothing here was under "+
			"contention and the rest of this test proves nothing")

	require.Falsef(firstBound.IsZero(),
		"not one of %d polls published a commit bound: every probe lost the write "+
			"lock to the importer, so complete_through stayed at zero and the feed "+
			"can never return a row while an import is running", polls)
	assert.Truef(lastBound.After(firstBound),
		"the commit bound stood still at %s across %d polls before the timeout: a bound that "+
			"never advances holds every later change back indefinitely", firstBound, polls)

	consumer.mu.Lock()
	rows, delivered := consumer.rows, len(consumer.subjects)
	var fromImporter int
	for id := range consumer.subjects {
		if id > lastSeededID {
			fromImporter++
		}
	}
	consumer.mu.Unlock()

	assert.Positive(rows,
		"the feed returned pages but never a row while an import was writing "+
			"continuously: a feed complete through an instant it never reaches "+
			"delivers nothing")
	assert.Positivef(fromImporter,
		"the feed delivered %d messages, all of them seeded before the import "+
			"started: none of the %d rows the importer wrote during the run ever "+
			"reached the consumer", delivered, imported.Load())
}
