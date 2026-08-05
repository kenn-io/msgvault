package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// newChangesServer builds a server over a real store on whichever backend the
// test run targets, so the feed is exercised through the same router, handler,
// and SQL a client would reach.
func newChangesServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st := testutil.NewTestStore(t)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Logger: testLogger(),
	})
	relaxChangeFeedRateLimit(t, srv)
	return srv, st
}

func relaxChangeFeedRateLimit(t *testing.T, srv *Server) {
	t.Helper()
	srv.changesRateLimiter.Close()
	srv.changesRateLimiter = NewRateLimiter(1_000_000, 1_000_000)
	t.Cleanup(func() {
		srv.rateLimiter.Close()
		srv.changesRateLimiter.Close()
	})
}

// seedChangedMessages inserts count messages through the same UpsertMessage
// path importers use, so the INSERT trigger stamps content_changed_at exactly
// as it would in production. Ids come back in insert order.
func seedChangedMessages(t *testing.T, st *store.Store, count int) []int64 {
	t.Helper()
	src, err := st.GetOrCreateSource("gmail", "changes@example.com")
	require.NoError(t, err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(
		src.ID, "changes-conv", "email_thread", "Changes thread")
	require.NoError(t, err, "EnsureConversationWithType")

	ids := make([]int64, 0, count)
	for i := 1; i <= count; i++ {
		id, err := st.UpsertMessage(&store.Message{
			SourceID:        src.ID,
			SourceMessageID: fmt.Sprintf("changes-msg-%d", i),
			ConversationID:  convID,
			MessageType:     "email",
			Subject:         sql.NullString{String: fmt.Sprintf("changes subject %d", i), Valid: true},
			Snippet:         sql.NullString{String: fmt.Sprintf("changes snippet %d", i), Valid: true},
			SizeEstimate:    int64(1000 + i),
		})
		require.NoError(t, err, "UpsertMessage")
		ids = append(ids, id)
	}
	return ids
}

// seedMoreChangedMessages inserts count further messages through UpsertMessage,
// under a source_message_id prefix of the caller's choosing so a second batch
// creates new rows instead of re-upserting the first one (an identical upsert
// is value-guarded and would not move any watermark).
func seedMoreChangedMessages(t *testing.T, st *store.Store, prefix string, count int) []int64 {
	t.Helper()
	src, err := st.GetOrCreateSource("gmail", "changes@example.com")
	require.NoError(t, err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(
		src.ID, "changes-conv", "email_thread", "Changes thread")
	require.NoError(t, err, "EnsureConversationWithType")

	ids := make([]int64, 0, count)
	for i := 1; i <= count; i++ {
		id, err := st.UpsertMessage(&store.Message{
			SourceID:        src.ID,
			SourceMessageID: fmt.Sprintf("%s-%d", prefix, i),
			ConversationID:  convID,
			MessageType:     "email",
			Subject:         sql.NullString{String: fmt.Sprintf("%s subject %d", prefix, i), Valid: true},
			SizeEstimate:    int64(2000 + i),
		})
		require.NoError(t, err, "UpsertMessage")
		ids = append(ids, id)
	}
	return ids
}

// setChangesWatermark forces content_changed_at to an exact value. The
// statement names only content_changed_at, so no UPDATE OF list matches and the
// trigger does not overwrite it.
func setChangesWatermark(t *testing.T, st *store.Store, value string, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		_, err := st.DB().Exec(
			st.Rebind(`UPDATE messages SET content_changed_at = ? WHERE id = ?`), value, id)
		require.NoErrorf(t, err, "set content_changed_at for message %d", id)
	}
}

// setChangesWatermarkAt forces content_changed_at to an exact INSTANT rather
// than a fixed literal, binding it the way each backend compares it: SQLite
// stores the trigger's textual format and compares lexically, PostgreSQL parses
// a real timestamptz. Tests that have to place a watermark relative to the
// database clock need this; tests that only need a known ordering can use a
// literal through setChangesWatermark.
func setChangesWatermarkAt(t *testing.T, st *store.Store, when time.Time, ids ...int64) {
	t.Helper()
	var value any = when.UTC()
	if !st.IsPostgreSQL() {
		value = when.UTC().Format(store.SQLiteTimestampLayout)
	}
	for _, id := range ids {
		_, err := st.DB().Exec(
			st.Rebind(`UPDATE messages SET content_changed_at = ? WHERE id = ?`), value, id)
		require.NoErrorf(t, err, "set content_changed_at for message %d", id)
	}
}

// seedChangedMessageAtID seeds one message on an exact id, which is how a test
// reaches an id no auto-increment hands out. 0 and negatives are legal ids: the
// column is SQLite's INTEGER PRIMARY KEY — the rowid — and BIGINT on
// PostgreSQL, and the schema constrains neither further.
//
// The two backends need opposite orders, the same way seedMessageAtID in
// internal/store's tests does: SQLite takes any 64-bit rowid, so the row is
// inserted first and moved afterwards; PostgreSQL's id is GENERATED ALWAYS AS
// IDENTITY and refuses the UPDATE outright, so the identity sequence is
// repositioned before the insert instead.
func seedChangedMessageAtID(t *testing.T, st *store.Store, id int64) int64 {
	t.Helper()
	if st.IsPostgreSQL() {
		// The default lower bound of a bigint identity is 1, so an id below that
		// needs MINVALUE lowered before RESTART will accept it.
		alter := fmt.Sprintf(`ALTER TABLE messages ALTER COLUMN id RESTART WITH %d`, id)
		if id < 1 {
			alter = fmt.Sprintf(
				`ALTER TABLE messages ALTER COLUMN id SET MINVALUE %d RESTART WITH %d`, id, id)
		}
		_, err := st.DB().Exec(alter)
		require.NoErrorf(t, err, "reposition the messages identity sequence to %d", id)
		got := seedChangedMessages(t, st, 1)[0]
		require.Equalf(t, id, got, "the seeded message did not land on id %d", id)
		return id
	}
	got := seedChangedMessages(t, st, 1)[0]
	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages SET id = ? WHERE id = ?`), id, got)
	require.NoErrorf(t, err, "move message %d to id %d", got, id)
	return id
}

// setChangesMessageTimestamp writes a lifecycle timestamp column directly.
// These are content columns, so the write also bumps the watermark.
func setChangesMessageTimestamp(t *testing.T, st *store.Store, id int64, col string, value time.Time) {
	t.Helper()
	_, err := st.DB().Exec(
		st.Rebind(fmt.Sprintf(`UPDATE messages SET %s = ? WHERE id = ?`, col)), value, id)
	require.NoErrorf(t, err, "set messages.%s for message %d", col, id)
}

// subSecondWatermark is a watermark literal carrying the finest sub-second
// resolution the target backend actually produces: microseconds on PostgreSQL,
// milliseconds on SQLite (strftime('%f') stops there, and the SQLite cursor
// parameter is encoded at that same resolution). Either way the fraction is
// non-zero, which is what the RFC3339Nano serialisation has to preserve.
func subSecondWatermark(st *store.Store) string {
	if st.IsPostgreSQL() {
		return "2026-07-26 10:00:00.731123"
	}
	return "2026-07-26 10:00:00.731"
}

// changesTarget builds a /messages/changes URL. An empty cursor or a zero limit
// omits that parameter, which is how a first-run consumer calls the feed.
//
// The cursor is opaque, so no test in this file builds one out of its parts. A
// test that needs to start from a chosen position asks encodeChangesCursor for
// it — the same codec the server publishes with — and every other test reuses a
// next_cursor a real response handed back.
func changesTarget(cursor string, limit int) string {
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit != 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	target := "/api/v1/messages/changes"
	if encoded := q.Encode(); encoded != "" {
		target += "?" + encoded
	}
	return target
}

// changesArchiveUID reads the durable archive identity the server binds every
// cursor it issues to. A test that builds a cursor has to bind it the same way
// or the server rejects it as belonging to another archive — which is the
// point.
func changesArchiveUID(t *testing.T, srv *Server) string {
	t.Helper()
	identifier, ok := srv.store.(ArchiveIdentifier)
	require.True(t, ok, "the change feed's store must be able to identify its archive")
	uid, err := identifier.ArchiveUIDContext(context.Background())
	require.NoError(t, err, "ArchiveUIDContext")
	return uid
}

// changesCursor builds a cursor for srv's archive at a chosen position — just
// after the row (at, id) — through the same codec the server publishes with.
func changesCursor(t *testing.T, srv *Server, at time.Time, id int64) string {
	t.Helper()
	return encodeChangesCursor(changesArchiveUID(t, srv), store.ChangedMessagesAfter(at, id))
}

// changesInstantCursor builds the other position the server publishes: the
// START of an instant, which carries no id tiebreak and so stands below every
// row stamped there. It is what the future-cursor clamp hands back, and what an
// absent cursor means with at zero.
func changesInstantCursor(t *testing.T, srv *Server, at time.Time) string {
	t.Helper()
	return encodeChangesCursor(changesArchiveUID(t, srv), store.ChangedMessagesFrom(at))
}

// changesFarFuture is a cursor no watermark can reach, so a page requested with
// it comes back empty and carries nothing but the clock reading.
func changesFarFuture(t *testing.T, srv *Server) string {
	t.Helper()
	return changesCursor(t, srv, time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC), 0)
}

// changesServerTime reads the database clock the way a client does: every page
// carries it, empty ones included.
func changesServerTime(t *testing.T, srv *Server) time.Time {
	t.Helper()
	resp := getChangesPage(t, srv, changesTarget(changesFarFuture(t, srv), 1))
	at, err := time.Parse(time.RFC3339Nano, resp.ServerTime)
	require.NoErrorf(t, err, "server_time %q must parse as RFC3339", resp.ServerTime)
	return at
}

// changesCompleteThrough reads how far the feed is complete — its page bound,
// which is what decides whether a row is publishable yet.
func changesCompleteThroughString(t *testing.T, resp ChangesResponse) string {
	t.Helper()
	require.NotNil(t, resp.CompleteThrough, "complete_through must be present once a bound exists")
	return *resp.CompleteThrough
}

func changesCompleteThrough(t *testing.T, srv *Server) time.Time {
	t.Helper()
	resp := getChangesPage(t, srv, changesTarget(changesFarFuture(t, srv), 1))
	value := changesCompleteThroughString(t, resp)
	at, err := time.Parse(time.RFC3339Nano, value)
	require.NoErrorf(t, err, "complete_through %q must parse as RFC3339", value)
	return at
}

// settleChangesClock blocks until every watermark written so far is
// publishable. The feed withholds the instant it is bounded at — that instant
// can still receive commits — so a test that seeds rows through the triggers
// and then expects to see them has to let the bound leave it. Tests that stamp
// their own watermarks in the past do not need it.
func settleChangesClock(t *testing.T, srv *Server) {
	t.Helper()
	waitChangesBoundPast(t, srv, changesServerTime(t, srv))
}

// waitChangesBoundPast blocks until the feed's commit bound has moved strictly
// past at, which is what makes a watermark stamped at or below at publishable.
func waitChangesBoundPast(t *testing.T, srv *Server, at time.Time) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !changesCompleteThrough(t, srv).After(at) {
		if time.Now().After(deadline) {
			require.Failf(t, "the change feed stopped advancing",
				"complete_through never moved past %s; something is holding a write "+
					"transaction open", at)
			return
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// getChangesPage serves one feed request and decodes a successful response.
func getChangesPage(t *testing.T, srv *Server, target string) ChangesResponse {
	t.Helper()
	w := doGet(srv, target)
	require.Equalf(t, http.StatusOK, w.Code, "GET %s: %s", target, w.Body.String())
	var resp ChangesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "decode changes response")
	return resp
}

// changedIDs extracts the ids of a page in the order the feed returned them.
func changedIDs(resp ChangesResponse) []int64 {
	ids := make([]int64, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		ids = append(ids, m.ID)
	}
	return ids
}

// TestChangesEndpoint_WalksEveryMessageExactlyOnce follows next_cursor the way
// a client would, across a block sharing one watermark: five pages, no
// duplicate and no skip. The final page is also the exact-boundary case: 25 rows
// walked five at a time ends on a full page with nothing after it, and has_more
// must say so.
func TestChangesEndpoint_WalksEveryMessageExactlyOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	const total = 25
	const pageSize = 5
	want := seedChangedMessages(t, st, total)
	setChangesWatermark(t, st, subSecondWatermark(st), want...)

	var (
		got    []int64
		cursor string
		pages  int
	)
	for {
		require.Lessf(pages, 200,
			"the feed did not terminate after %d pages: the cursor is not advancing", pages)
		resp := getChangesPage(t, srv, changesTarget(cursor, pageSize))
		require.LessOrEqual(len(resp.Messages), pageSize, "a page must not exceed the limit")
		require.Equal(len(resp.Messages), resp.Count, "count must describe the page it ships with")
		require.NotEmpty(resp.NextCursor, "every page must hand back something to send next")
		if len(resp.Messages) == 0 {
			assert.False(resp.HasMore, "an empty page has nothing after it")
			break
		}
		pages++
		got = append(got, changedIDs(resp)...)
		assert.Equalf(len(got) < total, resp.HasMore,
			"has_more after %d of %d rows", len(got), total)
		cursor = resp.NextCursor
	}

	require.Len(got, total,
		"a walk over %d rows sharing one watermark returned %d rows: the HTTP cursor "+
			"must deliver each row exactly once", total, len(got))
	assert.Equal(want, got,
		"the walk must return every same-instant row exactly once, in id order")
	assert.Equal(total/pageSize, pages, "walk page count")
}

// TestChangesEndpoint_CursorRoundTripsFullPrecision is the loop guard: take
// next_cursor from one response, send it back, and assert the second page does
// not repeat the first. A cursor that lost the sub-second part of the watermark
// re-selects the page that was just delivered — forever.
//
// The token is opaque, so the precision itself is held by
// TestChangesCursorRoundTripsSubSecondPrecision; what is checked here is the
// position the server chose to publish, and that resending it moves the walk on.
func TestChangesEndpoint_CursorRoundTripsFullPrecision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	ids := seedChangedMessages(t, st, 3)
	watermark := subSecondWatermark(st)
	setChangesWatermark(t, st, watermark, ids...)

	first := getChangesPage(t, srv, changesTarget("", 10))
	require.Equal(ids, changedIDs(first), "the first page returns the whole archive")

	last := first.Messages[len(first.Messages)-1]
	watermarkAt, err := time.Parse(time.RFC3339Nano, last.ContentChangedAt)
	require.NoErrorf(err, "content_changed_at %q must parse as RFC3339", last.ContentChangedAt)
	require.NotZerof(watermarkAt.Nanosecond(),
		"this test is meaningless unless the watermark %q carries a sub-second part", watermark)
	assert.Equal(changesCursor(t, srv, watermarkAt, last.ID), first.NextCursor,
		"the published cursor must be the last row's position, sub-second part included")

	second := getChangesPage(t, srv, changesTarget(first.NextCursor, 10))
	assert.Empty(second.Messages,
		"resending next_cursor must not redeliver the page it came from; a "+
			"second-truncated cursor makes a polling consumer loop forever")
}

// TestChangesEndpoint_ResumeFromEarlierCursorRedelivers documents the
// consequence of the overlap advice: resuming from an earlier cursor
// re-delivers rows rather than erroring or skipping. That makes an overlapping
// re-read safe. It says nothing about whether every change reaches the feed —
// a change committing after a later transaction's watermark was already
// returned is missed, and no cursor arithmetic here can recover it.
func TestChangesEndpoint_ResumeFromEarlierCursorRedelivers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	ids := seedChangedMessages(t, st, 6)
	setChangesWatermark(t, st, subSecondWatermark(st), ids...)

	first := getChangesPage(t, srv, changesTarget("", 3))
	require.Len(first.Messages, 3, "first page")
	require.True(first.HasMore, "more rows remain")

	// Resume from the position the consumer held after the FIRST row of the
	// first page: the two rows it already saw come back again.
	resumed := first.Messages[0]
	resumedAt, err := time.Parse(time.RFC3339Nano, resumed.ContentChangedAt)
	require.NoErrorf(err, "content_changed_at %q must parse as RFC3339", resumed.ContentChangedAt)
	rewound := getChangesPage(t, srv, changesTarget(changesCursor(t, srv, resumedAt, resumed.ID), 10))
	assert.Equal(ids[1:], changedIDs(rewound),
		"a cursor resumed from an earlier position redelivers the rows after it, "+
			"so a consumer may safely re-read an overlapping window")
}

// TestChangesEndpoint_EmptyArchiveReturnsEchoableCursor covers the first poll
// of an archive with nothing in it. The response must be directly re-sendable
// as the next request, and server_time must be a real database clock reading:
// the store returns a zero ServerTime for a non-positive limit, so a handler
// that forwarded its raw limit would publish "0001-01-01T00:00:00Z" here.
func TestChangesEndpoint_EmptyArchiveReturnsEchoableCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _ := newChangesServer(t)

	w := doGet(srv, changesTarget("", 0))
	require.Equalf(http.StatusOK, w.Code, "body: %s", w.Body.String())

	var raw map[string]json.RawMessage
	require.NoError(json.Unmarshal(w.Body.Bytes(), &raw), "decode response object")
	assert.JSONEq("[]", string(raw["messages"]),
		"messages must be an empty array, never null: a client that ranges over "+
			"null gets a nil dereference in most languages")

	var resp ChangesResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &resp), "decode changes response")
	assert.Equal(0, resp.Count, "count")
	assert.False(resp.HasMore, "has_more")
	assert.Equal(changesInstantCursor(t, srv, time.Time{}), resp.NextCursor,
		"a caller that sent no cursor is still handed one: the start of the archive, "+
			"which is where it still stands")

	serverTime, err := time.Parse(time.RFC3339Nano, resp.ServerTime)
	require.NoErrorf(err, "server_time %q must parse as RFC3339", resp.ServerTime)
	assert.WithinDuration(time.Now().UTC(), serverTime, time.Hour,
		"server_time must be the database's clock reading, not a zero time")
}

// TestChangesEndpoint_EmptyPageEchoesRequestCursor covers a caught-up consumer.
// There is no last row to derive a cursor from, so the response echoes the
// request's cursor; zero values would replay the whole archive on the next poll
// of an idle feed.
func TestChangesEndpoint_EmptyPageEchoesRequestCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	ids := seedChangedMessages(t, st, 3)
	setChangesWatermark(t, st, subSecondWatermark(st), ids...)

	first := getChangesPage(t, srv, changesTarget("", 10))
	require.Len(first.Messages, 3, "first page")

	caughtUp := getChangesPage(t, srv, changesTarget(first.NextCursor, 10))
	require.Empty(caughtUp.Messages, "the consumer is caught up")
	assert.Equal(first.NextCursor, caughtUp.NextCursor,
		"an empty page echoes the cursor it was sent so the consumer holds its place")
	assert.False(caughtUp.HasMore, "has_more")

	// The echoed cursor must itself be re-sendable, which is the whole point.
	stillCaughtUp := getChangesPage(t, srv, changesTarget(caughtUp.NextCursor, 10))
	assert.Empty(stillCaughtUp.Messages, "polling an idle feed must stay empty")
}

// TestChangesEndpoint_CursorAboveTheDatabaseClockRecovers is the other half of
// the echo: a cursor the feed can never satisfy must not be echoed back.
//
// The page query stops strictly below the database clock, so a cursor above
// that clock matches nothing — and the response is 200 / count=0 /
// has_more=false, byte-identical to "you are caught up". Echoing that cursor
// hands the poison straight back, so the consumer polls a stalled feed forever
// while the archive changes. A backwards clock step (NTP correction, a resumed
// VM, a restore onto slower hardware) or a client that builds its own cursor
// gets there without doing anything wrong.
//
// What this proves is that the consumer starts moving again, not that it lost
// nothing on the way: a backward step also strands changes below the recovered
// cursor, which no cursor policy reaches. That is
// TestChangesEndpoint_BackwardClockStepLosesChangesBelowTheCursor.
func TestChangesEndpoint_CursorAboveTheDatabaseClockRecovers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	seedChangedMessages(t, st, 2)
	settleChangesClock(t, srv)

	future := changesCursor(t, srv, changesServerTime(t, srv).Add(time.Hour), 0)
	poisoned := getChangesPage(t, srv, changesTarget(future, 100))
	require.Zero(poisoned.Count, "a cursor an hour ahead of the clock matches nothing")

	boundValue := changesCompleteThroughString(t, poisoned)
	bound, err := time.Parse(time.RFC3339Nano, boundValue)
	require.NoErrorf(err, "complete_through %q must parse as RFC3339", boundValue)
	assert.Equal(changesInstantCursor(t, srv, bound), poisoned.NextCursor,
		"the cursor must come back moved down to the START of this page's own commit "+
			"bound, which is never above server_time; echoing it would make the very "+
			"next poll unsatisfiable too, and landing after any id in that instant "+
			"would skip the rows stamped there")

	// Changes arrive after the poisoned poll and must be delivered.
	seedMoreChangedMessages(t, st, "late", 3)
	settleChangesClock(t, srv)

	resumed := getChangesPage(t, srv, changesTarget(poisoned.NextCursor, 100))
	assert.NotZero(resumed.Count,
		"the feed stalled: changes made after the cursor was clamped were never delivered")
}

// TestChangesEndpoint_ClampedCursorReachesEveryIDAtTheBound covers the row the
// clamp above has to be able to come back for.
//
// The clamped cursor stands ON the commit bound, and the page query stops
// strictly below it, so a row stamped exactly there is delivered by a LATER
// poll or by none at all. Which of the two it is comes down to the id half of
// the clamped position: the store's keyset predicate is
// `>= cursor AND (> cursor OR id > tiebreak)`, so any tiebreak VALUE skips the
// rows at the bound whose ids do not sort above it. Message ids are not all
// positive — `id` is SQLite's rowid and BIGINT on PostgreSQL, with no further
// constraint in the schema — and SQLite stamps at millisecond resolution, so a
// write landing in the same millisecond as the bound on a row at id 0 or below
// is reachable rather than theoretical. Dropping it would be silent and
// permanent: the row is not waiting for anything, and only a later edit that
// gives it a newer watermark would ever bring it back.
//
// The existing clamp tests cannot catch this. They seed through UpsertMessage,
// which hands out auto-generated positive ids, and every positive id sorts
// above any tiebreak the clamp has ever published.
func TestChangesEndpoint_ClampedCursorReachesEveryIDAtTheBound(t *testing.T) {
	for _, id := range []int64{0, -7} {
		t.Run(fmt.Sprintf("a row at id %d", id), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			srv, st := newChangesServer(t)

			seedChangedMessageAtID(t, st, id)

			poisoned := getChangesPage(t, srv, changesTarget(changesFarFuture(t, srv), 100))
			require.Zero(poisoned.Count, "a cursor in the year 2999 matches nothing")
			boundValue := changesCompleteThroughString(t, poisoned)
			bound, err := time.Parse(time.RFC3339Nano, boundValue)
			require.NoErrorf(err, "complete_through %q must parse as RFC3339",
				boundValue)

			// Stamped exactly at the instant the clamp landed on — the one
			// instant a clamped cursor has to remain able to reach.
			setChangesWatermarkAt(t, st, bound, id)
			waitChangesBoundPast(t, srv, bound)

			resumed := getChangesPage(t, srv, changesTarget(poisoned.NextCursor, 100))
			assert.Containsf(changedIDs(resumed), id,
				"message %d is stamped exactly at the bound the clamp moved the cursor "+
					"to (%s) and the bound has since moved past it, so the clamped cursor "+
					"must still deliver it; a clamped position that sorts above it drops "+
					"the row for good", id, boundValue)
		})
	}
}

// TestChangesEndpoint_BackwardClockStepLosesChangesBelowTheCursor holds the
// documentation to what the feed actually does when the database clock steps
// backwards, because the two readings differ by "delay" versus "loss".
//
// The feed orders by a wall-clock watermark and a consumer's cursor only moves
// forward, so a step back means the writes that follow it are stamped in clock
// time the walk has already passed. Everything stamped below the cursor the
// consumer is holding fails the keyset lower bound on that poll and on every
// poll afterwards. docs/api-server.md's delivery contract names this as a loss
// bounded by the size of the step, and points at a full re-read from an empty
// cursor as the repair; both halves are asserted here.
//
// The two subtests are the same failure on either side of the future-cursor
// clamp, which is why the clamp cannot be blamed for it or fixed to prevent it.
func TestChangesEndpoint_BackwardClockStepLosesChangesBelowTheCursor(t *testing.T) {
	t.Run("the clamp does not recover what the step stranded", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		srv, st := newChangesServer(t)

		ids := seedChangedMessages(t, st, 2)
		delivered, afterStep := ids[0], ids[1]
		now := changesServerTime(t, srv)
		setChangesWatermarkAt(t, st, now.Add(-2*time.Hour), delivered)
		// The clock stepped back minutes ago; this change committed after the
		// step, so the database stamped it from the stepped-back clock — below
		// the cursor the consumer has held since before the step.
		setChangesWatermarkAt(t, st, now.Add(-5*time.Minute), afterStep)

		preStep := changesCursor(t, srv, now.Add(time.Hour), 4242)
		poisoned := getChangesPage(t, srv, changesTarget(preStep, 100))
		require.Zero(poisoned.Count, "a cursor above the stepped-back clock matches nothing")
		require.NotEqual(preStep, poisoned.NextCursor, "the future cursor must be clamped")
		boundValue := changesCompleteThroughString(t, poisoned)

		recovered := getChangesPage(t, srv, changesTarget(poisoned.NextCursor, 100))
		assert.NotContainsf(changedIDs(recovered), afterStep,
			"message %d was stamped below the clamp target (this page's commit bound, "+
				"%s), so the clamp cannot return it", afterStep, boundValue)

		// And it never comes back: the clamped cursor only rises from here.
		again := getChangesPage(t, srv, changesTarget(recovered.NextCursor, 100))
		assert.NotContainsf(changedIDs(again), afterStep,
			"message %d is below the cursor for good; a later poll cannot reach "+
				"back under it", afterStep)

		// The documented repair. Unlike an unparseable or NULL watermark, the
		// row is perfectly selectable — it is only below the cursor — so a
		// reconciling full re-read does return it.
		reread := getChangesPage(t, srv, changesTarget("", 100))
		assert.Containsf(changedIDs(reread), afterStep,
			"a full re-read from an empty cursor is the only thing that recovers "+
				"message %d, and the delivery contract says so", afterStep)
	})

	t.Run("and the clamp is not what loses it", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		srv, st := newChangesServer(t)

		ids := seedChangedMessages(t, st, 3)
		delivered, afterStep, later := ids[0], ids[1], ids[2]
		now := changesServerTime(t, srv)
		setChangesWatermarkAt(t, st, now.Add(-30*time.Minute), delivered)
		// Same shape as above, but this consumer polls late enough that the
		// clock has already climbed back above its cursor. The future-cursor
		// branch never runs, and the row the step stranded is lost regardless.
		setChangesWatermarkAt(t, st, now.Add(-time.Hour), afterStep)
		setChangesWatermarkAt(t, st, now.Add(-10*time.Minute), later)

		sent := now.Add(-30 * time.Minute)
		page := getChangesPage(t, srv, changesTarget(changesCursor(t, srv, sent, delivered), 100))

		serverTime, err := time.Parse(changesTimeLayout, page.ServerTime)
		require.NoError(err, "server_time must parse")
		require.Truef(serverTime.After(sent),
			"this cursor (%s) is below the clock (%s), so the future-cursor clamp "+
				"cannot have fired", sent, page.ServerTime)

		assert.Containsf(changedIDs(page), later,
			"message %d is above the cursor and must still be delivered: the feed "+
				"looks entirely healthy while it drops the stranded row", later)
		assert.NotContainsf(changedIDs(page), afterStep,
			"message %d was stamped below the cursor by the stepped-back clock and "+
				"is skipped with no clamp involved", afterStep)

		reread := getChangesPage(t, srv, changesTarget("", 100))
		assert.Containsf(changedIDs(reread), afterStep,
			"the repair is the same on this side of the clamp: only a full re-read "+
				"from an empty cursor returns message %d", afterStep)
	})
}

// TestChangesEndpoint_ClampsLimit pins the page-size rules a client can rely on
// and the boundary the store handoff makes dangerous: a non-positive limit
// reaching the store returns a zero server_time and no database round trip, so
// the default has to be applied first.
func TestChangesEndpoint_ClampsLimit(t *testing.T) {
	srv, st := newChangesServer(t)
	seedChangedMessages(t, st, maxPageSize+1)
	settleChangesClock(t, srv)

	tests := []struct {
		name   string
		target string
		want   int
	}{
		{"absent falls back to the default", "/api/v1/messages/changes", defaultChangesPageSize},
		{"above the maximum clamps", "/api/v1/messages/changes?limit=100000", maxPageSize},
		{"zero falls back to the default", "/api/v1/messages/changes?limit=0", defaultChangesPageSize},
		{"negative falls back to the default", "/api/v1/messages/changes?limit=-5", defaultChangesPageSize},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			resp := getChangesPage(t, srv, tc.target)
			assert.Len(resp.Messages, tc.want, "page size")
			assert.Equal(tc.want, resp.Count, "count")
			assert.True(resp.HasMore, "rows remain beyond a clamped page")
			serverTime, err := time.Parse(time.RFC3339Nano, resp.ServerTime)
			require.NoErrorf(err, "server_time %q must parse as RFC3339", resp.ServerTime)
			assert.False(serverTime.IsZero(),
				"server_time must come from the database clock: the store skips the "+
					"round trip entirely for a non-positive limit")
		})
	}
}

// changesLimitParam returns the published schema for the feed's limit query
// parameter, straight out of the document the OpenAPI artifacts and the
// generated clients are built from.
func changesLimitParam(t *testing.T) *huma.Param {
	t.Helper()
	path := OpenAPIDocument().Paths["/api/v1/messages/changes"]
	require.NotNil(t, path, "the changes endpoint must be in the OpenAPI document")
	require.NotNil(t, path.Get, "the changes endpoint must document its GET")
	for _, p := range path.Get.Parameters {
		if p.Name == "limit" {
			return p
		}
	}
	require.FailNow(t, "the changes endpoint must document a limit parameter")
	return nil
}

// TestChangesEndpoint_PublishedLimitRangeMatchesWhatTheServerAccepts stops the
// schema and the handler contradicting each other.
//
// The handler clamps limit rather than rejecting it: zero and negative values
// fall back to the default, oversized ones to the cap, and all of them answer
// 200. A published minimum/maximum turns those same requests into client-side
// validation failures — the generated Go client refuses to send a request the
// server would have answered — and contradicts the parameter's own description.
func TestChangesEndpoint_PublishedLimitRangeMatchesWhatTheServerAccepts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)
	seedChangedMessages(t, st, 3)
	setChangesWatermark(t, st, subSecondWatermark(st), 1, 2, 3)

	limit := changesLimitParam(t)
	require.NotNil(limit.Schema, "the limit parameter must carry a schema")

	for _, raw := range []int64{0, -1, maxPageSize + 1, 1_000_000} {
		w := doGet(srv, fmt.Sprintf("/api/v1/messages/changes?limit=%d", raw))
		require.Equalf(http.StatusOK, w.Code,
			"the server clamps limit=%d rather than rejecting it: %s", raw, w.Body.String())

		if limit.Schema.Minimum != nil {
			assert.LessOrEqualf(*limit.Schema.Minimum, float64(raw),
				"the schema publishes minimum %v, so a generated client refuses to send "+
					"limit=%d — which the server answers with 200", *limit.Schema.Minimum, raw)
		}
		if limit.Schema.Maximum != nil {
			assert.GreaterOrEqualf(*limit.Schema.Maximum, float64(raw),
				"the schema publishes maximum %v, so a generated client refuses to send "+
					"limit=%d — which the server answers with 200", *limit.Schema.Maximum, raw)
		}
	}
}

// TestChangesEndpoint_ExactPageBoundaryReportsNoMorePages pins how has_more is
// computed. A page that exactly fills the limit is indistinguishable from a
// partial one unless the handler looks one row further, and a spurious
// has_more sends every caught-up consumer round again.
func TestChangesEndpoint_ExactPageBoundaryReportsNoMorePages(t *testing.T) {
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	ids := seedChangedMessages(t, st, 3)
	setChangesWatermark(t, st, subSecondWatermark(st), ids...)

	resp := getChangesPage(t, srv, changesTarget("", len(ids)))

	assert.Equal(ids, changedIDs(resp), "the page holds every row")
	assert.False(resp.HasMore,
		"a page that exactly fills the limit with nothing after it must report "+
			"has_more false")
}

// TestChangesEndpoint_RejectsMalformedCursor: a cursor this API did not issue is
// rejected rather than silently read as the start of the archive, which would
// turn a client bug into a full re-delivery. The rejection happens before the
// store is consulted, so a client typo is reported as the typo it is whatever
// backend is configured.
func TestChangesEndpoint_RejectsMalformedCursor(t *testing.T) {
	srv, st := newChangesServer(t)
	ids := seedChangedMessages(t, st, 3)
	setChangesWatermark(t, st, subSecondWatermark(st), ids...)

	tests := []struct {
		name     string
		target   string
		wantCode string
	}{
		{"cursor is not a token", "/api/v1/messages/changes?cursor=yesterday!!", "invalid_cursor"},
		{"cursor is a truncated token", "/api/v1/messages/changes?cursor=" +
			changesCursor(t, srv, time.Now(), 5)[:6], "invalid_cursor"},
		{"limit not numeric", "/api/v1/messages/changes?limit=many", "invalid_limit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := doGet(srv, tc.target)
			require.Equalf(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
			env := decodeErrorEnvelope(t, w)
			assert.Equal(t, tc.wantCode, env.Error, "error code")
			assert.NotContains(t, w.Body.String(), `"messages"`,
				"an unusable cursor must not fall back to the beginning of the archive: "+
					"the caller would silently re-receive everything it already holds")
		})
	}
}

// TestChangesEndpoint_EmptyCursorStartsFromTheBeginning pins what an EMPTY
// parameter value means, as opposed to an unusable one. `?cursor=` is read as
// absent — the same as omitting it — across this whole API, so a client whose
// serialiser writes empty query parameters gets the first-run behaviour rather
// than a 400. It is the surprising half of the cursor contract, so the docs
// state it and this holds them to it.
func TestChangesEndpoint_EmptyCursorStartsFromTheBeginning(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	ids := seedChangedMessages(t, st, 3)
	setChangesWatermark(t, st, subSecondWatermark(st), ids...)

	empty := getChangesPage(t, srv, "/api/v1/messages/changes?cursor=&limit=10")
	absent := getChangesPage(t, srv, "/api/v1/messages/changes?limit=10")

	require.Equal(ids, changedIDs(empty),
		"an empty cursor is absent, not a parse failure, so the feed starts from "+
			"the beginning of the archive")
	assert.Equal(changedIDs(absent), changedIDs(empty), "an empty cursor and no cursor must agree")
	assert.Equal(absent.NextCursor, empty.NextCursor, "and must leave the caller in the same place")
	assert.Equal(len(ids), empty.Count, "count")
	assert.False(empty.HasMore, "the whole archive fits in one page here")
}

// TestChangesEndpoint_AcceptsAFabricatedCursorForItsOwnArchive pins a DECISION
// at the route, where a consumer meets it.
//
// The cursor is not signed and the server has no secret to sign it with, so it
// cannot distinguish a cursor it issued from a well-formed one a caller built,
// and it does not try. A forged cursor buys its holder nothing: it moves that
// caller's own position in that caller's own feed and reaches no message the
// caller could not already request through /messages/filter. The published
// contract says "opaque" — do not construct one — but that is advice about
// coupling, not an enforced rule, and the docs say so rather than promising an
// enforcement this server cannot perform.
func TestChangesEndpoint_AcceptsAFabricatedCursorForItsOwnArchive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)
	ids := seedChangedMessages(t, st, 4)
	setChangesWatermark(t, st, "2026-07-26 10:00:00.731", ids...)

	// Hand-built from the published shape, naming a position no page ever
	// issued: after the second row's watermark, so the walk resumes at the third.
	fabricated := "1." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil,
		`{"t":"2026-07-26T10:00:00.731Z","i":%d,"a":"%s"}`,
		ids[1], changesArchiveUID(t, srv)))
	require.NotEqual(changesInstantCursor(t, srv, time.Time{}), fabricated,
		"this test is meaningless unless the token is one no page handed out")

	resp := getChangesPage(t, srv, changesTarget(fabricated, 10))

	assert.Equal(ids[2:], changedIDs(resp),
		"a well-formed cursor naming this archive is honoured whoever built it")
}

// TestChangesEndpoint_RejectsACursorFromAnotherArchive is the silent-loss guard
// the whole feed exists for, applied to the cursor itself.
//
// A cursor is a position in ONE archive: a watermark and an archive-local
// message id. Point a consumer at a restored copy, a rebuilt archive, or simply
// a different one — same daemon, different --db — and its stored cursor is
// meaningful nowhere but where it came from. Accepted, it starts the walk at
// some unrelated position and every record before that position is never
// delivered: exactly the silent omission the feed is meant to make impossible.
// So the cursor carries the archive's durable UID and a foreign one is a 400.
//
// The same test pins the other half, because a check that rejects everything
// would also pass the first half: a cursor sent back to the archive that issued
// it resumes the walk.
func TestChangesEndpoint_RejectsACursorFromAnotherArchive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srvA, stA := newChangesServer(t)
	idsA := seedChangedMessages(t, stA, 4)
	setChangesWatermark(t, stA, subSecondWatermark(stA), idsA...)
	first := getChangesPage(t, srvA, changesTarget("", 2))
	require.Equal(idsA[:2], changedIDs(first), "precondition: archive A hands out page one")
	require.NotEmpty(first.NextCursor, "precondition: archive A publishes a cursor")

	// Its own archive: the cursor resumes the walk.
	resumed := getChangesPage(t, srvA, changesTarget(first.NextCursor, 2))
	assert.Equal(idsA[2:], changedIDs(resumed),
		"a cursor sent back to the archive that issued it must resume the walk")

	// A different archive: the cursor means nothing there.
	srvB, stB := newChangesServer(t)
	idsB := seedChangedMessages(t, stB, 4)
	setChangesWatermark(t, stB, subSecondWatermark(stB), idsB...)

	w := doGet(srvB, changesTarget(first.NextCursor, 10))
	require.Equalf(http.StatusBadRequest, w.Code,
		"a cursor from another archive must be rejected, not silently honoured: %s",
		w.Body.String())
	env := decodeErrorEnvelope(t, w)
	assert.Equal("invalid_cursor", env.Error, "error code")
	assert.Contains(env.Message, "archive",
		"the message must say the cursor belongs to a different archive")
	assert.Contains(env.Message, "from the beginning",
		"and it must name the repair: the sync restarts from the beginning")
	assert.NotContains(w.Body.String(), `"messages"`,
		"a foreign cursor must never be read as the start of the archive: the "+
			"consumer would silently re-receive everything, or worse, resume at a "+
			"position that skips rows it has never seen")
}

// TestChangesEndpoint_AlwaysPublishesACursor: a client always has something to
// send back, whatever shape the page took. A page that withheld the cursor would
// leave the caller with nothing to hold its place but the start of the archive.
func TestChangesEndpoint_AlwaysPublishesACursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)
	ids := seedChangedMessages(t, st, 3)
	setChangesWatermark(t, st, subSecondWatermark(st), ids...)

	first := getChangesPage(t, srv, changesTarget("", 10))
	require.Len(first.Messages, len(ids), "the archive fits in one page")

	full := getChangesPage(t, srv, changesTarget("", 2))
	require.True(full.HasMore, "a full page with rows behind it")

	caughtUp := getChangesPage(t, srv, changesTarget(first.NextCursor, 10))
	require.Empty(caughtUp.Messages, "an empty page")

	firstEver := getChangesPage(t, srv, changesTarget("", 0))

	empty, _ := newChangesServer(t)
	emptyArchive := getChangesPage(t, empty, changesTarget("", 10))

	for name, page := range map[string]ChangesResponse{
		"a partial page":              first,
		"a full page":                 full,
		"an empty page":               caughtUp,
		"a first request":             firstEver,
		"a first request, no archive": emptyArchive,
	} {
		assert.NotEmptyf(page.NextCursor, "%s must hand back a cursor", name)
	}
}

// TestChangesEndpoint_ReportsDeletedMessages: removals are changes. A consumer
// that never learns a message was hidden or deleted at the source keeps
// mirroring something the archive no longer shows.
func TestChangesEndpoint_ReportsDeletedMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	ids := seedChangedMessages(t, st, 3)
	live, hidden, removed := ids[0], ids[1], ids[2]
	hiddenAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	removedAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	setChangesMessageTimestamp(t, st, hidden, "deleted_at", hiddenAt)
	setChangesMessageTimestamp(t, st, removed, "deleted_from_source_at", removedAt)
	settleChangesClock(t, srv)

	resp := getChangesPage(t, srv, changesTarget("", 10))
	byID := make(map[int64]ChangedMessageJSON, len(resp.Messages))
	for _, m := range resp.Messages {
		byID[m.ID] = m
	}
	require.Contains(byID, live, "a live message must appear in the feed")
	require.Contains(byID, hidden, "a dedup-hidden message must appear in the feed")
	require.Contains(byID, removed, "a source-deleted message must appear in the feed")

	assert.Nil(byID[live].DeletedAt, "a live message carries no deleted_at")
	assert.Nil(byID[live].DeletedFromSourceAt, "a live message carries no deleted_from_source_at")
	require.NotNil(byID[hidden].DeletedAt, "deleted_at must be reported, not just implied")
	require.NotNil(byID[removed].DeletedFromSourceAt, "deleted_from_source_at must be reported")

	gotHiddenAt, err := time.Parse(time.RFC3339Nano, *byID[hidden].DeletedAt)
	require.NoErrorf(err, "deleted_at %q must parse as RFC3339", *byID[hidden].DeletedAt)
	assert.WithinDuration(hiddenAt, gotHiddenAt, time.Second, "deleted_at value")
	gotRemovedAt, err := time.Parse(time.RFC3339Nano, *byID[removed].DeletedFromSourceAt)
	require.NoErrorf(err, "deleted_from_source_at %q must parse as RFC3339",
		*byID[removed].DeletedFromSourceAt)
	assert.WithinDuration(removedAt, gotRemovedAt, time.Second, "deleted_from_source_at value")
}

// TestChangesEndpoint_UnavailableWhenStoreLacksSupport covers the optional
// interface. A store that cannot answer the watermark query must produce a
// defined refusal, not a panic and not an empty 200 that a consumer would read
// as "nothing changed".
func TestChangesEndpoint_UnavailableWhenStoreLacksSupport(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := newTestServerWithEngine(t, &querytest.MockEngine{})
	require.NotImplements((*ChangedMessageLister)(nil), srv.store,
		"this test only means something with a store that lacks the feed")

	w := doGet(srv, changesTarget("", 0))

	require.Equalf(http.StatusServiceUnavailable, w.Code, "body: %s", w.Body.String())
	env := decodeErrorEnvelope(t, w)
	assert.Equal("feature_unavailable", env.Error, "error code")
}

// stubArchiveUID is the identity every store double below reports. The feed
// binds each cursor it issues to it, exactly as it does to a real archive's.
const stubArchiveUID = "9a8b7c6d5e4f30211203f4e5d6c7b8a9"

// stubArchiveIdentity gives a store double the archive identity the change feed
// needs before it can read or issue a cursor. It is embedded rather than folded
// into mockStore so that a double can still be built WITHOUT it — which is what
// TestChangesEndpoint_UnavailableWhenTheStoreCannotIdentifyItsArchive needs.
type stubArchiveIdentity struct{}

func (stubArchiveIdentity) ArchiveUIDContext(context.Context) (string, error) {
	return stubArchiveUID, nil
}

// stubChangedMessageLister answers the feed with a fixed page, so a handler test
// can present a row no real store produces.
type stubChangedMessageLister struct {
	*mockStore
	stubArchiveIdentity

	page  store.ChangedMessagePage
	calls int
}

func (s *stubChangedMessageLister) ListChangedMessages(
	_ context.Context, _ store.ChangedMessagesCursor, _ int,
) (store.ChangedMessagePage, error) {
	s.calls++
	return s.page, nil
}

// failingChangedMessageLister refuses the watermark query the way a real store
// does — PostgreSQL refuses it outright when it has never seen every writer.
type failingChangedMessageLister struct {
	*mockStore
	stubArchiveIdentity

	err error
}

func (s *failingChangedMessageLister) ListChangedMessages(
	_ context.Context, _ store.ChangedMessagesCursor, _ int,
) (store.ChangedMessagePage, error) {
	return store.ChangedMessagePage{}, s.err
}

// TestChangesEndpoint_StoreFailureIsA500WithTheDetailOnlyInTheLog pins where a
// refused watermark query is reported. The store's own message names the remedy
// (a PostgreSQL grant) and the database objects behind it, so it belongs in the
// operator's log and not in a response any API client can read.
func TestChangesEndpoint_StoreFailureIsA500WithTheDetailOnlyInTheLog(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	logs := &bytes.Buffer{}
	const detail = "read watermark bounds: grant the msgvault role pg_read_all_stats"
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store: &failingChangedMessageLister{
			mockStore: &mockStore{},
			err:       errors.New(detail),
		},
		Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	w := doGet(srv, changesTarget("", 10))

	require.Equalf(http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	env := decodeErrorEnvelope(t, w)
	assert.Equal("internal_error", env.Error, "error code")
	assert.NotContains(w.Body.String(), "pg_read_all_stats",
		"the store's message names database internals and an operator's remedy; a "+
			"client that cannot act on either must not be handed them")
	assert.Contains(logs.String(), detail,
		"the operator has to be able to act on the failure, so the detail the "+
			"response withholds must survive in the log")
}

func TestChangesEndpoint_MalformedWatermarkIsA500WithoutACursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	id := seedChangedMessages(t, st, 1)[0]
	setChangesWatermark(t, st, "1999-13-45 99:99:99.999", id)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Logger: testLogger(),
	})

	w := doGet(srv, changesTarget("", 10))

	require.Equalf(http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	env := decodeErrorEnvelope(t, w)
	assert.Equal("internal_error", env.Error)
	assert.NotContains(w.Body.String(), `"next_cursor"`,
		"corrupt cursor state must stop the feed instead of publishing a synthetic position")
}

// TestChangesEndpoint_CanceledRequestIsNotReportedAsAServerFault covers the
// branch beside the 500: a client that hangs up mid-query is not a fault of this
// server, so it answers with the defined refusal and leaves the error log clean
// for failures an operator can do something about.
func TestChangesEndpoint_CanceledRequestIsNotReportedAsAServerFault(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	logs := &bytes.Buffer{}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store: &failingChangedMessageLister{
			mockStore: &mockStore{},
			err:       fmt.Errorf("list changed messages: %w", context.Canceled),
		},
		Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	w := doGet(srv, changesTarget("", 10))

	require.Equalf(http.StatusServiceUnavailable, w.Code, "body: %s", w.Body.String())
	env := decodeErrorEnvelope(t, w)
	assert.Equal("query_canceled", env.Error, "error code")
	assert.NotContains(logs.String(), "changed messages query failed",
		"an abandoned request must not raise an error an operator will go looking "+
			"for a cause of")
}

// unidentifiedChangedMessageLister can serve the feed but cannot say which
// archive it is — the capability split ArchiveIdentifier exists to express.
type unidentifiedChangedMessageLister struct {
	*mockStore

	page store.ChangedMessagePage
}

func (s *unidentifiedChangedMessageLister) ListChangedMessages(
	_ context.Context, _ store.ChangedMessagesCursor, _ int,
) (store.ChangedMessagePage, error) {
	return s.page, nil
}

// TestChangesEndpoint_UnavailableWhenTheStoreCannotIdentifyItsArchive: a cursor
// that names no archive is one any other archive will silently honour, so a
// store that cannot identify itself gets the same defined refusal as one that
// cannot serve the feed at all. Falling back to an unbound cursor would trade a
// visible 503 for silent, undetectable data loss on the next restore.
func TestChangesEndpoint_UnavailableWhenTheStoreCannotIdentifyItsArchive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  &unidentifiedChangedMessageLister{mockStore: &mockStore{}},
		Logger: testLogger(),
	})
	require.Implements((*ChangedMessageLister)(nil), srv.store,
		"this test only means something with a store that CAN serve the feed")
	require.NotImplements((*ArchiveIdentifier)(nil), srv.store,
		"and that cannot identify its archive")

	w := doGet(srv, changesTarget("", 10))

	require.Equalf(http.StatusServiceUnavailable, w.Code, "body: %s", w.Body.String())
	env := decodeErrorEnvelope(t, w)
	assert.Equal("feature_unavailable", env.Error, "error code")
	assert.NotContains(w.Body.String(), `"next_cursor"`,
		"an unbound cursor must never be published")
}

// erroringArchiveIdentity reports a store whose archive identity cannot be read
// — a real state: ErrArchiveIdentityCorrupt means the identity migration ran but
// its durable UID is gone.
type erroringArchiveIdentity struct {
	*mockStore

	err error
}

func (s *erroringArchiveIdentity) ArchiveUIDContext(context.Context) (string, error) {
	return "", s.err
}

func (s *erroringArchiveIdentity) ListChangedMessages(
	_ context.Context, _ store.ChangedMessagesCursor, _ int,
) (store.ChangedMessagePage, error) {
	return store.ChangedMessagePage{}, nil
}

// TestChangesEndpoint_UnreadableArchiveIdentityIsA500WithTheDetailOnlyInTheLog:
// the other loud failure. An identity that errors is a broken archive, not a
// missing feature, so it is a 500 with the cause in the operator's log — and
// again never a cursor bound to nothing.
func TestChangesEndpoint_UnreadableArchiveIdentityIsA500WithTheDetailOnlyInTheLog(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	logs := &bytes.Buffer{}
	const detail = "archive identity is corrupt: migration ledger is present but archive UID is missing"
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store: &erroringArchiveIdentity{
			mockStore: &mockStore{},
			err:       errors.New(detail),
		},
		Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	w := doGet(srv, changesTarget("", 10))

	require.Equalf(http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	env := decodeErrorEnvelope(t, w)
	assert.Equal("internal_error", env.Error, "error code")
	assert.NotContains(w.Body.String(), "migration ledger",
		"the response must not carry the archive's internals")
	assert.Contains(logs.String(), detail,
		"the operator has to be able to act on a corrupt archive identity")
	assert.NotContains(w.Body.String(), `"next_cursor"`,
		"an unbound cursor must never be published")
}

// blockingArchiveIdentity holds the archive-identity lookup open until the test
// releases it, which is what a saturated connection pool does to it: the lookup
// waits for a connection, and nothing about waiting for a connection is bounded
// by the request that is waiting on it.
type blockingArchiveIdentity struct {
	*mockStore

	entered chan struct{}
	release chan struct{}
}

func (s *blockingArchiveIdentity) ArchiveUIDContext(ctx context.Context) (string, error) {
	close(s.entered)
	select {
	case <-s.release:
		return stubArchiveUID, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *blockingArchiveIdentity) ListChangedMessages(
	_ context.Context, _ store.ChangedMessagesCursor, _ int,
) (store.ChangedMessagePage, error) {
	return store.ChangedMessagePage{}, nil
}

// TestChangesEndpoint_CanceledRequestDoesNotBlockInArchiveIdentity covers the
// step before the feed's context-aware query: resolving which archive the
// cursor belongs to.
//
// The lookup receives the request context directly, so a saturated pool cannot
// outlive both the client hanging up and the server's request timeout.
func TestChangesEndpoint_CanceledRequestDoesNotBlockInArchiveIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	identity := &blockingArchiveIdentity{
		mockStore: &mockStore{},
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	// Released whatever happens, so the blocked resolution never outlives the
	// test even if the handler returns without it.
	t.Cleanup(func() { close(identity.release) })
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  identity,
		Logger: testLogger(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, changesTarget("", 10), nil).WithContext(ctx)
	w := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		srv.Router().ServeHTTP(w, req)
	}()

	select {
	case <-identity.entered:
	case <-time.After(10 * time.Second):
		require.FailNow("the handler never reached archive-identity resolution")
	}
	cancel()

	select {
	case <-served:
	case <-time.After(10 * time.Second):
		require.FailNow(
			"the handler is still blocked in archive-identity resolution after the " +
				"request was cancelled; on a saturated pool that wait outlives the " +
				"server's request timeout too")
	}

	require.Equalf(http.StatusServiceUnavailable, w.Code, "body: %s", w.Body.String())
	env := decodeErrorEnvelope(t, w)
	assert.Equal("query_canceled", env.Error,
		"an abandoned request is not a server fault, so it gets the same defined "+
			"refusal as one cancelled inside the feed query")
}

// TestChangesEndpoint_NoBoundPublishesNull pins the wire value of the state
// complete_through has no instant for.
func TestChangesEndpoint_NoBoundPublishesNull(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store: &stubChangedMessageLister{
			mockStore: &mockStore{},
			page: store.ChangedMessagePage{
				ServerTime: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
				// CompleteThrough left zero: no bound established yet.
			},
		},
		Logger: testLogger(),
	})

	w := doGet(srv, changesTarget("", 10))
	require.Equalf(http.StatusOK, w.Code, "body: %s", w.Body.String())

	var raw map[string]json.RawMessage
	require.NoError(json.Unmarshal(w.Body.Bytes(), &raw), "decode response object")
	require.Contains(raw, "complete_through",
		"complete_through is required, so the no-bound state must still carry it")
	assert.JSONEq(`null`, string(raw["complete_through"]),
		"no bound is a state, not the year-one instant")
}

// seedSparseChangedMessages inserts the two shapes whose JSON is mostly holes:
// an email that was never deleted and carries no platform timestamps, and a
// chat message with no subject, no snippet, and no platform id. Returns their
// ids in that order.
func seedSparseChangedMessages(t *testing.T, st *store.Store) (email, chat int64) {
	t.Helper()
	src, err := st.GetOrCreateSource("gmail", "sparse@example.com")
	require.NoError(t, err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(
		src.ID, "sparse-conv", "email_thread", "Sparse thread")
	require.NoError(t, err, "EnsureConversationWithType")

	email, err = st.UpsertMessage(&store.Message{
		SourceID:        src.ID,
		SourceMessageID: "sparse-email-1",
		ConversationID:  convID,
		MessageType:     "email",
		Subject:         sql.NullString{String: "Q4 planning", Valid: true},
		Snippet:         sql.NullString{String: "Here's the draft", Valid: true},
		SentAt:          sql.NullTime{Time: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC), Valid: true},
		SizeEstimate:    8412,
	})
	require.NoError(t, err, "UpsertMessage email")

	// No subject, no snippet, no platform id: the store COALESCEs all three to
	// the empty string, which is exactly what a `required` declaration rejects.
	chat, err = st.UpsertMessage(&store.Message{
		SourceID:       src.ID,
		ConversationID: convID,
		MessageType:    "imessage",
		SentAt:         sql.NullTime{Time: time.Date(2026, 3, 2, 11, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(t, err, "UpsertMessage chat")
	return email, chat
}

// TestChangesEndpoint_PagesSatisfyTheGeneratedClientContract feeds the handler's
// real output into the published Go client's own model and validator.
//
// The API and store tests decode into this package's structs, which accept
// anything; the contract a consumer actually holds is the generated one, where a
// field declared required is rejected when it is absent OR empty. That gap is
// how a feed whose every row omits something shipped a client that refused its
// own server's ordinary 200s.
func TestChangesEndpoint_PagesSatisfyTheGeneratedClientContract(t *testing.T) {
	t.Run("a page of live and sparse rows", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		srv, st := newChangesServer(t)
		email, chat := seedSparseChangedMessages(t, st)
		settleChangesClock(t, srv)

		page := decodeGeneratedChangesPage(t, srv, changesTarget("", 10))
		require.NoError(page.Validate(),
			"the generated client must accept an ordinary page: a live message has "+
				"no deletion timestamps and a chat message has no subject")

		rows := make(map[int64]generated.ChangedMessageJSON, len(page.Messages))
		for _, row := range page.Messages {
			rows[row.ID] = row
		}
		require.Contains(rows, email, "the email row must be in the page")
		require.Contains(rows, chat, "the chat row must be in the page")

		// Without these the validation above could pass vacuously on rows that
		// happened to be fully populated.
		assert.Nil(rows[email].ReceivedAt, "the email has no received_at")
		assert.Nil(rows[email].DeletedAt, "the email was never deleted")
		assert.Nil(rows[email].DeletedFromSourceAt, "the email is still at the source")
		assert.Nil(rows[chat].Subject, "the chat message has no subject")
		assert.Nil(rows[chat].Snippet, "the chat message has no snippet")
		assert.Nil(rows[chat].SourceMessageID, "the chat message has no platform id")
	})

	t.Run("the first poll of an empty archive", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		srv, _ := newChangesServer(t)

		page := decodeGeneratedChangesPage(t, srv, changesTarget("", 10))
		require.NoError(page.Validate(),
			"a caller that has never polled still gets a cursor for the start of the "+
				"archive, which is what makes next_cursor declarable as required")
		assert.Empty(page.Messages, "messages")
		assert.NotEmpty(page.NextCursor, "next_cursor")
		assert.NotEmpty(page.ServerTime, "server_time is always a clock reading")
	})
}

// decodeGeneratedChangesPage serves one feed request and decodes the response
// into the published client's model rather than this package's.
func decodeGeneratedChangesPage(t *testing.T, srv *Server, target string) generated.ChangesResponse {
	t.Helper()
	w := doGet(srv, target)
	require.Equalf(t, http.StatusOK, w.Code, "GET %s: %s", target, w.Body.String())
	var page generated.ChangesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page),
		"decode the response into the generated client model")
	return page
}

// TestChangesResponseFieldsAreAllTracked asserts the feed's correctness
// invariant, which is ONE-DIRECTIONAL: every JSON field of a response item must
// be tracked by the content_changed_at triggers. A field outside the tracked
// set would be cached stale by a consumer forever, because no trigger can
// invalidate it.
//
// The converse does NOT hold and is deliberately not asserted:
// MessagesContentColumns legitimately contains columns the feed does not return
// (sender_id and metadata are tracked because changing them means "re-read this
// message", but neither is in the response).
//
// Two exemptions:
//   - id, source_id: immutable identity, so no trigger is needed.
//   - content_changed_at: the watermark itself. It is classified as
//     non-content — a trigger keying off it would recurse — but it is
//     necessarily present in every response item as the cursor.
func TestChangesResponseFieldsAreAllTracked(t *testing.T) {
	assert := assert.New(t)
	exempt := []string{"id", "source_id", "content_changed_at"}

	for field := range reflect.TypeFor[ChangedMessageJSON]().Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		if slices.Contains(exempt, name) {
			continue
		}
		assert.Containsf(store.MessagesContentColumns, name,
			"the feed reports %q, so a change to messages.%s must move "+
				"content_changed_at; otherwise every consumer caches it stale forever",
			name, name)
	}
}

// TestChangesEndpoint_PublishesHowFarItIsComplete covers the field a consumer
// needs to tell a quiet archive from a blocked feed.
//
// The page stops below the oldest write that could still commit, so an open
// write transaction pins it. In that state the response is otherwise
// indistinguishable from being caught up — no rows, has_more false — while
// server_time keeps moving. complete_through is what says which one it is.
func TestChangesEndpoint_PublishesHowFarItIsComplete(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)

	ids := seedChangedMessages(t, st, 2)
	settleChangesClock(t, srv)

	caughtUp := getChangesPage(t, srv, changesTarget("", 10))
	require.Len(caughtUp.Messages, 2, "the seeded messages must be delivered first")
	caughtUpValue := changesCompleteThroughString(t, caughtUp)
	completeThrough, err := time.Parse(time.RFC3339Nano, caughtUpValue)
	require.NoErrorf(err, "complete_through %q must parse as RFC3339", caughtUpValue)
	serverTime, err := time.Parse(time.RFC3339Nano, caughtUp.ServerTime)
	require.NoErrorf(err, "server_time %q must parse as RFC3339", caughtUp.ServerTime)
	assert.Falsef(completeThrough.After(serverTime),
		"complete_through %s is after server_time %s: the feed cannot be complete "+
			"through an instant the database clock has not reached",
		caughtUpValue, caughtUp.ServerTime)

	// A writer stamps a change and holds its transaction open.
	tx, err := st.DB().BeginTx(context.Background(), nil)
	require.NoError(err, "begin the pending write")
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(
		st.Rebind(`UPDATE messages SET subject = ? WHERE id = ?`), "pending", ids[0])
	require.NoError(err, "stamp the pending change")

	held := getChangesPage(t, srv,
		changesTarget(caughtUp.NextCursor, 10))
	assert.Empty(held.Messages, "an uncommitted change must not be reported")
	heldValue := changesCompleteThroughString(t, held)
	heldThrough, err := time.Parse(time.RFC3339Nano, heldValue)
	require.NoErrorf(err, "complete_through %q must parse as RFC3339", heldValue)
	heldServerTime, err := time.Parse(time.RFC3339Nano, held.ServerTime)
	require.NoErrorf(err, "server_time %q must parse as RFC3339", held.ServerTime)
	assert.Truef(heldServerTime.After(heldThrough),
		"the clock reads %s and the feed claims to be complete through %s, with a "+
			"write still pending: a held-back feed that publishes complete_through "+
			"== server_time is telling a consumer it is caught up when it is not",
		held.ServerTime, heldValue)

	require.NoError(tx.Commit(), "commit the pending change")
	deadline := time.Now().Add(20 * time.Second)
	var resumed ChangesResponse
	for {
		resumed = getChangesPage(t, srv,
			changesTarget(held.NextCursor, 10))
		if len(resumed.Messages) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	require.Len(resumed.Messages, 1, "the committed change must arrive")
	assert.Equal(ids[0], resumed.Messages[0].ID, "the changed message")
	resumedValue := changesCompleteThroughString(t, resumed)
	resumedThrough, err := time.Parse(time.RFC3339Nano, resumedValue)
	require.NoErrorf(err, "complete_through %q must parse as RFC3339", resumedValue)
	assert.Truef(resumedThrough.After(heldThrough),
		"complete_through stayed at %s once the write finished: a bound that never "+
			"recovers is a stalled feed, not a cautious one", resumedValue)
}

// TestChangesEndpoint_StalledFeedIsLogged is the operator's half of the same
// signal. complete_through tells the consumer; nothing tells whoever runs the
// server, and the cause — a connection sitting inside a transaction — is theirs
// to fix, not the consumer's.
func TestChangesEndpoint_StalledFeedIsLogged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	logs := &bytes.Buffer{}
	stalledFor := 9 * time.Minute
	serverTime := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store: &stubChangedMessageLister{
			mockStore: &mockStore{},
			page: store.ChangedMessagePage{
				ServerTime:      serverTime,
				CompleteThrough: serverTime.Add(-stalledFor),
			},
		},
		Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	relaxChangeFeedRateLimit(t, srv)

	resp := getChangesPage(t, srv, changesTarget("", 10))
	require.Empty(resp.Messages, "the stub serves an empty page")

	assert.Contains(logs.String(), "message change feed is not advancing",
		"a feed that has stopped advancing must reach the operator's logs: the "+
			"response says so, but nobody running the server is reading someone "+
			"else's polling responses")
	assert.Contains(logs.String(), stalledFor.String(),
		"the log line must carry how far behind the feed is, so an operator can "+
			"tell a momentary batch from a connection left open since Tuesday")

	for range 5 {
		getChangesPage(t, srv, changesTarget("", 10))
	}
	assert.Equal(1, strings.Count(logs.String(), "message change feed is not advancing"),
		"consumers poll, so the condition is re-observed on every request; one "+
			"stuck connection must not become a log flood")
}

// TestChangesEndpoint_FeedWithNoBoundYetLogsAFiniteLag covers the one state in
// which complete_through is not an instant the feed reached but the absence of
// one.
//
// A store that has never established a commit bound reports the zero time — on
// SQLite, a server that has not yet caught the database with its write lock
// free, which a restart during a bulk import produces. The page is correct (it
// is complete through nothing, so it carries no rows and moves no cursor), but
// subtracting year 1 from now saturates time.Duration, and the operator's
// warning would then read "lag=2562047h47m16.854775807s" — the saturated value,
// which Round(time.Second) cannot shorten — looking like a corrupt clock rather
// than a server that started a moment ago. The lag is not merely large here; it
// is undefined, and the log has to say which of the two it is.
func TestChangesEndpoint_FeedWithNoBoundYetLogsAFiniteLag(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	logs := &bytes.Buffer{}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store: &stubChangedMessageLister{
			mockStore: &mockStore{},
			page: store.ChangedMessagePage{
				ServerTime: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
				// CompleteThrough left zero: no bound established yet.
			},
		},
		Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	resp := getChangesPage(t, srv, changesTarget("", 10))
	require.Empty(resp.Messages, "a feed with no bound can publish no rows")

	assert.Contains(logs.String(), "message change feed is not advancing",
		"a feed that has never established a bound is not advancing, and the "+
			"operator has the same problem to fix as any other stall")
	assert.NotContains(logs.String(), "2562047h",
		"the lag against the zero time saturates time.Duration; reporting the "+
			"saturated value tells an operator their clock is broken when what "+
			"actually happened is that no bound has been established yet")
	assert.Contains(logs.String(), "no commit bound",
		"the cause must distinguish 'a transaction has been open for N minutes' "+
			"from 'nothing has ever been proved committed', because they are fixed "+
			"differently")
}

// TestChangesEndpoint_CompleteThroughIsAReachabilityBoundNotACursor pins what
// complete_through actually promises, which is weaker than it reads.
//
// It bounds what the feed is COMPLETE through, not what this response handed
// over: when the page filled, everything between the last row and that instant
// is still waiting behind next_cursor. The published wording used to say the
// change had "been offered to you", and a consumer that believed it and set its
// next cursor from complete_through skipped every one of those rows silently.
// So the property is two-sided — the gap is real (a consumer must not treat the
// bound as a cursor), and following next_cursor closes it completely.
func TestChangesEndpoint_CompleteThroughIsAReachabilityBoundNotACursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newChangesServer(t)
	seeded := seedChangedMessages(t, st, 12)
	settleChangesClock(t, srv)

	first := getChangesPage(t, srv, changesTarget("", 3))
	require.True(first.HasMore, "a page of 3 out of 12 must report more to come")
	boundValue := changesCompleteThroughString(t, first)
	bound, err := time.Parse(time.RFC3339Nano, boundValue)
	require.NoErrorf(err, "complete_through %q must parse as RFC3339", boundValue)

	below := countMessagesStampedBelow(t, st, bound)
	assert.Greaterf(below, first.Count,
		"complete_through %s stands above %d committed changes but the page carried "+
			"%d: a consumer that resumed from the bound would skip the difference, "+
			"which is why it must never be used as a cursor",
		boundValue, below, first.Count)

	// Following next_cursor instead is what the guarantee is actually about.
	delivered := map[int64]bool{}
	page := first
	for range 20 {
		for _, m := range page.Messages {
			delivered[m.ID] = true
		}
		if !page.HasMore {
			break
		}
		page = getChangesPage(t, srv, changesTarget(page.NextCursor, 3))
	}
	assert.False(page.HasMore, "the walk must reach the end of the feed")
	for _, id := range seeded {
		assert.Truef(delivered[id],
			"message %d was committed below the first page's complete_through (%s) and "+
				"following next_cursor never produced it: the bound would then promise "+
				"something the cursor does not deliver", id, boundValue)
	}
}

// countMessagesStampedBelow counts the rows whose watermark is strictly below
// instant, binding it the way each backend compares watermarks: PostgreSQL
// parses a real timestamptz, SQLite compares the trigger's textual format
// lexically.
func countMessagesStampedBelow(t *testing.T, st *store.Store, instant time.Time) int {
	t.Helper()
	var arg any = instant.UTC()
	if !st.IsPostgreSQL() {
		arg = instant.UTC().Format(store.SQLiteTimestampLayout)
	}
	var n int
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT count(*) FROM messages WHERE content_changed_at < ?`), arg).Scan(&n),
		"count the committed changes below the bound")
	return n
}

