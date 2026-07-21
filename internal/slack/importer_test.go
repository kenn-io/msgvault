package slack

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// tsBase anchors test message times ~25h in the past: recent enough that
// thread roots stay inside the 30-day tracking lookback, old enough that
// offsets up to a few hours never land in the future.
var tsBase = time.Now().Add(-25 * time.Hour).UTC().Truncate(time.Second)

// ts renders a Slack ts for offset minutes after tsBase.
func ts(minutes int) string {
	return strconv.FormatInt(tsBase.Add(time.Duration(minutes)*time.Minute).Unix(), 10) + ".000100"
}

// tsFresh renders a Slack ts a few seconds in the future — a message created
// "now", strictly after any backfill pin or sweep watermark taken earlier in
// the test (real replies are always created at post time, never back-dated).
func tsFresh(offsetSeconds int) string {
	return strconv.FormatInt(time.Now().Add(time.Duration(2+offsetSeconds)*time.Second).Unix(), 10) + ".000100"
}

// testWorkspace builds a fake workspace exercising every persist path:
// channels with threads, reactions, mentions, edits, bot messages, a group
// DM, and a 1:1 DM.
func testWorkspace(t *testing.T) *fakeSlack {
	t.Helper()
	f := newFakeSlack(t)
	f.users = []map[string]any{
		{"id": "UME", "name": "me", "real_name": "Test User",
			"profile": map[string]any{"email": "me@example.com", "display_name": "Me"}},
		{"id": "UALICE", "name": "alice", "real_name": "Alice Example",
			"profile": map[string]any{"email": "alice@example.com", "display_name": "Alice"}},
		{"id": "UBOB", "name": "bob", "real_name": "Bob Example",
			"profile": map[string]any{}}, // no email: resolves by bare ID
	}
	general := &fakeConv{
		ID: "C01", Name: "general", Kind: "public",
		Members: []string{"UME", "UALICE", "UBOB"},
	}
	for i := range 8 {
		general.Msgs = append(general.Msgs, fakeMsg{TS: ts(i), User: "UALICE", Text: "hello " + strconv.Itoa(i)})
	}
	general.Msgs[1].Text = "ping <@UME> see <https://example.com|the docs>"
	general.Msgs[2].Reactions = []map[string]any{
		{"name": "thumbsup", "users": []string{"UME", "UBOB"}, "count": 2},
	}
	general.Msgs[3].Edited = true
	general.Msgs[4].User = ""
	general.Msgs[4].BotID = "B042"
	general.Msgs[4].Username = "deploybot"
	general.Msgs[4].Text = ""
	general.Msgs[4].LegacyAttachments = []map[string]any{
		{"fallback": "Build #42 failed on main"},
	}
	general.Msgs[5].Replies = []fakeMsg{
		{TS: ts(100), ThreadTS: general.Msgs[5].TS, User: "UBOB", Text: "reply one"},
		{TS: ts(101), ThreadTS: general.Msgs[5].TS, User: "UME", Text: "reply two"},
	}
	f.convs = []*fakeConv{
		general,
		{ID: "C02", Name: "secrets", Kind: "private",
			Members: []string{"UME", "UALICE"},
			Msgs:    []fakeMsg{{TS: ts(30), User: "UALICE", Text: "private hi"}}},
		{ID: "G01", Name: "mpdm-me--alice--bob-1", Kind: "mpim",
			Members: []string{"UME", "UALICE", "UBOB"},
			Msgs:    []fakeMsg{{TS: ts(10), User: "UME", Text: "group hi"}}},
		{ID: "D01", Kind: "im", IMUser: "UALICE",
			Msgs: []fakeMsg{{TS: ts(20), User: "UALICE", Text: "dm hi"}}},
	}
	return f
}

// totalWorkspaceMessages is the archived-row count for the full test
// workspace: 8 channel + 1 private + 1 mpim + 1 im top-level, plus 2 thread
// replies (the root re-upserts in place).
const totalWorkspaceMessages = 13

func testImporter(t *testing.T, f *fakeSlack) (*Importer, ImportOptions) {
	t.Helper()
	prevInterval := checkpointMinInterval
	checkpointMinInterval = 0
	t.Cleanup(func() { checkpointMinInterval = prevInterval })

	srv := f.serve()
	client := NewClient(srv.URL, "xoxp-test")
	client.disableRateLimits()
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, client, "T01")
	return imp, ImportOptions{TeamID: "T01", UserID: "UME", NoMedia: true}
}

