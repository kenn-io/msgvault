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
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	sum, err := imp.Import(context.Background(), opts)
	require.NoError(t, err)
	assert.Equal(t, 4, sum.ConversationsProcessed)
	assert.Equal(t, 2, sum.RepliesFetched)
	assert.Zero(t, sum.FetchErrors)

	var msgCount int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&msgCount))
	assert.Equal(t, totalWorkspaceMessages, msgCount)

	// Conversation types and titles.
	var title, convType string
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT title, conversation_type FROM conversations WHERE source_conversation_id = ?`), "C01").
		Scan(&title, &convType))
	assert.Equal(t, "#general", title)
	assert.Equal(t, "channel", convType)
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT title, conversation_type FROM conversations WHERE source_conversation_id = ?`), "D01").
		Scan(&title, &convType))
	assert.Equal(t, "Alice", title)
	assert.Equal(t, "direct_chat", convType)
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT title, conversation_type FROM conversations WHERE source_conversation_id = ?`), "C02").
		Scan(&title, &convType))
	assert.Equal(t, "#secrets", title, "private channels archive like channels")
	assert.Equal(t, "channel", convType)
	var privateMsgs int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE c.source_conversation_id = ?`), "C02").Scan(&privateMsgs))
	assert.Equal(t, 1, privateMsgs)

	// Email-based identity: Alice deduped against mail archives by address.
	var aliceID int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT id FROM participants WHERE email_address = ?`), "alice@example.com").Scan(&aliceID))

	// Thread replies linked to their root.
	var linked int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM messages child
		JOIN messages parent ON parent.id = child.reply_to_message_id
		WHERE child.source_message_id = ? AND parent.source_message_id = ?`),
		"C01:"+ts(100), "C01:"+ts(5)).Scan(&linked))
	assert.Equal(t, 1, linked)

	// Reactions: two users on message 2.
	var reactions int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM reactions r
		JOIN messages m ON m.id = r.message_id
		WHERE m.source_message_id = ? AND r.reaction_value = 'thumbsup'`), "C01:"+ts(2)).Scan(&reactions))
	assert.Equal(t, 2, reactions)

	// Mention row for <@UME>, with mrkdwn rendered in the body.
	var mentions int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_recipients mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_message_id = ? AND mr.recipient_type = 'mention'`), "C01:"+ts(1)).Scan(&mentions))
	assert.Equal(t, 1, mentions)
	var body string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT mb.body_text FROM message_bodies mb
		JOIN messages m ON m.id = mb.message_id
		WHERE m.source_message_id = ?`), "C01:"+ts(1)).Scan(&body))
	assert.Equal(t, "ping @Me see the docs (https://example.com)", body)

	// Edited flag, bot sender, raw archive format.
	var edited bool
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT is_edited FROM messages WHERE source_message_id = ?`), "C01:"+ts(3)).Scan(&edited))
	assert.True(t, edited)
	var botSender string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT p.display_name FROM messages m JOIN participants p ON p.id = m.sender_id
		WHERE m.source_message_id = ?`), "C01:"+ts(4)).Scan(&botSender))
	assert.Equal(t, "deploybot", botSender)
	// The bot message's content lives in a legacy attachment (empty text);
	// its fallback must be the searchable body.
	var botBody string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT mb.body_text FROM message_bodies mb
		JOIN messages m ON m.id = mb.message_id WHERE m.source_message_id = ?`), "C01:"+ts(4)).Scan(&botBody))
	assert.Equal(t, "Build #42 failed on main", botBody)
	var rawFormat string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT mr.raw_format FROM message_raw mr JOIN messages m ON m.id = mr.message_id
		WHERE m.source_message_id = ?`), "C01:"+ts(0)).Scan(&rawFormat))
	assert.Equal(t, "slack_json", rawFormat)

	// Membership recorded.
	var members int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM conversation_participants cp
		JOIN conversations c ON c.id = cp.conversation_id
		WHERE c.source_conversation_id = ?`), "C01").Scan(&members))
	assert.Equal(t, 3, members)
}

