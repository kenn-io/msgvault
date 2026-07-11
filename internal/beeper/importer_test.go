package beeper

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// e2eChat builds a chat exercising every persist path: replies, reactions,
// deletions, hidden events, edits, transcriptions, and mentions. Timestamps
// are older than the reconcile window so head re-walks terminate immediately.
func e2eChat() *fakeChat {
	base := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	ch := &fakeChat{
		ID: "!e2e:beeper.local", AccountID: "signal", Network: "Signal",
		Title: "E2E", Type: "group",
		Participants: []map[string]any{
			{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
			{"id": "@15550100010:beeper.local", "fullName": "Alice", "phoneNumber": "+15550100010", "isAdmin": true},
			{"id": "@signal_bob:beeper.local", "fullName": "Bob", "email": "bob@example.com"},
		},
	}
	for i := range 45 {
		m := fakeMsg{
			ID: "m" + strconv.Itoa(i), SortKey: i,
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Text:      "hello " + strconv.Itoa(i),
			SenderID:  "@15550100010:beeper.local", SenderName: "Alice",
		}
		switch i {
		case 10:
			m.Reactions = []map[string]any{
				{"id": "@signal_bob:beeper.local", "reactionKey": "👍", "participantID": "@signal_bob:beeper.local", "emoji": true},
			}
		case 20:
			m.LinkedMessageID = "m5"
		case 30:
			m.IsDeleted = true
		case 31:
			m.IsHidden = true
		case 40:
			edited := m.Timestamp.Add(time.Hour)
			m.EditedTimestamp = &edited
		case 41:
			m.Type = "VOICE"
			m.Text = ""
			m.Attachments = []map[string]any{
				{"id": "mxc://x/voice41", "type": "audio", "isVoiceNote": true,
					"transcription": map[string]any{"transcription": "call me back tomorrow", "engine": "test"}},
			}
		case 42:
			m.Mentions = []string{"@signal_bob:beeper.local", "@room"}
		}
		ch.Msgs = append(ch.Msgs, m)
	}
	ch.LastActivity = ch.Msgs[len(ch.Msgs)-1].Timestamp
	return ch
}

func newTestImporter(t *testing.T, f *fakeBeeper) (*Importer, *store.Store, func()) {
	t.Helper()
	srv := f.server()
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, NewClient(srv.URL, testToken, 10000))
	return imp, st, srv.Close
}