func TestImportEndToEnd(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(4, sum.ConversationsProcessed)
	assert.Equal(2, sum.RepliesFetched)
	assert.Zero(sum.FetchErrors)

	var msgCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&msgCount))
	assert.Equal(totalWorkspaceMessages, msgCount)

	// Conversation types and titles.
	var title, convType string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT title, conversation_type FROM conversations WHERE source_conversation_id = ?`), "C01").
		Scan(&title, &convType))
	assert.Equal("#general", title)
	assert.Equal("channel", convType)
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT title, conversation_type FROM conversations WHERE source_conversation_id = ?`), "D01").
		Scan(&title, &convType))
	assert.Equal("Alice", title)
	assert.Equal("direct_chat", convType)
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT title, conversation_type FROM conversations WHERE source_conversation_id = ?`), "C02").
		Scan(&title, &convType))
	assert.Equal("#secrets", title, "private channels archive like channels")
	assert.Equal("channel", convType)
	var privateMsgs int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE c.source_conversation_id = ?`), "C02").Scan(&privateMsgs))
	assert.Equal(1, privateMsgs)

	// Email-based identity: Alice deduped against mail archives by address.
	var aliceID int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT id FROM participants WHERE email_address = ?`), "alice@example.com").Scan(&aliceID))

	// Thread replies linked to their root.
	var linked int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM messages child
		JOIN messages parent ON parent.id = child.reply_to_message_id
		WHERE child.source_message_id = ? AND parent.source_message_id = ?`),
		"C01:"+ts(100), "C01:"+ts(5)).Scan(&linked))
	assert.Equal(1, linked)

	// Reactions: two users on message 2.
	var reactions int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM reactions r
		JOIN messages m ON m.id = r.message_id
		WHERE m.source_message_id = ? AND r.reaction_value = 'thumbsup'`), "C01:"+ts(2)).Scan(&reactions))
	assert.Equal(2, reactions)

	// Mention row for <@UME>, with mrkdwn rendered in the body.
	var mentions int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_recipients mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_message_id = ? AND mr.recipient_type = 'mention'`), "C01:"+ts(1)).Scan(&mentions))
	assert.Equal(1, mentions)
	var body string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mb.body_text FROM message_bodies mb
		JOIN messages m ON m.id = mb.message_id
		WHERE m.source_message_id = ?`), "C01:"+ts(1)).Scan(&body))
	assert.Equal("ping @Me see the docs (https://example.com)", body)

	// Edited flag, bot sender, raw archive format.
	var edited bool
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT is_edited FROM messages WHERE source_message_id = ?`), "C01:"+ts(3)).Scan(&edited))
	assert.True(edited)
	var botSender string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT p.display_name FROM messages m JOIN participants p ON p.id = m.sender_id
		WHERE m.source_message_id = ?`), "C01:"+ts(4)).Scan(&botSender))
	assert.Equal("deploybot", botSender)
	// The bot message's content lives in a legacy attachment (empty text);
	// its fallback must be the searchable body.
	var botBody string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mb.body_text FROM message_bodies mb
		JOIN messages m ON m.id = mb.message_id WHERE m.source_message_id = ?`), "C01:"+ts(4)).Scan(&botBody))
	assert.Equal("Build #42 failed on main", botBody)
	var rawFormat string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mr.raw_format FROM message_raw mr JOIN messages m ON m.id = mr.message_id
		WHERE m.source_message_id = ?`), "C01:"+ts(0)).Scan(&rawFormat))
	assert.Equal("slack_json", rawFormat)

	// Membership recorded.
	var members int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM conversation_participants cp
		JOIN conversations c ON c.id = cp.conversation_id
		WHERE c.source_conversation_id = ?`), "C01").Scan(&members))
	assert.Equal(3, members)
}