func TestImportIncrementalCatchesNewMessagesAndLateReplies(t *testing.T) {
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(t, err)

	// A new top-level message and a late reply to the (old) thread root.
	f.mu.Lock()
	general := f.conv("C01")
	general.Msgs = append(general.Msgs, fakeMsg{TS: ts(200), User: "UBOB", Text: "fresh news"})
	lateReply := tsFresh(0)
	root := general.findRoot(ts(5))
	root.Replies = append(root.Replies, fakeMsg{TS: lateReply, ThreadTS: root.TS, User: "UALICE", Text: "late reply"})
	f.mu.Unlock()

	sum, err := imp.Import(context.Background(), opts)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.RepliesFetched, "only the late reply is new; earlier replies are behind the thread cursor")

	for _, id := range []string{"C01:" + ts(200), "C01:" + lateReply} {
		var n int
		require.NoError(t, st.DB().QueryRow(st.Rebind(
			`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), id).Scan(&n))
		assert.Equal(t, 1, n, id)
	}
}

func TestImportIncrementalMidWindowFailureDoesNotAdvanceCursor(t *testing.T) {
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(t, err)

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
	require.Error(t, err, "a run with fetch errors must not report success")

	// The cursor must not have advanced past the unfetched older page: after
	// healing, ALL five burst messages are archived exactly once.
	f.mu.Lock()
	f.failHistoryContinuations = false
	f.mu.Unlock()
	_, err = imp.Import(context.Background(), opts)
	require.NoError(t, err)
	for i := range 5 {
		var n int
		require.NoError(t, st.DB().QueryRow(st.Rebind(
			`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C01:"+ts(300+i)).Scan(&n))
		assert.Equal(t, 1, n, "burst message %d", i)
	}
}

func TestBackfillThreadFetchFailureLeavesPageResumable(t *testing.T) {
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	// Backfill fetches each root's replies INLINE before advancing past the
	// containing page: a reply-fetch failure must leave the page (and its
	// threads) to be retried, and the run must not report success.
	f.failReplies[ts(5)] = true
	sum, err := imp.Import(context.Background(), opts)
	require.Error(t, err, "a run with fetch errors must not report success")
	assert.Positive(t, sum.FetchErrors)

	// The replies never landed.
	var n int
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C01:"+ts(100)).Scan(&n))
	assert.Zero(t, n)

	// Healed: the resumed backfill refetches the page and its threads.
	f.mu.Lock()
	delete(f.failReplies, ts(5))
	f.mu.Unlock()
	sum, err = imp.Import(context.Background(), opts)
	require.NoError(t, err)
	assert.Equal(t, 2, sum.RepliesFetched)
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C01:"+ts(100)).Scan(&n))
	assert.Equal(t, 1, n)
	var total, distinct int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(DISTINCT source_message_id) FROM messages WHERE message_type='slack'`).Scan(&distinct))
	assert.Equal(t, distinct, total, "page refetch after thread failure must not duplicate")
}

func TestImportHistoryFailureLeavesConversationResumable(t *testing.T) {
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	f.failHistory["C01"] = true
	_, err := imp.Import(context.Background(), opts)
	require.Error(t, err)

	// The healthy conversations still synced.
	var n int
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "D01:"+ts(20)).Scan(&n))
	assert.Equal(t, 1, n)

	f.mu.Lock()
	delete(f.failHistory, "C01")
	f.mu.Unlock()
	_, err = imp.Import(context.Background(), opts)
	require.NoError(t, err)
	var total int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	assert.Equal(t, totalWorkspaceMessages, total)
}

func TestImportInterruptResumesWithoutDuplicates(t *testing.T) {
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
	require.NoError(t, err)
	var total, distinct int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(DISTINCT source_message_id) FROM messages WHERE message_type='slack'`).Scan(&distinct))
	assert.Equal(t, totalWorkspaceMessages, total)
	assert.Equal(t, distinct, total)
}