func TestImportBackfillEndToEnd(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(e2eChat())
	imp, st, done := newTestImporter(t, f)
	defer done()

	sum, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	assert.EqualValues(1, sum.ChatsProcessed)
	assert.EqualValues(44, sum.MessagesProcessed, "43 persisted + 1 deletion tombstone; hidden events are not counted")
	assert.EqualValues(43, sum.MessagesAdded)
	assert.EqualValues(0, sum.Errors)

	var msgCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='beeper'`).Scan(&msgCount))
	assert.Equal(43, msgCount)

	// Conversation and its full participant list (admin role preserved).
	var convID int64
	var convType string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT id, conversation_type FROM conversations WHERE source_conversation_id = ?`,
	), "!e2e:beeper.local").Scan(&convID, &convType))
	assert.Equal("group_chat", convType)
	var partCount, adminCount int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = ?`), convID).Scan(&partCount))
	assert.Equal(3, partCount)
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = ? AND role='admin'`), convID).Scan(&adminCount))
	assert.Equal(1, adminCount)

	// Phone participant deduped by E.164, sender attribution set.
	var alicePID int64
	require.NoError(st.DB().QueryRow(
		`SELECT id FROM participants WHERE phone_number = '+15550100010'`).Scan(&alicePID))
	var senderCount int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE sender_id = ? AND message_type='beeper'`), alicePID).Scan(&senderCount))
	assert.Equal(43, senderCount)

	// Reply linked to its parent.
	var parentID int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT id FROM messages WHERE source_message_id = ?`), "m5").Scan(&parentID))
	var linkedParent sql.NullInt64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT reply_to_message_id FROM messages WHERE source_message_id = ?`), "m20").Scan(&linkedParent))
	require.True(linkedParent.Valid)
	assert.Equal(parentID, linkedParent.Int64)

	// Embedded reaction persisted.
	var reactionValue string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT r.reaction_value FROM reactions r
		JOIN messages m ON m.id = r.message_id
		WHERE m.source_message_id = ?`), "m10").Scan(&reactionValue))
	assert.Equal("👍", reactionValue)

	// Edit flag set.
	var isEdited bool
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT is_edited FROM messages WHERE source_message_id = ?`), "m40").Scan(&isEdited))
	assert.True(isEdited)

	// Voice transcription is searchable (body + FTS).
	var voiceBody string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT b.body_text FROM message_bodies b
		JOIN messages m ON m.id = b.message_id
		WHERE m.source_message_id = ?`), "m41").Scan(&voiceBody))
	assert.Contains(voiceBody, "[voice message]")
	assert.Contains(voiceBody, "call me back tomorrow")

	// Mentions become mention recipient rows; @room is skipped; no from/to rows.
	var mentionCount, fromToCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_recipients mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_message_id = ? AND mr.recipient_type = 'mention'`), "m42").Scan(&mentionCount))
	assert.Equal(1, mentionCount)
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*) FROM message_recipients mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.message_type = 'beeper' AND mr.recipient_type IN ('from','to')`).Scan(&fromToCount))
	assert.Zero(fromToCount)

	// Raw JSON archived with the beeper_json format tag.
	var rawFormat string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mr.raw_format FROM message_raw mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_message_id = ?`), "m10").Scan(&rawFormat))
	assert.Equal("beeper_json", rawFormat)

	// Sync state persisted: backfill complete, incremental cursor primed, anchor set.
	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	require.True(run.CursorAfter.Valid)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	cs := state.Chats["!e2e:beeper.local"]
	require.NotNil(cs)
	assert.True(cs.Done)
	assert.NotEmpty(cs.Newest)
	require.NotEmpty(state.Anchors)
	assert.NotEmpty(state.ListWatermark)

	// Conversation stats recomputed.
	var statCount int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT message_count FROM conversations WHERE id = ?`), convID).Scan(&statCount))
	assert.Equal(43, statCount)

	// The completed run persists its final counters for sync diagnostics
	// (mid-run checkpoints are throttled and may never have fired).
	var processed, added int64
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT messages_processed, messages_added FROM sync_runs
		WHERE source_id = ? AND status = 'completed' ORDER BY id DESC LIMIT 1`), src.ID).
		Scan(&processed, &added))
	assert.EqualValues(44, processed)
	assert.EqualValues(43, added)
}

func TestImportSecondRunIncremental(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(e2eChat())
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	// Three new messages arrive.
	now := time.Now().UTC().Truncate(time.Second)
	for i := range 3 {
		f.appendMsg("!e2e:beeper.local", fakeMsg{
			ID: "new" + strconv.Itoa(i), SortKey: 100 + i,
			Timestamp: now.Add(time.Duration(i-3) * time.Minute),
			Text:      "fresh " + strconv.Itoa(i),
			SenderID:  "@signal_bob:beeper.local", SenderName: "Bob",
		})
	}

	f.resetRequests()
	sum, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	// The reconcile pass may re-upsert recent messages, so MessagesAdded can
	// exceed 3; the DB count is the duplicate-free ground truth.
	assert.GreaterOrEqual(sum.MessagesAdded, int64(3))

	var total int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='beeper'`).Scan(&total))
	assert.Equal(46, total, "no duplicates on re-run")

	// The second run must be cursor-driven: chat discovery filtered by
	// activity, message fetches either direction=after (incremental) or the
	// cursor-less head page (reconcile) — never a direction=before backfill.
	var sawActivityFilter bool
	for _, req := range f.requests() {
		if strings.HasPrefix(req, "/v1/chats/search") {
			sawActivityFilter = sawActivityFilter || strings.Contains(req, "lastActivityAfter=")
		}
		if strings.Contains(req, "/messages?") {
			assert.NotContains(req, "direction=before", "second run must not re-backfill: %s", req)
		}
	}
	assert.True(sawActivityFilter, "second run must filter chat discovery by lastActivityAfter")
}