func TestImportIncrementalCatchesNewMessagesAndLateReplies(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// A new top-level message and a late reply to the (old) thread root.
	f.mu.Lock()
	general := f.conv("C01")
	general.Msgs = append(general.Msgs, fakeMsg{TS: ts(200), User: "UBOB", Text: "fresh news"})
	lateReply := tsFresh(0)
	root := general.findRoot(ts(5))
	root.Replies = append(root.Replies, fakeMsg{TS: lateReply, ThreadTS: root.TS, User: "UALICE", Text: "late reply"})
	f.mu.Unlock()

	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(1, sum.RepliesFetched, "only the late reply is new; earlier replies are behind the thread cursor")

	for _, id := range []string{"C01:" + ts(200), "C01:" + lateReply} {
		var n int
		require.NoError(st.DB().QueryRow(st.Rebind(
			`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), id).Scan(&n))
		assert.Equal(1, n, id)
	}
}

func TestImportIncrementalMidWindowFailureDoesNotAdvanceCursor(t *testing.T) {
	require := require.New(t)
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// Five new messages: an incremental window of two pages (fake pageSize 3,
	// newest-first). Page one serves ts(304)..ts(302); page two dies.
	f.mu.Lock()
	general := f.conv("C01")
	for i := range 5 {
		general.Msgs = append(general.Msgs, fakeMsg{TS: ts(300 + i), User: "UBOB", Text: "burst " + strconv.Itoa(i)})
	}
	f.failHistoryContinuations = true
	f.mu.Unlock()

	_, err = imp.Import(context.Background(), opts)
	require.Error(err, "a run with fetch errors must not report success")

	// The cursor must not have advanced past the unfetched older page: after
	// healing, ALL five burst messages are archived exactly once.
	f.mu.Lock()
	f.failHistoryContinuations = false
	f.mu.Unlock()
	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	for i := range 5 {
		var n int
		require.NoError(st.DB().QueryRow(st.Rebind(
			`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C01:"+ts(300+i)).Scan(&n))
		assert.Equal(t, 1, n, "burst message %d", i)
	}
}

func TestBackfillThreadFetchFailureParksDrainDebt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	// Backfill records each root as drain debt before its page's cursor
	// advances; a reply-fetch failure parks the debt entry at its resume
	// point, and the run must not report success.
	f.failReplies[ts(5)] = true
	sum, err := imp.Import(context.Background(), opts)
	require.Error(err, "a run with fetch errors must not report success")
	assert.Positive(sum.FetchErrors)

	// The replies never landed.
	var n int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C01:"+ts(100)).Scan(&n))
	assert.Zero(n)

	// Healed: the resumed backfill refetches the page and its threads.
	f.mu.Lock()
	delete(f.failReplies, ts(5))
	f.mu.Unlock()
	sum, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(2, sum.RepliesFetched)
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C01:"+ts(100)).Scan(&n))
	assert.Equal(1, n)
	var total, distinct int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(DISTINCT source_message_id) FROM messages WHERE message_type='slack'`).Scan(&distinct))
	assert.Equal(distinct, total, "page refetch after thread failure must not duplicate")
}

func TestImportHistoryFailureLeavesConversationResumable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	f.failHistory["C01"] = true
	_, err := imp.Import(context.Background(), opts)
	require.Error(err)

	// The healthy conversations still synced.
	var n int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "D01:"+ts(20)).Scan(&n))
	assert.Equal(1, n)

	f.mu.Lock()
	delete(f.failHistory, "C01")
	f.mu.Unlock()
	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	var total int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	assert.Equal(totalWorkspaceMessages, total)
}

func TestImportInterruptResumesWithoutDuplicates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	// Cancel partway through the first run: as soon as the first history
	// page has been served, mid-conversation-walk.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			f.mu.Lock()
			served := f.historyCalls > 0
			f.mu.Unlock()
			if served {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	_, _ = imp.Import(ctx, opts)

	// Resume to completion: every message exactly once.
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	var total, distinct int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(DISTINCT source_message_id) FROM messages WHERE message_type='slack'`).Scan(&distinct))
	assert.Equal(totalWorkspaceMessages, total)
	assert.Equal(distinct, total)
}

func TestImportFullReUpsertsInPlace(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// An old message is edited at the source; only --full re-walks it.
	f.mu.Lock()
	f.conv("C01").Msgs[0].Text = "hello 0 (edited)"
	f.mu.Unlock()

	full := opts
	full.Full = true
	_, err = imp.Import(context.Background(), full)
	require.NoError(err)

	var body string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mb.body_text FROM message_bodies mb
		JOIN messages m ON m.id = mb.message_id WHERE m.source_message_id = ?`), "C01:"+ts(0)).Scan(&body))
	assert.Equal("hello 0 (edited)", body)
	var total int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	assert.Equal(totalWorkspaceMessages, total, "full run must upsert, not duplicate")
}

func TestImportLimitLeavesBackfillResumable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	// A cap below #general's 8 top-level messages: the first run must stop
	// early without marking the conversation complete or advancing past
	// unfetched pages.
	limited := opts
	limited.Limit = 4
	limited.NoThreads = true
	_, err := imp.Import(context.Background(), limited)
	require.NoError(err)
	var partial int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&partial))
	assert.Less(partial, totalWorkspaceMessages)

	// An uncapped run completes the backfill: every message exactly once.
	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	var total, distinct int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(DISTINCT source_message_id) FROM messages WHERE message_type='slack'`).Scan(&distinct))
	assert.Equal(totalWorkspaceMessages, total, "limited first run must not lose messages")
	assert.Equal(distinct, total)
}