// TestChangesEndpoint_FutureCursorClampsToTheCommitBoundNotTheClock pins that
// recovering a consumer whose cursor is above the database clock never moves that
// cursor above a change that is stamped but not yet committed.
//
// While a writer holds an open transaction, complete_through sits strictly below
// server_time and the in-flight row's watermark sits between them. Clamping to
// server_time -- which this did -- places the cursor above that row, and when the
// writer commits the row is below the cursor forever. The bound is by
// construction below every write it can see, so a cursor placed there cannot
// skip one. The writes it cannot see, and every other exception to what the
// feed delivers, are enumerated in one place: docs/api-server.md's delivery
// contract.
func TestChangesEndpoint_FutureCursorClampsToTheCommitBoundNotTheClock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	serverTime := time.Date(2026, 7, 26, 10, 0, 30, 0, time.UTC)
	// A writer has been open since :10, so the feed is complete only through :10
	// even though the clock reads :30. That writer's row, stamped at :20, is
	// still uncommitted.
	completeThrough := time.Date(2026, 7, 26, 10, 0, 10, 0, time.UTC)
	inFlightStamp := time.Date(2026, 7, 26, 10, 0, 20, 0, time.UTC)

	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store: &stubChangedMessageLister{
			mockStore: &mockStore{},
			page: store.ChangedMessagePage{
				Messages:        nil,
				ServerTime:      serverTime,
				CompleteThrough: completeThrough,
			},
		},
		Logger: testLogger(),
	})

	future := serverTime.Add(time.Hour)
	resp := getChangesPage(t, srv, changesTarget(changesCursor(t, srv, future, 7), 10))

	// Equality, not "not after". A merely-lower cursor is satisfied by the zero
	// time, and an implementation that rewound the consumer to the start of the
	// archive on every future cursor -- which this plan explicitly rejects --
	// would pass a `not after` assertion while being badly wrong. The tiebreak
	// resets with it: it belonged to a different instant.
	require.Equal(changesInstantCursor(t, srv, completeThrough), resp.NextCursor,
		"the recovered cursor must be exactly the commit bound (%s) with no "+
			"tiebreak; anything above it skips the change stamped at %s by the "+
			"still-open writer, and anything below it replays the archive",
		completeThrough, inFlightStamp)
	assert.NotEqual(changesCursor(t, srv, future, 7), resp.NextCursor,
		"and it must not be the unsatisfiable cursor that was sent")
}