func TestImportLimitIsSharedAcrossIncrementalAndReconcile(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(e2eChat())
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	now := time.Now().UTC().Truncate(time.Second)
	for i := range 45 {
		f.appendMsg("!e2e:beeper.local", fakeMsg{
			ID: "limited-new-" + strconv.Itoa(i), SortKey: 100 + i,
			Timestamp: now.Add(time.Duration(i) * time.Second), Text: "limited incremental",
			SenderID: "@signal_bob:beeper.local", SenderName: "Bob",
		})
	}

	f.resetRequests()
	sum, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal", Limit: 20})
	require.NoError(err)
	assert.EqualValues(20, sum.MessagesProcessed,
		"one per-chat budget must cover incremental and reconciliation work")

	messageLists := 0
	for _, req := range f.requests() {
		if strings.Contains(req, "/messages?") && !strings.Contains(req, "/messages/") {
			messageLists++
			assert.Contains(req, "direction=after", "the limited run must stop before reconciliation: %s", req)
		}
	}
	assert.Equal(1, messageLists, "the limit must stop before fetching another page")

	var limitedTotal int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type = 'beeper'`).Scan(&limitedTotal))
	assert.Equal(63, limitedTotal, "the first incremental page is archived without skipping later pages")

	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	var finalTotal int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type = 'beeper'`).Scan(&finalTotal))
	assert.Equal(88, finalTotal, "the next run must resume the pages left by the limit")
}

func TestImportResumeAfterLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	base := time.Now().Add(-60 * 24 * time.Hour).UTC().Truncate(time.Second)
	ch := &fakeChat{
		ID: "!big:beeper.local", AccountID: "signal", Network: "Signal", Title: "Big", Type: "single",
		Participants: []map[string]any{
			{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
			{"id": "@signal_ann:beeper.local", "fullName": "Ann"},
		},
	}
	for i := range 100 {
		m := fakeMsg{
			ID: "b" + strconv.Itoa(i), SortKey: i,
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Text:      "bulk " + strconv.Itoa(i),
			SenderID:  "@signal_ann:beeper.local", SenderName: "Ann",
		}
		if i == 99 {
			m.LinkedMessageID = "b5" // parent only reached by the resumed run
		}
		ch.Msgs = append(ch.Msgs, m)
	}
	ch.LastActivity = ch.Msgs[len(ch.Msgs)-1].Timestamp
	f.addChat(ch)
	imp, st, done := newTestImporter(t, f)
	defer done()

	// Run 1: limited — backfill stops early but stays resumable.
	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal", Limit: 30})
	require.NoError(err)

	var afterRun1 int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='beeper'`).Scan(&afterRun1))
	require.Less(afterRun1, 100)

	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	require.NotNil(state.Chats["!big:beeper.local"])
	assert.False(state.Chats["!big:beeper.local"].Done, "limited backfill must stay resumable")

	// Run 2: unlimited — resumes from the stored cursor without refetching.
	f.resetRequests()
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	var total int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='beeper'`).Scan(&total))
	assert.Equal(100, total)

	for _, req := range f.requests() {
		if strings.Contains(req, "/messages?") && strings.Contains(req, "direction=before") {
			assert.Contains(req, "cursor=", "resumed backfill must continue from the stored cursor: %s", req)
		}
	}

	// Chat now complete.
	run, err = st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	state, err = LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.True(state.Chats["!big:beeper.local"].Done)

	// The reply pair buffered in run 1 (parent beyond the limit) must have
	// survived the interruption and linked once the resumed run archived it.
	var parentID int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT id FROM messages WHERE source_message_id = ?`), "b5").Scan(&parentID))
	var linked sql.NullInt64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT reply_to_message_id FROM messages WHERE source_message_id = ?`), "b99").Scan(&linked))
	require.True(linked.Valid, "reply pairs must survive a resumed backfill")
	assert.Equal(parentID, linked.Int64)
}