// oldThreadWorkspace builds a workspace whose only thread root is ~10 days
// old — far older than any recent-activity window, so reply capture can only
// come from mechanisms that key on the REPLY's creation time.
func oldThreadWorkspace(t *testing.T) (*fakeSlack, string) {
	t.Helper()
	f := newFakeSlack(t)
	f.users = []map[string]any{
		{"id": "UME", "name": "me", "profile": map[string]any{"email": "me@example.com"}},
	}
	rootTS := ts(-14400) // ~10 days before tsBase
	f.convs = []*fakeConv{{
		ID: "C09", Name: "archive", Kind: "public", Members: []string{"UME"},
		Msgs: []fakeMsg{
			{TS: rootTS, User: "UME", Text: "ancient root",
				Replies: []fakeMsg{{TS: ts(-14390), ThreadTS: rootTS, User: "UME", Text: "ancient reply"}}},
			{TS: ts(0), User: "UME", Text: "recent chatter"},
		},
	}}
	return f, rootTS
}

func TestSweepFindsLateReplyToAncientThread(t *testing.T) {
	require := require.New(t)
	f, rootTS := oldThreadWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// A NEW reply lands on the ~10-day-old thread after backfill. No
	// lookback window applies: the sweep discovers by the reply's creation
	// time, so root age is irrelevant (the old design's documented LB-3
	// blind spot).
	lateReply := tsFresh(0)
	f.mu.Lock()
	root := f.conv("C09").findRoot(rootTS)
	root.Replies = append(root.Replies, fakeMsg{TS: lateReply, ThreadTS: rootTS, User: "UME", Text: "late reply"})
	f.mu.Unlock()

	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	var linked int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM messages child
		JOIN messages parent ON parent.id = child.reply_to_message_id
		WHERE child.source_message_id = ? AND parent.source_message_id = ?`),
		"C09:"+lateReply, "C09:"+rootTS).Scan(&linked))
	assert.Equal(t, 1, linked, "the sweep must archive and link a late reply to an ancient root")
}

func TestSweepWatermarkHoldsOnCanonicalFetchFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, rootTS := oldThreadWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	lateReply := tsFresh(0)
	f.mu.Lock()
	root := f.conv("C09").findRoot(rootTS)
	root.Replies = append(root.Replies, fakeMsg{TS: lateReply, ThreadTS: rootTS, User: "UME", Text: "late reply"})
	// The canonical fetch is anchored at the discovered hit's ts.
	f.failReplies[lateReply] = true
	f.mu.Unlock()

	sum, err := imp.Import(context.Background(), opts)
	require.Error(err, "a sweep with a failed canonical fetch must not report success")
	assert.Positive(sum.FetchErrors)
	var n int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C09:"+lateReply).Scan(&n))
	require.Zero(n)

	// Healed: the watermark parked before the failed hit, so the next sweep
	// re-discovers and archives it — exactly once.
	f.mu.Lock()
	delete(f.failReplies, lateReply)
	f.mu.Unlock()
	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C09:"+lateReply).Scan(&n))
	assert.Equal(1, n)
	var total, distinct int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(DISTINCT source_message_id) FROM messages WHERE message_type='slack'`).Scan(&distinct))
	assert.Equal(distinct, total)
}

func TestSweepCertificationStaysBehindLagHorizon(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, rootTS := oldThreadWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// A reply created "now" — inside the lag window, newer than anything
	// the search index is certified to have served. The canonical fetch
	// still archives it, but certification must NOT follow fetched content
	// past the horizon: a not-yet-indexed reply in an UNRELATED thread
	// would land below the watermark and be skipped forever.
	lateReply := tsFresh(0)
	f.mu.Lock()
	root := f.conv("C09").findRoot(rootTS)
	root.Replies = append(root.Replies, fakeMsg{TS: lateReply, ThreadTS: rootTS, User: "UME", Text: "late reply"})
	f.mu.Unlock()

	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	var n int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C09:"+lateReply).Scan(&n))
	assert.Equal(1, n, "replies inside the lag window are still archived")

	state := imp.loadResumeState(sum.SourceID)
	require.NotEmpty(state.SweepWatermark)
	horizon := time.Now().Add(-sweepLagMargin)
	assert.True(tsTime(state.SweepWatermark).Before(horizon.Add(time.Second)),
		"certification %s must stay behind the lag horizon %s, never follow fetched content",
		state.SweepWatermark, tsFormat(horizon.UTC()))
}