func TestImportFullReUpsertsInPlace(t *testing.T) {
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(t, err)

	// An old message is edited at the source; only --full re-walks it.
	f.mu.Lock()
	f.conv("C01").Msgs[0].Text = "hello 0 (edited)"
	f.mu.Unlock()

	full := opts
	full.Full = true
	_, err = imp.Import(context.Background(), full)
	require.NoError(t, err)

	var body string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT mb.body_text FROM message_bodies mb
		JOIN messages m ON m.id = mb.message_id WHERE m.source_message_id = ?`), "C01:"+ts(0)).Scan(&body))
	assert.Equal(t, "hello 0 (edited)", body)
	var total int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	assert.Equal(t, totalWorkspaceMessages, total, "full run must upsert, not duplicate")
}

func TestImportLimitLeavesBackfillResumable(t *testing.T) {
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
	require.NoError(t, err)
	var partial int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&partial))
	assert.Less(t, partial, totalWorkspaceMessages)

	// An uncapped run completes the backfill: every message exactly once.
	_, err = imp.Import(context.Background(), opts)
	require.NoError(t, err)
	var total, distinct int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(DISTINCT source_message_id) FROM messages WHERE message_type='slack'`).Scan(&distinct))
	assert.Equal(t, totalWorkspaceMessages, total, "limited first run must not lose messages")
	assert.Equal(t, distinct, total)
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
	f, rootTS := oldThreadWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(t, err)

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
	require.NoError(t, err)
	var linked int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM messages child
		JOIN messages parent ON parent.id = child.reply_to_message_id
		WHERE child.source_message_id = ? AND parent.source_message_id = ?`),
		"C09:"+lateReply, "C09:"+rootTS).Scan(&linked))
	assert.Equal(t, 1, linked, "the sweep must archive and link a late reply to an ancient root")
}

func TestSweepWatermarkHoldsOnCanonicalFetchFailure(t *testing.T) {
	f, rootTS := oldThreadWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(t, err)

	lateReply := tsFresh(0)
	f.mu.Lock()
	root := f.conv("C09").findRoot(rootTS)
	root.Replies = append(root.Replies, fakeMsg{TS: lateReply, ThreadTS: rootTS, User: "UME", Text: "late reply"})
	// The canonical fetch is anchored at the discovered hit's ts.
	f.failReplies[lateReply] = true
	f.mu.Unlock()

	sum, err := imp.Import(context.Background(), opts)
	require.Error(t, err, "a sweep with a failed canonical fetch must not report success")
	assert.Positive(t, sum.FetchErrors)
	var n int
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C09:"+lateReply).Scan(&n))
	require.Zero(t, n)

	// Healed: the watermark parked before the failed hit, so the next sweep
	// re-discovers and archives it — exactly once.
	f.mu.Lock()
	delete(f.failReplies, lateReply)
	f.mu.Unlock()
	_, err = imp.Import(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C09:"+lateReply).Scan(&n))
	assert.Equal(t, 1, n)
	var total, distinct int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(DISTINCT source_message_id) FROM messages WHERE message_type='slack'`).Scan(&distinct))
	assert.Equal(t, distinct, total)
}