// TestChangesEndpoint_FutureCursorIsEchoedWhenNoBoundIsEstablished pins the one
// case where the clamp must not fire. A server that has never taken a bound
// reading reports complete_through as the zero time; clamping down to it would
// replay the whole archive, and clamping to the clock would be the unsafe move
// this fix removes. Echoing holds the consumer's place until the bound resolves.
func TestChangesEndpoint_FutureCursorIsEchoedWhenNoBoundIsEstablished(t *testing.T) {
	assert := assert.New(t)

	serverTime := time.Date(2026, 7, 26, 10, 0, 30, 0, time.UTC)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store: &stubChangedMessageLister{
			mockStore: &mockStore{},
			page: store.ChangedMessagePage{
				Messages:        nil,
				ServerTime:      serverTime,
				CompleteThrough: time.Time{}, // no bound established yet
			},
		},
		Logger: testLogger(),
	})

	// A nonzero tiebreak, so the echo is checked as a whole position. Resetting
	// the id here would re-deliver the start of that instant on every poll, which
	// the clamp branch accepts deliberately but this branch must not: nothing has
	// been clamped, so there is nothing to re-deliver.
	sent := changesCursor(t, srv, serverTime.Add(time.Hour), 7)
	resp := getChangesPage(t, srv, changesTarget(sent, 10))

	assert.Equal(sent, resp.NextCursor,
		"with no bound established the cursor must be echoed unchanged, tiebreak "+
			"included: not clamped to the clock and not reset to the zero time")
}