func TestSweepTruncatedDayFailsRunAndHalts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, rootTS := oldThreadWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	lateReply := tsFresh(0)
	f.mu.Lock()
	root := f.conv("C09").findRoot(rootTS)
	root.Replies = append(root.Replies, fakeMsg{TS: lateReply, ThreadTS: rootTS, User: "UME", Text: "late reply"})
	// The day reports more results than search can ever page to: whatever
	// lies past the 10k ceiling is unreachable, so the run must fail
	// loudly instead of certifying past unarchived replies.
	f.searchTotalOverride = sweepTruncationCeiling + 1
	f.mu.Unlock()

	sum, err := imp.Import(context.Background(), opts)
	require.Error(err, "a truncated sweep day must fail the run, not silently skip unreachable replies")
	assert.Positive(sum.FetchErrors)
	// The reachable results (the day's earliest, ascending) were archived.
	var n int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C09:"+lateReply).Scan(&n))
	assert.Equal(1, n)

	// Healed (the day drained below the ceiling): a clean run, no dups.
	f.mu.Lock()
	f.searchTotalOverride = 0
	f.mu.Unlock()
	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	var total, distinct int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(DISTINCT source_message_id) FROM messages WHERE message_type='slack'`).Scan(&distinct))
	assert.Equal(distinct, total)
}

func TestSweepRecoversGapForReIncludedChannel(t *testing.T) {
	require := require.New(t)
	f, rootTS := oldThreadWorkspace(t)
	// A second channel that stays included throughout, so sweeps keep
	// advancing the workspace watermark while #archive is excluded.
	f.convs = append(f.convs, &fakeConv{
		ID: "C11", Name: "keep", Kind: "public", Members: []string{"UME"},
		Msgs: []fakeMsg{{TS: ts(1), User: "UME", Text: "keep hi"}},
	})
	imp, opts := testImporter(t, f)
	st := imp.store

	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// While #archive is excluded, a reply lands on its ancient thread, and
	// enough (warped) time passes that the workspace watermark certifies
	// past the reply's creation time.
	gapReply := tsFresh(0)
	f.mu.Lock()
	root := f.conv("C09").findRoot(rootTS)
	root.Replies = append(root.Replies, fakeMsg{TS: gapReply, ThreadTS: rootTS, User: "UME", Text: "reply while excluded"})
	f.mu.Unlock()

	imp.now = func() time.Time { return time.Now().Add(time.Hour) }
	excluded := opts
	excluded.ExcludeChannels = []string{"archive"}
	_, err = imp.Import(context.Background(), excluded)
	require.NoError(err)

	state := imp.loadResumeState(sum.SourceID)
	require.True(tsLess(gapReply, state.SweepWatermark),
		"test setup: the watermark must have certified past the excluded channel's reply")
	var n int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C09:"+gapReply).Scan(&n))
	require.Zero(n, "test setup: the reply must not be archived while its channel is excluded")

	// Re-included: the channel re-enters certified behind the watermark; a
	// channel-scoped gap sweep must recover the reply that the workspace
	// sweep — floored at the watermark — will never revisit.
	imp.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C09:"+gapReply).Scan(&n))
	assert.Equal(t, 1, n, "a reply created while its channel was excluded must be recovered on re-entry")
}

func TestImportLimitBoundsThreadReplies(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeSlack(t)
	f.users = []map[string]any{
		{"id": "UME", "name": "me", "profile": map[string]any{"email": "me@example.com"}},
	}
	busyRoot := fakeMsg{TS: ts(0), User: "UME", Text: "busy root"}
	for i := range 12 {
		busyRoot.Replies = append(busyRoot.Replies,
			fakeMsg{TS: ts(i + 1), ThreadTS: busyRoot.TS, User: "UME", Text: "reply " + strconv.Itoa(i)})
	}
	f.convs = []*fakeConv{{
		ID: "C20", Name: "busy", Kind: "public", Members: []string{"UME"},
		Msgs: []fakeMsg{busyRoot},
	}}
	imp, opts := testImporter(t, f)
	st := imp.store

	// One discovered thread must not blow through the budget: its
	// reply_count forecast charges the run at recording time, and the drain
	// fetches on budget-sized pages, parking the remainder as durable debt.
	limited := opts
	limited.Limit = 3
	_, err := imp.Import(context.Background(), limited)
	require.NoError(err)
	var partial int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&partial))
	assert.Positive(partial)
	assert.LessOrEqual(partial, 4, "--limit 3 must bound thread replies, not just top-level history")

	// The unlimited run completes the thread: every message exactly once.
	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	var total, distinct int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(DISTINCT source_message_id) FROM messages WHERE message_type='slack'`).Scan(&distinct))
	assert.Equal(13, total, "resumed runs must recover the budget-clipped replies")
	assert.Equal(distinct, total)
}