func TestImportReactionEventRefreshesTarget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(e2eChat())
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	// Bob reacts to an old message: the network delivers a REACTION event and
	// the target's embedded reactions[] now include it.
	ch := f.chat("!e2e:beeper.local")
	ch.Msgs[5].Reactions = []map[string]any{
		{"id": "@signal_bob:beeper.local", "reactionKey": "❤️", "participantID": "@signal_bob:beeper.local", "emoji": true},
	}
	f.appendMsg("!e2e:beeper.local", fakeMsg{
		ID: "react1", SortKey: 200, Timestamp: time.Now().UTC().Truncate(time.Second),
		Type: "REACTION", IsHidden: true, LinkedMessageID: "m5",
		SenderID: "@signal_bob:beeper.local", SenderName: "Bob",
	})

	sum, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	assert.EqualValues(1, sum.ReactionsRefreshed)

	var reactionValue string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT r.reaction_value FROM reactions r
		JOIN messages m ON m.id = r.message_id
		WHERE m.source_message_id = ?`), "m5").Scan(&reactionValue))
	assert.Equal("❤️", reactionValue)

	// The REACTION event itself must not become a message row.
	var eventRows int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "react1").Scan(&eventRows))
	assert.Zero(eventRows)
}

func TestImportReconcileCatchesRecentDeletion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	// Recent chat: all messages inside the reconcile window.
	base := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	ch := &fakeChat{
		ID: "!recent:beeper.local", AccountID: "signal", Network: "Signal", Title: "Recent", Type: "single",
		Participants: []map[string]any{
			{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
			{"id": "@signal_ann:beeper.local", "fullName": "Ann"},
		},
	}
	for i := range 5 {
		ch.Msgs = append(ch.Msgs, fakeMsg{
			ID: "r" + strconv.Itoa(i), SortKey: i,
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Text:      "recent " + strconv.Itoa(i),
			SenderID:  "@signal_ann:beeper.local", SenderName: "Ann",
		})
	}
	ch.LastActivity = ch.Msgs[len(ch.Msgs)-1].Timestamp
	f.addChat(ch)
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	// The sender deletes r2 in place; the chat sees new activity so the next
	// run visits it and the reconcile pass observes the tombstone.
	f.chat("!recent:beeper.local").Msgs[2].IsDeleted = true
	f.appendMsg("!recent:beeper.local", fakeMsg{
		ID: "r5", SortKey: 5, Timestamp: time.Now().UTC().Truncate(time.Second),
		Text: "newest", SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
	})

	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	var deletedAt sql.NullTime
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT deleted_from_source_at FROM messages WHERE source_message_id = ?`), "r2").Scan(&deletedAt))
	assert.True(deletedAt.Valid, "reconcile pass must tombstone recent deletions")

	// The archived content survives deletion — that is the point of the archive.
	var body string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT b.body_text FROM message_bodies b
		JOIN messages m ON m.id = b.message_id
		WHERE m.source_message_id = ?`), "r2").Scan(&body))
	assert.Equal("recent 2", body)
}

func TestImportReconcileVisitsRecentChatWithoutNewActivity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	now := time.Now().UTC().Truncate(time.Second)
	participants := []map[string]any{
		{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
		{"id": "@signal_ann:beeper.local", "fullName": "Ann"},
	}
	f.addChat(&fakeChat{
		ID: "!watermark:beeper.local", AccountID: "signal", Network: "Signal", Title: "Watermark", Type: "single",
		Participants: participants,
		Msgs: []fakeMsg{{
			ID: "w0", SortKey: 0, Timestamp: now, Text: "sets watermark",
			SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
		}},
		LastActivity: now,
	})
	recentActivity := now.Add(-12 * time.Hour)
	f.addChat(&fakeChat{
		ID: "!recent-idle:beeper.local", AccountID: "signal", Network: "Signal", Title: "Recent Idle", Type: "single",
		Participants: participants,
		Msgs: []fakeMsg{{
			ID: "ri0", SortKey: 0, Timestamp: recentActivity, Text: "deleted in place",
			SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
		}},
		LastActivity: recentActivity,
	})
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	// The recent chat changes in place without advancing LastActivity. It is
	// older than the one-hour watermark overlap but still inside the promised
	// reconciliation window, so the next run must enumerate and revisit it.
	f.chat("!recent-idle:beeper.local").Msgs[0].IsDeleted = true
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	var deletedAt sql.NullTime
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT deleted_from_source_at FROM messages WHERE source_message_id = ?`), "ri0").Scan(&deletedAt))
	assert.True(deletedAt.Valid, "recent inactive chat must be reconciled despite the newer global watermark")
}