func TestSweepSkipsNotDoneConversations(t *testing.T) {
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
	require.Error(t, err) // C10's history failure keeps the run partial
	lateReply := tsFresh(0)
	f.mu.Lock()
	root := f.conv("C09").findRoot(rootTS)
	root.Replies = append(root.Replies, fakeMsg{TS: lateReply, ThreadTS: rootTS, User: "UME", Text: "late reply"})
	f.mu.Unlock()

	_, err = imp.Import(context.Background(), opts)
	require.Error(t, err) // still partial: C10 still failing
	var n int
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C09:"+lateReply).Scan(&n))
	assert.Equal(t, 1, n, "done conversations are swept even while others are stuck")
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C10:"+stuckReply).Scan(&n))
	assert.Zero(t, n, "not-done conversations are owned by backfill, never the sweep")

	// C10 heals: its backfill fetches root AND replies inline.
	f.mu.Lock()
	delete(f.failHistory, "C10")
	f.mu.Unlock()
	_, err = imp.Import(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C10:"+stuckReply).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestImportLimitBoundsProcessedMessages(t *testing.T) {
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	limited := opts
	limited.Limit = 1
	limited.NoThreads = true
	_, err := imp.Import(context.Background(), limited)
	require.NoError(t, err)

	// Page requests are sized to the remaining budget, so each conversation
	// processes at most its limit — not a full server page.
	var total int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	assert.LessOrEqual(t, total, 4, "--limit 1 must not fetch whole pages per conversation")
	assert.Positive(t, total)
}

func TestImportLimitedRunsDrainIncrementalBacklog(t *testing.T) {
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(t, err)

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
		require.NoError(t, err)
	}
	for i := range 9 {
		var n int
		require.NoError(t, st.DB().QueryRow(st.Rebind(
			`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C01:"+ts(400+i)).Scan(&n))
		assert.Equal(t, 1, n, "backlog message %d must be drained by repeated limited runs", i)
	}
}

func TestImportDiscoversFirstReplyToOlderMessage(t *testing.T) {
	f, rootTS := oldThreadWorkspace(t)
	// The 10-day-old message starts with NO replies: it is archived as a
	// plain message, not tracked as a thread root.
	f.mu.Lock()
	f.conv("C09").findRoot(rootTS).Replies = nil
	f.mu.Unlock()
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(t, err)

	// First reply arrives long after archiving. The reply sweep must
	// discover it by creation time — the parent's age is irrelevant.
	lateReply := tsFresh(0)
	f.mu.Lock()
	root := f.conv("C09").findRoot(rootTS)
	root.Replies = []fakeMsg{{TS: lateReply, ThreadTS: rootTS, User: "UME", Text: "first ever reply"}}
	f.mu.Unlock()

	_, err = imp.Import(context.Background(), opts)
	require.NoError(t, err)
	var n int
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C09:"+lateReply).Scan(&n))
	assert.Equal(t, 1, n, "a first reply to an older message must be swept in without --full")
}

func TestMaintenanceRescanIsExplicit(t *testing.T) {
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	_, err := imp.Import(context.Background(), opts)
	require.NoError(t, err)

	// Edit the NEWEST archived message: archives ignore post-capture
	// mutations by default, so plain incremental runs must not see it.
	// The explicit --maintenance rescan repairs it (its inclusive upper
	// bound covers the cursor message itself).
	f.mu.Lock()
	last := len(f.conv("C01").Msgs) - 1
	f.conv("C01").Msgs[last].Text = "hello 7 (stealth edit)"
	f.mu.Unlock()

	_, err = imp.Import(context.Background(), opts)
	require.NoError(t, err)
	var body string
	readBody := func() string {
		require.NoError(t, st.DB().QueryRow(st.Rebind(`
			SELECT mb.body_text FROM message_bodies mb
			JOIN messages m ON m.id = mb.message_id WHERE m.source_message_id = ?`), "C01:"+ts(7)).Scan(&body))
		return body
	}
	assert.Equal(t, "hello 7", readBody(), "plain runs ignore post-capture edits")

	maint := opts
	maint.Maintenance = true
	_, err = imp.Import(context.Background(), maint)
	require.NoError(t, err)
	assert.Equal(t, "hello 7 (stealth edit)", readBody(), "--maintenance repairs edits")
}

func TestImportGoneConversationIsSkippedNotFatal(t *testing.T) {
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
	require.NoError(t, err, "a permanently-gone conversation must not fail the run")
	assert.Zero(t, sum.FetchErrors)

	// Everything else archived normally.
	var total int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&total))
	assert.Equal(t, totalWorkspaceMessages, total)
}

func TestImportChannelFilters(t *testing.T) {
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	opts.ExcludeChannels = []string{"general"}
	_, err := imp.Import(context.Background(), opts)
	require.NoError(t, err)

	var n int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM conversations WHERE source_conversation_id = ?`), "C01").Scan(&n))
	assert.Zero(t, n, "excluded channel must not be archived")
	// DMs are never filtered.
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM conversations WHERE source_conversation_id = ?`), "D01").Scan(&n))
	assert.Equal(t, 1, n)
}
