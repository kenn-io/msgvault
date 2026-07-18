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

// testWorkspace builds a fake workspace exercising every persist path:
// channels with threads, reactions, mentions, edits, bot messages, a group
// DM, and a 1:1 DM.
func testWorkspace(t *testing.T) *fakeSlack {
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
	general.Msgs[5].Replies = []fakeMsg{
		{TS: ts(100), ThreadTS: general.Msgs[5].TS, User: "UBOB", Text: "reply one"},
		{TS: ts(101), ThreadTS: general.Msgs[5].TS, User: "UME", Text: "reply two"},
	}
	f.convs = []*fakeConv{
		general,
		{ID: "G01", Name: "mpdm-me--alice--bob-1", Kind: "mpim",
			Members: []string{"UME", "UALICE", "UBOB"},
			Msgs:    []fakeMsg{{TS: ts(10), User: "UME", Text: "group hi"}}},
		{ID: "D01", Kind: "im", IMUser: "UALICE",
			Msgs: []fakeMsg{{TS: ts(20), User: "UALICE", Text: "dm hi"}}},
	}
	return f
}

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
	assert.Equal(t, 3, sum.ConversationsProcessed)
	assert.Equal(t, 2, sum.RepliesFetched)
	assert.Zero(t, sum.FetchErrors)

	// 8 channel + 1 mpim + 1 im top-level, 2 replies, root re-upserted in place.
	var msgCount int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='slack'`).Scan(&msgCount))
	assert.Equal(t, 12, msgCount)

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
	root := general.findRoot(ts(5))
	root.Replies = append(root.Replies, fakeMsg{TS: ts(201), ThreadTS: root.TS, User: "UALICE", Text: "late reply"})
	f.mu.Unlock()

	sum, err := imp.Import(context.Background(), opts)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.RepliesFetched, "only the late reply is new; earlier replies are behind the thread cursor")

	for _, id := range []string{"C01:" + ts(200), "C01:" + ts(201)} {
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

func TestImportRepliesFailureDoesNotAdvanceThreadCursor(t *testing.T) {
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	st := imp.store

	f.failReplies[ts(5)] = true
	sum, err := imp.Import(context.Background(), opts)
	require.Error(t, err, "a run with fetch errors must not report success")
	assert.Positive(t, sum.FetchErrors)

	// The replies never landed.
	var n int
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C01:"+ts(100)).Scan(&n))
	assert.Zero(t, n)

	// Healed: the next run retries the thread from its unadvanced cursor.
	f.mu.Lock()
	delete(f.failReplies, ts(5))
	f.mu.Unlock()
	sum, err = imp.Import(context.Background(), opts)
	require.NoError(t, err)
	assert.Equal(t, 2, sum.RepliesFetched)
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C01:"+ts(100)).Scan(&n))
	assert.Equal(t, 1, n)
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
	assert.Equal(t, 12, total)
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
	assert.Equal(t, 12, total)
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
	assert.Equal(t, 12, total, "full run must upsert, not duplicate")
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
	assert.Equal(t, 12, total)
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