func TestImportReactionTargetTransientErrorRetries(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(e2eChat())
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	// A reaction arrives for an old message, but fetching the target fails
	// transiently: the cursor must NOT advance past the event, or the
	// reaction would be lost forever (target is outside the reconcile window).
	ch := f.chat("!e2e:beeper.local")
	ch.Msgs[5].Reactions = []map[string]any{
		{"id": "@signal_bob:beeper.local", "reactionKey": "🔥", "participantID": "@signal_bob:beeper.local", "emoji": true},
	}
	f.appendMsg("!e2e:beeper.local", fakeMsg{
		ID: "react-t", SortKey: 300, Timestamp: time.Now().UTC().Truncate(time.Second),
		Type: "REACTION", IsHidden: true, LinkedMessageID: "m5",
		SenderID: "@signal_bob:beeper.local", SenderName: "Bob",
	})
	f.setMessageGetFailure("m5", true)

	sum, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.ErrorContains(err, "partial Beeper sync")
	assert.Positive(sum.FetchErrors, "transient target failure must be recorded as a fetch error")

	// Healed: the next run re-delivers the reaction event and archives it.
	f.setMessageGetFailure("m5", false)
	sum, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	assert.EqualValues(1, sum.ReactionsRefreshed)

	var reactionValue string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT r.reaction_value FROM reactions r
		JOIN messages m ON m.id = r.message_id
		WHERE m.source_message_id = ?`), "m5").Scan(&reactionValue))
	assert.Equal("🔥", reactionValue)
}

func TestImportTruncatedParticipantsFetchesDetail(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	base := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	ch := &fakeChat{
		ID: "!grp:beeper.local", AccountID: "signal", Network: "Signal", Title: "Grp", Type: "group",
		ParticipantsTruncated: true,
		Participants: []map[string]any{
			{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
			{"id": "@signal_ann:beeper.local", "fullName": "Ann"},
			{"id": "@signal_bea:beeper.local", "fullName": "Bea"},
		},
		Msgs: []fakeMsg{{
			ID: "g0", SortKey: 0, Timestamp: base, Text: "hi",
			SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
		}},
		LastActivity: base,
	}
	f.addChat(ch)
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	var convID int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT id FROM conversations WHERE source_conversation_id = ?`), "!grp:beeper.local").Scan(&convID))
	var partCount int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = ?`), convID).Scan(&partCount))
	assert.Equal(3, partCount, "truncated listing must trigger a full-detail fetch")
}

func TestImportFullReconcilesConversationParticipants(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	base := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	ch := &fakeChat{
		ID: "!membership:beeper.local", AccountID: "signal", Network: "Signal", Title: "Membership", Type: "group",
		Participants: []map[string]any{
			{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
			{"id": "@signal_ann:beeper.local", "fullName": "Ann"},
			{"id": "@signal_bea:beeper.local", "fullName": "Bea", "isAdmin": true},
		},
		Msgs: []fakeMsg{{
			ID: "membership-0", SortKey: 0, Timestamp: base, Text: "hello",
			SenderID: "@signal_bea:beeper.local", SenderName: "Bea",
		}},
		LastActivity: base,
	}
	f.addChat(ch)
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	// Bea leaves and Ann becomes an admin without any new message activity.
	// A full sync receives a complete membership snapshot and must make the
	// stored membership exactly match it.
	ch.Participants = []map[string]any{
		{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
		{"id": "@signal_ann:beeper.local", "fullName": "Ann", "isAdmin": true},
	}
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal", Full: true})
	require.NoError(err)

	var convID int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT id FROM conversations WHERE source_conversation_id = ?`), ch.ID).Scan(&convID))
	var memberCount, departedCount int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = ?`), convID).Scan(&memberCount))
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM conversation_participants cp
		JOIN participants p ON p.id = cp.participant_id
		WHERE cp.conversation_id = ? AND p.display_name = 'Bea'`), convID).Scan(&departedCount))
	assert.Equal(2, memberCount)
	assert.Zero(departedCount, "departed participants must be removed")

	var annRole string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT cp.role FROM conversation_participants cp
		JOIN participants p ON p.id = cp.participant_id
		WHERE cp.conversation_id = ? AND p.display_name = 'Ann'`), convID).Scan(&annRole))
	assert.Equal("admin", annRole, "role changes must replace the stored role")
}

func TestImportFullRepairsWithoutDuplicates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(e2eChat())
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal", Full: true})
	require.NoError(err)

	var total int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='beeper'`).Scan(&total))
	assert.Equal(43, total, "--full re-walk must upsert in place")
}

func TestImportEmptyChatPicksUpLaterMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(&fakeChat{
		ID: "!empty:beeper.local", AccountID: "signal", Network: "Signal", Title: "Empty", Type: "single",
		Participants: []map[string]any{{"id": "@me:beeper.local", "isSelf": true}},
		LastActivity: time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second),
	})
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	var total int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='beeper'`).Scan(&total))
	require.Zero(total)

	// The chat's first-ever message arrives after the empty backfill.
	f.appendMsg("!empty:beeper.local", fakeMsg{
		ID: "first", SortKey: 1, Timestamp: time.Now().UTC().Truncate(time.Second),
		Text: "hello at last", SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
	})

	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='beeper'`).Scan(&total))
	assert.Equal(1, total, "a chat that was empty at backfill must still pick up later messages")
}

func TestImportDegenerateTailPagination(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// The live API never reports hasMore=false walking backward; past the
	// oldest message it re-serves the tail under a synthetic decrementing
	// cursor. The backfill must still terminate, mark the chat done, and not
	// double-process the tail.
	f := newFakeBeeper(t)
	base := time.Now().Add(-60 * 24 * time.Hour).UTC().Truncate(time.Second)
	ch := &fakeChat{
		ID: "!tail:beeper.local", AccountID: "signal", Network: "Signal", Title: "Tail", Type: "single",
		TailCountdown: true,
		Participants: []map[string]any{
			{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
			{"id": "@signal_ann:beeper.local", "fullName": "Ann"},
		},
	}
	for i := range 45 {
		ch.Msgs = append(ch.Msgs, fakeMsg{
			ID: "t" + strconv.Itoa(i), SortKey: i,
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Text:      "tail " + strconv.Itoa(i),
			SenderID:  "@signal_ann:beeper.local", SenderName: "Ann",
		})
	}
	ch.LastActivity = ch.Msgs[len(ch.Msgs)-1].Timestamp
	f.addChat(ch)
	imp, st, done := newTestImporter(t, f)
	defer done()

	sum, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	assert.EqualValues(45, sum.MessagesProcessed, "tail re-serves must not be re-processed")
	assert.EqualValues(45, sum.MessagesAdded)

	var total int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='beeper'`).Scan(&total))
	assert.Equal(45, total)

	// Terminates promptly: 3 real pages + 1 degenerate page.
	msgRequests := 0
	for _, req := range f.requests() {
		if strings.Contains(req, "/messages") && !strings.Contains(req, "/messages/") {
			msgRequests++
		}
	}
	assert.LessOrEqual(msgRequests, 5, "degenerate tail must terminate the walk quickly")

	// The chat is complete despite hasMore never going false.
	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	require.NotNil(state.Chats["!tail:beeper.local"])
	assert.True(state.Chats["!tail:beeper.local"].Done)
}

func TestBackfillCheckpointCarriesPendingReplies(t *testing.T) {
	require := require.New(t)

	// A mid-chat checkpoint advances the resume cursor past pages whose reply
	// pairs are still only in memory; the checkpoint must persist those pairs
	// too, or a hard interruption loses the links permanently.
	oldInterval := checkpointMinInterval
	checkpointMinInterval = 0
	t.Cleanup(func() { checkpointMinInterval = oldInterval })

	f := newFakeBeeper(t)
	base := time.Now().Add(-90 * 24 * time.Hour).UTC().Truncate(time.Second)
	ch := &fakeChat{
		ID: "!ckpt:beeper.local", AccountID: "signal", Network: "Signal", Title: "Ckpt", Type: "single",
		Participants: []map[string]any{
			{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
			{"id": "@signal_ann:beeper.local", "fullName": "Ann"},
		},
	}
	// >25 pages so a mid-chat checkpoint fires; the newest message replies to
	// a parent near the beginning of history, far beyond the interrupt point.
	for i := range 600 {
		m := fakeMsg{
			ID: "c" + strconv.Itoa(i), SortKey: i,
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Text:      "ckpt " + strconv.Itoa(i),
			SenderID:  "@signal_ann:beeper.local", SenderName: "Ann",
		}
		if i == 599 {
			m.LinkedMessageID = "c5"
		}
		ch.Msgs = append(ch.Msgs, m)
	}
	ch.LastActivity = ch.Msgs[len(ch.Msgs)-1].Timestamp
	f.addChat(ch)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.cancelAfterPages = 27 // past the 25-page checkpoint, before history ends
	f.cancelFn = cancel

	imp, st, done := newTestImporter(t, f)
	defer done()

	// The interrupt can land on either side of a page delivery: as a
	// context-cancel (run fails; state lives in the run's checkpoint) or as a
	// fetch error (run completes; state lives in cursor_after). The invariant
	// under test holds for both: any persisted cursor that advanced past the
	// reply message must carry its pending pair.
	_, err := imp.Import(ctx, ImportOptions{AccountID: "signal"})
	src, serr := st.GetOrCreateSource("beeper", "signal")
	require.NoError(serr)
	var blob string
	if err != nil {
		require.ErrorIs(err, context.Canceled)
		cp, cerr := st.GetLatestCheckpointedSync(src.ID)
		require.NoError(cerr)
		require.True(cp.CursorBefore.Valid)
		blob = cp.CursorBefore.String
	} else {
		run, rerr := st.GetLastSuccessfulSync(src.ID)
		require.NoError(rerr)
		require.True(run.CursorAfter.Valid)
		blob = run.CursorAfter.String
	}
	state, err := LoadSyncState(blob)
	require.NoError(err)
	cs := state.Chats["!ckpt:beeper.local"]
	require.NotNil(cs)
	require.NotEmpty(cs.Oldest, "persisted state must carry the advanced cursor")
	require.False(cs.Done)
	require.Equal([][2]string{{"c599", "c5"}}, cs.PendingReplies,
		"persisted state must carry the reply pairs its cursor has walked past")

	// The resumed run completes the chat and links the pair.
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	var parentID int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT id FROM messages WHERE source_message_id = ?`), "c5").Scan(&parentID))
	var linked sql.NullInt64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT reply_to_message_id FROM messages WHERE source_message_id = ?`), "c599").Scan(&linked))
	require.True(linked.Valid)
	require.Equal(parentID, linked.Int64)
}

func TestImportUnfinishedChatGoneMarksDone(t *testing.T) {
	require := require.New(t)

	f := newFakeBeeper(t)
	f.addChat(e2eChat()) // 45 messages: a Limit run leaves it unfinished
	base := time.Now().Add(-20 * 24 * time.Hour).UTC().Truncate(time.Second)
	f.addChat(&fakeChat{
		ID: "!stays:beeper.local", AccountID: "signal", Network: "Signal", Title: "Stays", Type: "single",
		Participants: []map[string]any{{"id": "@me:beeper.local", "isSelf": true}},
		Msgs: []fakeMsg{{ID: "s0", SortKey: 0, Timestamp: base, Text: "hi",
			SenderID: "@signal_ann:beeper.local", SenderName: "Ann"}},
		LastActivity: base,
	})
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal", Limit: 10})
	require.NoError(err)

	// The unfinished chat disappears from Beeper (left/deleted) while the
	// other chat survives. Discovery must mark the gone chat Done — keeping
	// the archived rows — instead of erroring forever or pinning the
	// watermark.
	f.mu.Lock()
	f.chats = f.chats[1:] // drop !e2e, keep !stays
	f.mu.Unlock()
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	require.NotNil(state.Chats["!e2e:beeper.local"])
	require.True(state.Chats["!e2e:beeper.local"].Done, "a gone chat must stop being re-probed")

	// Archived rows survive.
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	require.Positive(count)
}

func TestImportIncrementalStuckCursorTerminates(t *testing.T) {
	require := require.New(t)

	f := newFakeBeeper(t)
	f.addChat(e2eChat())
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	var before int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&before))

	// The API misbehaves: direction=after re-serves the head page under a
	// non-advancing cursor. The incremental walk must terminate (mirroring
	// the backfill's degenerate-tail defense) without duplicating rows.
	f.chat("!e2e:beeper.local").StuckHead = true
	f.resetRequests()
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	var after int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&after))
	require.Equal(before, after, "re-served pages must not duplicate rows")
	require.Less(len(f.requests()), 70, "the walk must terminate promptly, not spin")
}

func TestImportWatermarkHeldOnFetchError(t *testing.T) {
	require := require.New(t)

	f := newFakeBeeper(t)
	f.addChat(e2eChat())
	base := time.Now().Add(-20 * 24 * time.Hour).UTC().Truncate(time.Second)
	f.addChat(&fakeChat{
		ID: "!other:beeper.local", AccountID: "signal", Network: "Signal", Title: "Other", Type: "single",
		Participants: []map[string]any{{"id": "@me:beeper.local", "isSelf": true}},
		Msgs: []fakeMsg{{ID: "o0", SortKey: 0, Timestamp: base, Text: "hi",
			SenderID: "@signal_ann:beeper.local", SenderName: "Ann"}},
		LastActivity: base,
	})
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	// New activity in both chats — the failing chat's message is missed this
	// run, and the healthy chat's much newer activity would (if the watermark
	// advanced) push the failing chat outside the discovery overlap forever.
	now := time.Now().UTC().Truncate(time.Second)
	f.appendMsg("!e2e:beeper.local", fakeMsg{ID: "b99", SortKey: 99, Timestamp: now.Add(-3 * time.Hour),
		Text: "missed me?", SenderID: "@signal_ann:beeper.local", SenderName: "Ann"})
	f.appendMsg("!other:beeper.local", fakeMsg{ID: "o1", SortKey: 1, Timestamp: now,
		Text: "recent", SenderID: "@signal_ann:beeper.local", SenderName: "Ann"})
	f.setMessageListFailure("!e2e:beeper.local", true)
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.ErrorContains(err, "partial Beeper sync")

	// The failure is reported only after the healthy chat is processed and
	// the resumable state is checkpointed. Monitoring sees a failed run while
	// successful work from the same attempt remains archived.
	var healthyCount int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = 'o1'`).Scan(&healthyCount))
	require.Equal(1, healthyCount, "healthy chats must continue after another chat fails")
	var status string
	var cursorBefore sql.NullString
	require.NoError(st.DB().QueryRow(`
		SELECT status, cursor_before FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status, &cursorBefore))
	require.Equal(store.SyncStatusFailed, status)
	require.True(cursorBefore.Valid, "partial progress must remain checkpointed for retry")

	// Healed: the held-back watermark keeps the chat discoverable and the
	// missed message is archived.
	f.setMessageListFailure("!e2e:beeper.local", false)
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = 'b99'`), src.ID).Scan(&count))
	require.Equal(1, count, "the missed message must be archived once the fetch heals")
}