func TestStandingLimitConvergesThroughThreadDebt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeSlack(t)
	f.users = []map[string]any{
		{"id": "UME", "name": "me", "profile": map[string]any{"email": "me@example.com"}},
	}
	// The OLDEST message is a root with more replies than the standing
	// limit; several newer top-levels sit above it. Newest-first pagination
	// reaches the root last, so the drain must carry the thread across runs.
	busyRoot := fakeMsg{TS: ts(0), User: "UME", Text: "busy root"}
	for i := range 12 {
		busyRoot.Replies = append(busyRoot.Replies,
			fakeMsg{TS: ts(i + 1), ThreadTS: busyRoot.TS, User: "UME", Text: "reply " + strconv.Itoa(i)})
	}
	conv := &fakeConv{ID: "C30", Name: "steady", Kind: "public", Members: []string{"UME"}, Msgs: []fakeMsg{busyRoot}}
	for i := range 5 {
		conv.Msgs = append(conv.Msgs, fakeMsg{TS: ts(100 + i), User: "UME", Text: "top " + strconv.Itoa(i)})
	}
	f.convs = []*fakeConv{conv}
	imp, opts := testImporter(t, f)
	st := imp.store

	// A standing --limit cron must converge to a complete archive BY
	// ITSELF: each run makes durable progress (history pages, then reply
	// drain resumed from the per-thread drained-to ts) — no unlimited run
	// required, no stall.
	limited := opts
	limited.Limit = 4
	for range 12 {
		_, err := imp.Import(context.Background(), limited)
		require.NoError(err)
	}
	var total, distinct int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(DISTINCT source_message_id) FROM messages WHERE message_type='slack'`).Scan(&distinct))
	assert.Equal(18, total, "repeated limited runs must drain the whole archive, thread included")
	assert.Equal(distinct, total)
}

func TestGapCatchUpCoversPostBackfillRoots(t *testing.T) {
	require := require.New(t)
	f, rootTS := oldThreadWorkspace(t)
	// A legacy G-prefixed private channel (name-filterable, but outside the
	// probed in:<#C…> search-scope form, so its gap recovery runs through
	// the thread catch-up walk) plus a channel that keeps the workspace
	// watermark advancing while it is excluded.
	f.convs = append(f.convs,
		&fakeConv{ID: "G05", Name: "legacy", Kind: "private", Members: []string{"UME"},
			Msgs: []fakeMsg{{TS: ts(2), User: "UME", Text: "legacy hi"}}},
		&fakeConv{ID: "C11", Name: "keep", Kind: "public", Members: []string{"UME"},
			Msgs: []fakeMsg{{TS: ts(1), User: "UME", Text: "keep hi"}}},
	)
	_ = rootTS
	imp, opts := testImporter(t, f)
	st := imp.store

	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// While #legacy is excluded, a NEW thread starts there — root AND reply
	// both created after the channel's backfill pin — and the watermark
	// certifies past them. A catch-up walk bounded by the original pin
	// would never anchor this root, losing the reply permanently.
	gapRoot, gapReply := tsFresh(0), tsFresh(1)
	f.mu.Lock()
	f.conv("G05").Msgs = append(f.conv("G05").Msgs, fakeMsg{TS: gapRoot, User: "UME", Text: "root while excluded",
		Replies: []fakeMsg{{TS: gapReply, ThreadTS: gapRoot, User: "UME", Text: "reply while excluded"}}})
	f.mu.Unlock()

	imp.now = func() time.Time { return time.Now().Add(time.Hour) }
	excluded := opts
	excluded.ExcludeChannels = []string{"legacy"}
	_, err = imp.Import(context.Background(), excluded)
	require.NoError(err)

	state := imp.loadResumeState(sum.SourceID)
	require.True(tsLess(gapReply, state.SweepWatermark),
		"test setup: the watermark must have certified past the excluded channel's reply")

	// Re-entry flags the gap as thread debt; the following run's catch-up
	// walk must cover roots created AFTER the original backfill pin.
	imp.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	imp.now = func() time.Time { return time.Now().Add(3 * time.Hour) }
	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)

	var n int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "G05:"+gapReply).Scan(&n))
	require.Equal(1, n, "gap recovery must anchor threads rooted after the backfill pin")
}

func TestSweepDayBoundariesFollowHistoricalDST(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	require.NotNil(ny)

	// Search files messages by the user's IANA zone with HISTORICAL DST
	// rules (probed live against a corpus spanning transitions: winter
	// boundary at EST midnight even when queried in summer; the spring-
	// forward day served as a 23-hour span). Certification boundaries must
	// match, or an interrupted per-day sweep could certify an hour the
	// day's query never served.
	jan15 := time.Date(2026, 1, 15, 0, 0, 0, 0, ny)
	assert.Equal("2026-01-16T05:00:00Z", nextDayStart(jan15, ny).UTC().Format(time.RFC3339),
		"winter boundary is EST midnight (-5), regardless of the current offset")
	jun15 := time.Date(2026, 6, 15, 0, 0, 0, 0, ny)
	assert.Equal("2026-06-16T04:00:00Z", nextDayStart(jun15, ny).UTC().Format(time.RFC3339),
		"summer boundary is EDT midnight (-4)")
	mar8 := time.Date(2026, 3, 8, 0, 0, 0, 0, ny)
	assert.Equal("2026-03-08T05:00:00Z", mar8.UTC().Format(time.RFC3339))
	assert.Equal("2026-03-09T04:00:00Z", nextDayStart(mar8, ny).UTC().Format(time.RFC3339),
		"the spring-forward day is 23 hours (probed live: on:2026-03-08 served exactly this span)")

	// The resolver serves the IANA zone when it loads; sweeping with the
	// flat current offset instead would put winter boundaries an hour off.
	r := &participantResolver{users: map[string]User{
		"UNY":  {ID: "UNY", TZ: "America/New_York", TZOffset: -4 * 3600},
		"UBAD": {ID: "UBAD", TZ: "Not/AZone", TZOffset: 7200},
	}}
	winter := time.Date(2026, 1, 16, 5, 0, 0, 0, time.UTC).In(r.tzLocation("UNY"))
	assert.Equal(0, winter.Hour(), "sweep-day arithmetic must apply the historical winter offset, not the current one")
	_, off := time.Date(2026, 1, 15, 0, 0, 0, 0, r.tzLocation("UBAD")).Zone()
	assert.Equal(7200, off, "an unloadable zone name falls back to the fixed current offset")
}

func TestSweepSkipsNotDoneConversations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, rootTS := oldThreadWorkspace(t)
	// A second conversation that cannot finish backfilling: its search hits
	// must be ignored — the backfill owns not-done conversations.
	f.convs = append(f.convs, &fakeConv{
		ID: "C10", Name: "stuck", Kind: "public", Members: []string{"UME"},
		Msgs: []fakeMsg{
			{TS: ts(-100), User: "UME", Text: "stuck root",
				Replies: []fakeMsg{{TS: tsFresh(4), ThreadTS: ts(-100), User: "UME", Text: "stuck late reply"}}},
		},
	})
	f.failHistory["C10"] = true
	stuckReply := f.convs[len(f.convs)-1].Msgs[0].Replies[0].TS
	imp, opts := testImporter(t, f)
	st := imp.store

	// C09 completes and gets a late reply; C10 never finishes backfill.
	_, err := imp.Import(context.Background(), opts)
	require.Error(err) // C10's history failure keeps the run partial
	lateReply := tsFresh(0)
	f.mu.Lock()
	root := f.conv("C09").findRoot(rootTS)
	root.Replies = append(root.Replies, fakeMsg{TS: lateReply, ThreadTS: rootTS, User: "UME", Text: "late reply"})
	f.mu.Unlock()

	_, err = imp.Import(context.Background(), opts)
	require.Error(err) // still partial: C10 still failing
	var n int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C09:"+lateReply).Scan(&n))
	assert.Equal(1, n, "done conversations are swept even while others are stuck")
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C10:"+stuckReply).Scan(&n))
	assert.Zero(n, "not-done conversations are owned by backfill, never the sweep")

	// C10 heals: its backfill fetches root AND replies inline.
	f.mu.Lock()
	delete(f.failHistory, "C10")
	f.mu.Unlock()
	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C10:"+stuckReply).Scan(&n))
	assert.Equal(1, n)
}

func TestImportLimitBoundsProcessedMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	limited := opts
	limited.Limit = 1
	limited.NoThreads = true
	_, err := imp.Import(context.Background(), limited)
	require.NoError(err)

	// Page requests are sized to the remaining budget, so each conversation
	// processes at most its limit — not a full server page.
	var total int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	assert.LessOrEqual(total, 4, "--limit 1 must not fetch whole pages per conversation")
	assert.Positive(total)
}

func TestImportLimitedRunsDrainIncrementalBacklog(t *testing.T) {
	require := require.New(t)
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// A 9-message backlog against a standing --limit 2: without the
	// incremental window checkpoint, every limited run restarts from the
	// newest page and the backlog never drains.
	f.mu.Lock()
	general := f.conv("C01")
	for i := range 9 {
		general.Msgs = append(general.Msgs, fakeMsg{TS: ts(400 + i), User: "UBOB", Text: "backlog " + strconv.Itoa(i)})
	}
	f.mu.Unlock()

	limited := opts
	limited.Limit = 2
	limited.NoThreads = true
	for range 8 {
		_, err = imp.Import(context.Background(), limited)
		require.NoError(err)
	}
	for i := range 9 {
		var n int
		require.NoError(st.DB().QueryRow(st.Rebind(
			`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C01:"+ts(400+i)).Scan(&n))
		assert.Equal(t, 1, n, "backlog message %d must be drained by repeated limited runs", i)
	}
}

