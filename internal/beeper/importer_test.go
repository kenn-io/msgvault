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

type recordingEnqueuer struct {
	ids []int64
}

func (e *recordingEnqueuer) EnqueueMessages(_ context.Context, ids []int64) error {
	e.ids = append(e.ids, ids...)
	return nil
}

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
	enq := &recordingEnqueuer{}
	imp, st, done := newTestImporter(t, f)
	defer done()

	sum, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal", EmbedEnqueuer: enq})
	require.NoError(err)

	assert.EqualValues(1, sum.ChatsProcessed)
	assert.EqualValues(44, sum.MessagesProcessed, "43 persisted + 1 deletion tombstone")
	assert.EqualValues(43, sum.MessagesAdded)
	assert.EqualValues(1, sum.HiddenSkipped)
	assert.EqualValues(0, sum.Errors)
	assert.Len(enq.ids, 43)

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
	var ftsHits int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'tomorrow'`).Scan(&ftsHits))
	assert.Equal(1, ftsHits)

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
	require.NotNil(state.Anchor)
	assert.NotEmpty(state.ListWatermark)

	// Conversation stats recomputed.
	var statCount int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT message_count FROM conversations WHERE id = ?`), convID).Scan(&statCount))
	assert.Equal(43, statCount)
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
		ch.Msgs = append(ch.Msgs, fakeMsg{
			ID: "b" + strconv.Itoa(i), SortKey: i,
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Text:      "bulk " + strconv.Itoa(i),
			SenderID:  "@signal_ann:beeper.local", SenderName: "Ann",
		})
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

func TestImportAnchorMismatchFailsFast(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(e2eChat())
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	require.NotNil(state.Anchor)

	// Simulate a reinstall/re-index: the anchor message ID now maps to a
	// different message (timestamp changed).
	ch := f.chat("!e2e:beeper.local")
	for i := range ch.Msgs {
		if ch.Msgs[i].ID == state.Anchor.MessageID {
			ch.Msgs[i].Timestamp = ch.Msgs[i].Timestamp.Add(time.Hour)
		}
	}

	var before int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='beeper'`).Scan(&before))

	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.Error(err)
	assert.Contains(err.Error(), "re-assigned")
	assert.Contains(err.Error(), "re-add")

	var after int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='beeper'`).Scan(&after))
	assert.Equal(before, after, "no rows may be written after an anchor mismatch")

	// The failure is recorded on the sync run.
	var status string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT status FROM sync_runs WHERE source_id = ? ORDER BY id DESC LIMIT 1`), src.ID).Scan(&status))
	assert.Equal("failed", status)
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

func TestImportScopedToSingleChat(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(e2eChat())
	base := time.Now().Add(-10 * 24 * time.Hour).UTC().Truncate(time.Second)
	f.addChat(&fakeChat{
		ID: "!other:beeper.local", AccountID: "signal", Network: "Signal", Title: "Other", Type: "single",
		Participants: []map[string]any{{"id": "@me:beeper.local", "isSelf": true}},
		Msgs: []fakeMsg{{ID: "o0", SortKey: 0, Timestamp: base, Text: "other",
			SenderID: "@signal_zed:beeper.local", SenderName: "Zed"}},
		LastActivity: base,
	})
	imp, st, done := newTestImporter(t, f)
	defer done()

	sum, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal", ChatID: "!other:beeper.local"})
	require.NoError(err)
	assert.EqualValues(1, sum.ChatsProcessed)

	var total int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='beeper'`).Scan(&total))
	assert.Equal(1, total)
}