func TestImportDiscoversFirstReplyToOlderMessage(t *testing.T) {
	require := require.New(t)
	f, rootTS := oldThreadWorkspace(t)
	// The 10-day-old message starts with NO replies: it is archived as a
	// plain message, not tracked as a thread root.
	f.mu.Lock()
	f.conv("C09").findRoot(rootTS).Replies = nil
	f.mu.Unlock()
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// First reply arrives long after archiving. The reply sweep must
	// discover it by creation time — the parent's age is irrelevant.
	lateReply := tsFresh(0)
	f.mu.Lock()
	root := f.conv("C09").findRoot(rootTS)
	root.Replies = []fakeMsg{{TS: lateReply, ThreadTS: rootTS, User: "UME", Text: "first ever reply"}}
	f.mu.Unlock()

	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	var n int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C09:"+lateReply).Scan(&n))
	assert.Equal(t, 1, n, "a first reply to an older message must be swept in without --full")
}

func TestMaintenanceRescanIsExplicit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// Edit the NEWEST archived message: archives ignore post-capture
	// mutations by default, so plain incremental runs must not see it.
	// The explicit --maintenance rescan repairs it (its inclusive upper
	// bound covers the cursor message itself).
	f.mu.Lock()
	last := len(f.conv("C01").Msgs) - 1
	f.conv("C01").Msgs[last].Text = "hello 7 (stealth edit)"
	f.mu.Unlock()

	_, err = imp.Import(context.Background(), opts)
	require.NoError(err)
	var body string
	readBody := func() string {
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT mb.body_text FROM message_bodies mb
			JOIN messages m ON m.id = mb.message_id WHERE m.source_message_id = ?`), "C01:"+ts(7)).Scan(&body))
		return body
	}
	assert.Equal("hello 7", readBody(), "plain runs ignore post-capture edits")

	maint := opts
	maint.Maintenance = true
	_, err = imp.Import(context.Background(), maint)
	require.NoError(err)
	assert.Equal("hello 7 (stealth edit)", readBody(), "--maintenance repairs edits")
}

func TestImportGoneConversationIsSkippedNotFatal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	// Enumerated but unreadable: history answers channel_not_found (observed
	// live with a sandbox provisioning-bot DM). The fake 404s any channel it
	// has no record of, so listing a ghost entry reproduces it exactly.
	f.convs = append(f.convs, &fakeConv{ID: "D_GONE", Kind: "im", IMUser: "UALICE"})
	ghost := f.convs[len(f.convs)-1]
	f.handleGhost(ghost)
	imp, opts := testImporter(t, f)
	st := imp.store

	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err, "a permanently-gone conversation must not fail the run")
	assert.Zero(sum.FetchErrors)

	// Everything else archived normally.
	var total int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	assert.Equal(totalWorkspaceMessages, total)
}

func TestImportChannelFilters(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	opts.ExcludeChannels = []string{"general"}
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	var n int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM conversations WHERE source_conversation_id = ?`), "C01").Scan(&n))
	assert.Zero(n, "excluded channel must not be archived")
	// DMs are never filtered.
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM conversations WHERE source_conversation_id = ?`), "D01").Scan(&n))
	assert.Equal(1, n)
}
