package line

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/line/linetest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const (
	ownerMID = "u0000000000000000000000000000000a"
	aliceMID = "u0000000000000000000000000000000b"
	bobMID   = "u0000000000000000000000000000000c"
	groupMID = "c0000000000000000000000000000000d"
	roomMID  = "r0000000000000000000000000000000e"
)

// sampleFixture covers every chat kind and every content kind the importer
// classifies.
func sampleFixture() linetest.Fixture {
	return linetest.Fixture{
		Users: []linetest.User{
			{MID: aliceMID, Name: "Alice Profile", CustomName: "Alice"},
			{MID: bobMID, Name: "Bob"},
		},
		Chats: []linetest.Chat{
			{MID: ownerMID, Type: 100, Messages: []linetest.Message{
				{ID: "900", ContentType: 0, Millis: 1700000000000, Text: "note to self"},
			}},
			{MID: aliceMID, Type: 0, Members: []string{aliceMID}, Messages: []linetest.Message{
				{ID: "100", ContentType: 0, Millis: 1700000001000, Sender: aliceMID, Text: "hello from alice"},
				{ID: "101", ContentType: 0, Millis: 1700000002000, Text: "hello back", Metadata: map[string]string{"replyOriginalMessageId": "100"}},
				{ID: "102", ContentType: 1, Millis: 1700000003000, Sender: aliceMID, Text: "[Photo]"},
				{ID: "103", ContentType: 7, Millis: 1700000004000, Text: "￼", Metadata: map[string]string{"STKID": "1", "STKPKGID": "2"}},
				{ID: "104", ContentType: 6, Millis: 1700000005000, Metadata: map[string]string{"TYPE": "A", "RESULT": "NORMAL", "DURATION": "12"}},
				{ID: "105", ContentType: 0, Millis: 1700000006000, Text: "Message unsent", Metadata: map[string]string{"systemMessageTypeName": "ChatMessageUnsent"}},
				{ID: "106", ContentType: 18, Millis: 1700000007000, Text: "Alice left", Metadata: map[string]string{"systemMessageTypeName": "ChatEvent"}},
				{ID: "107", ContentType: 101, Millis: 1700000008000, Metadata: map[string]string{"UNSENT": "1"}},
				{NoID: true, ContentType: 0, Millis: 1700000009000, Text: "never left the phone"},
				{ID: "108", ContentType: 17, Millis: 1700000010000, Sender: aliceMID, Text: "￼", Metadata: map[string]string{"ALT_TEXT": "rich card text", "MARKUP_JSON": "{}"}},
				{ID: "109", ContentType: 107, Millis: 1700000011000, Sender: aliceMID, Text: "see https://example.com"},
				{ID: "110", ContentType: 13, Millis: 1700000012000, Sender: aliceMID, Text: "￼", Metadata: map[string]string{"displayName": "Someone", "mid": bobMID}},
				{ID: "111", ContentType: 0, Millis: 1700000013000, Sender: aliceMID, Text: "first copy"},
				{ID: "111", ContentType: 0, Millis: 1700000014000, Sender: aliceMID, Text: "second copy of the same id"},
			}},
			{MID: groupMID, Type: 2, Name: "Book club", Members: []string{ownerMID, aliceMID, bobMID}, Messages: []linetest.Message{
				{ID: "200", ContentType: 0, Millis: 1700000020000, Sender: bobMID, Text: "group text from bob"},
				{ID: "201", ContentType: 0, Millis: 1700000021000, Text: "group text from me"},
			}},
			{MID: roomMID, Type: 1, Members: []string{aliceMID, bobMID}, Messages: []linetest.Message{
				{ID: "300", ContentType: 0, Millis: 1700000030000, Sender: aliceMID, Text: "room text"},
			}},
		},
	}
}

func count(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(query), args...).Scan(&n))
	return n
}

func TestImportSampleStore(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	dir := linetest.Write(t, t.TempDir(), sampleFixture())
	summary, err := Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	assert.Equal(ownerMID, summary.Owner)
	assert.True(summary.OwnerFromStore)
	assert.Equal(4, summary.ChatsProcessed)
	assert.Zero(summary.ChatsSkipped)
	assert.EqualValues(9, summary.MessagesAdded, "note, alice hello, reply, rich, link text, first copy, bob group, me group, room")
	assert.EqualValues(1, summary.MessagesSkipped, "the duplicate id")
	assert.EqualValues(1, summary.MediaSkipped)
	assert.EqualValues(1, summary.StickersSkipped)
	assert.EqualValues(1, summary.CallsSkipped)
	assert.EqualValues(2, summary.SystemSkipped, "the unsent notice and the chat event")
	assert.EqualValues(2, summary.UnsentSkipped, "the row without an id and the unsent row")
	assert.EqualValues(1, summary.UnsupportedSkipped, "the contact card")
	assert.Equal(3, summary.ParticipantsResolved)

	var body string
	assert.Equal(9, count(t, st, "SELECT count(*) FROM messages WHERE message_type = 'line'"))
	assert.Equal(3, count(t, st, "SELECT count(*) FROM messages WHERE is_from_me = TRUE"))
	assert.Equal(1, count(t, st, "SELECT count(*) FROM messages WHERE source_message_id = ?", aliceMID+":111"))
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT b.body_text FROM messages m JOIN message_bodies b ON b.message_id = m.id WHERE m.source_message_id = ?`), aliceMID+":111").Scan(&body))
	assert.Equal("first copy", body, "the first row with an id wins")

	var parent string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT p.source_message_id FROM messages m JOIN messages p ON p.id = m.reply_to_message_id WHERE m.source_message_id = ?"), aliceMID+":101").Scan(&parent))
	assert.Equal(aliceMID+":100", parent)

	var kind, title string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT conversation_type, COALESCE(title, '') FROM conversations WHERE source_conversation_id = ?"), groupMID).Scan(&kind, &title))
	assert.Equal("group_chat", kind)
	assert.Equal("Book club", title)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT conversation_type, COALESCE(title, '') FROM conversations WHERE source_conversation_id = ?"), aliceMID).Scan(&kind, &title))
	assert.Equal("direct_chat", kind)
	assert.Empty(title)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT conversation_type FROM conversations WHERE source_conversation_id = ?"), roomMID).Scan(&kind))
	assert.Equal("group_chat", kind)

	members := func(conv string) int {
		return count(t, st, `SELECT count(*) FROM conversation_participants cp JOIN conversations c ON c.id = cp.conversation_id WHERE c.source_conversation_id = ?`, conv)
	}
	assert.Equal(3, members(groupMID), "owner, alice and bob from the group roster")
	assert.Equal(3, members(roomMID), "owner plus the two room members")
	assert.Equal(2, members(aliceMID))
	assert.Equal(1, members(ownerMID))

	var name string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT p.display_name FROM participants p JOIN participant_identifiers pi ON pi.participant_id = p.id WHERE pi.identifier_type = 'line' AND pi.identifier_value = ?`), aliceMID).Scan(&name))
	assert.Equal("Alice", name, "the owner's nickname for a contact wins over the profile name")

	var sender string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT pi.identifier_value FROM participant_identifiers pi JOIN messages m ON m.sender_id = pi.participant_id WHERE m.source_message_id = ?`), groupMID+":200").Scan(&sender))
	assert.Equal(bobMID, sender)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT pi.identifier_value FROM participant_identifiers pi JOIN messages m ON m.sender_id = pi.participant_id WHERE m.source_message_id = ?`), groupMID+":201").Scan(&sender))
	assert.Equal(ownerMID, sender)
}

func TestImportIsIdempotent(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	dir := linetest.Write(t, t.TempDir(), sampleFixture())
	first, err := Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	second, err := Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	assert.Zero(second.MessagesAdded)
	assert.Equal(first.MessagesAdded+1, second.MessagesSkipped)
	assert.False(second.WasResumed, "a completed run leaves no chat to resume")
	assert.Equal(int(first.MessagesAdded), count(t, st, "SELECT count(*) FROM messages"))
	assert.Equal(1, count(t, st, "SELECT count(*) FROM sources"))
}

func TestImportOwnerFromFlagOrStore(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	f := sampleFixture()
	f.Chats = f.Chats[1:] // no Keep Memo chat
	dir := linetest.Write(t, t.TempDir(), f)
	_, err := Import(t.Context(), st, ImportOptions{Path: dir})
	require.ErrorIs(err, ErrNoOwner)
	assert.Zero(count(t, st, "SELECT count(*) FROM sources"))
	summary, err := Import(t.Context(), st, ImportOptions{Path: dir, Me: " myline.id "})
	require.NoError(err)
	assert.Equal("myline.id", summary.Owner)
	assert.False(summary.OwnerFromStore)
	var identifier string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT identifier FROM sources WHERE source_type = 'line'").Scan(&identifier))
	assert.Equal("myline.id", identifier)
	assert.Equal(1, count(t, st, `SELECT count(*) FROM participant_identifiers WHERE identifier_type = 'line' AND identifier_value = ?`, "myline.id"))
}

func TestImportLimitPausesAndResumes(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	dir := linetest.Write(t, t.TempDir(), sampleFixture())
	summary, err := Import(t.Context(), st, ImportOptions{Path: dir, Limit: 3})
	require.NoError(err)
	assert.True(summary.LimitReached)
	assert.EqualValues(3, summary.MessagesAdded)
	var status string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1").Scan(&status))
	assert.Equal("paused", status)

	summary, err = Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	assert.True(summary.WasResumed, "the checkpoint names the completed chats")
	assert.False(summary.LimitReached)
	assert.EqualValues(6, summary.MessagesAdded)
	assert.Equal(9, count(t, st, "SELECT count(*) FROM messages"))
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1").Scan(&status))
	assert.Equal("completed", status)
}

func TestImportWithoutGroupsFile(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	f := sampleFixture()
	f.NoGroupsFile = true
	dir := linetest.Write(t, t.TempDir(), f)
	summary, err := Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	assert.EqualValues(9, summary.MessagesAdded)
	var title string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT COALESCE(title, '') FROM conversations WHERE source_conversation_id = ?"), groupMID).Scan(&title))
	assert.Empty(title, "group names live in UnifiedGroup.sqlite")
	assert.Equal(2, count(t, st, `SELECT count(*) FROM conversation_participants cp JOIN conversations c ON c.id = cp.conversation_id WHERE c.source_conversation_id = ?`, groupMID), "owner plus the one sender seen")
}

func TestImportSkipsUnknownChatType(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	f := sampleFixture()
	f.Chats = append(f.Chats, linetest.Chat{MID: "s0000000000000000000000000000000f", Type: 3, Messages: []linetest.Message{{ID: "400", Millis: 1700000040000, Text: "square"}}})
	dir := linetest.Write(t, t.TempDir(), f)
	summary, err := Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	assert.Equal(1, summary.ChatsSkipped)
	assert.EqualValues(9, summary.MessagesAdded)
}

func TestImportLimitCancelAfterCheckpointPausesSync(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	dir := linetest.Write(t, t.TempDir(), linetest.Fixture{Chats: []linetest.Chat{{MID: ownerMID, Type: 100, Messages: []linetest.Message{
		{ID: "1", Millis: 1700000000000, Text: "first"}, {ID: "2", Millis: 1700000001000, Text: "second"},
	}}}})
	queryCtx := context.WithoutCancel(t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var saved int
		require.NoError(st.DB().QueryRowContext(queryCtx, `SELECT COUNT(*) FROM sync_runs WHERE cursor_before IS NOT NULL`).Scan(&saved))
		return saved > 0
	})
	summary, err := Import(ctx, st, ImportOptions{Path: dir, Limit: 1})
	require.NoError(err)
	require.True(summary.LimitReached)
	require.True(canceled(), "the cancel must land after the checkpoint commits")
	var status string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status))
	assert.Equal("paused", status)
}

func TestImportPreservesWhitespace(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	f := sampleFixture()
	f.Chats[1].Messages[0].Text = "  code\n  "
	f.Chats[1].Messages[9].Metadata["ALT_TEXT"] = "  rich\n  "
	dir := linetest.Write(t, t.TempDir(), f)
	_, err := Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	for id, want := range map[string]string{"100": "  code\n  ", "108": "  rich\n  "} {
		var body string
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT b.body_text FROM messages m JOIN message_bodies b ON b.message_id = m.id WHERE m.source_message_id = ?`), aliceMID+":"+id).Scan(&body))
		assert.Equal(want, body)
	}
}

func TestImportNoResumeLimitPauses(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	dir := linetest.Write(t, t.TempDir(), sampleFixture())
	_, err := Import(t.Context(), st, ImportOptions{Path: dir, Limit: 1})
	require.NoError(err)
	var id int64
	var old string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id, cursor_before FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&id, &old))
	old = strings.Replace(old, `"version":3`, `"version":999`, 1)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sync_runs SET cursor_before = ? WHERE id = ?`), old, id)
	require.NoError(err)
	summary, err := Import(t.Context(), st, ImportOptions{Path: dir, Limit: 1, NoResume: true})
	require.NoError(err)
	require.True(summary.LimitReached)
	assert.False(summary.WasResumed)
	var status, cursor string
	var processed, added int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status, COALESCE(cursor_before, ''), messages_processed, messages_added FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status, &cursor, &processed, &added))
	assert.Equal("paused", status)
	assert.NotEmpty(cursor)
	assert.EqualValues(2, processed)
	assert.EqualValues(1, added)
	summary, err = Import(t.Context(), st, ImportOptions{Path: dir, Limit: 1})
	require.NoError(err)
	assert.True(summary.WasResumed)
}

func TestImportCheckpointVersions(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	dir := linetest.Write(t, t.TempDir(), sampleFixture())
	_, err := Import(t.Context(), st, ImportOptions{Path: dir, Limit: 1})
	require.NoError(err)
	var id int64
	var cursor string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id, cursor_before FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&id, &cursor))
	cursor = strings.Replace(cursor, `"version":3`, `"version":999`, 1)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sync_runs SET cursor_before = ? WHERE id = ?`), cursor, id)
	require.NoError(err)
	_, err = Import(t.Context(), st, ImportOptions{Path: dir})
	require.ErrorContains(err, "newer than supported")
}

func TestImportUnnamedParticipantDisplay(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	f := sampleFixture()
	f.Users[0].Name, f.Users[0].CustomName = "", ""
	dir := linetest.Write(t, t.TempDir(), f)
	_, err := Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	var incoming, outgoing int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`), aliceMID+":100").Scan(&incoming))
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`), aliceMID+":101").Scan(&outgoing))
	msg, err := st.GetMessageContext(t.Context(), incoming)
	require.NoError(err)
	assert.Equal(aliceMID, msg.FromName)
	msg, err = st.GetMessageContext(t.Context(), outgoing)
	require.NoError(err)
	assert.Contains(msg.To, aliceMID)
	var recipientName string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COALESCE(display_name, '') FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`), outgoing).Scan(&recipientName))
	assert.Empty(recipientName)
	var observed string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COALESCE(p.display_name, '') FROM participants p JOIN participant_identifiers pi ON pi.participant_id = p.id WHERE pi.identifier_type = 'line' AND pi.identifier_value = ?`), aliceMID).Scan(&observed))
	assert.Empty(observed)
	listed, _, err := st.ListMessagesContext(t.Context(), 0, 100, nil)
	require.NoError(err)
	var found, foundOutgoing bool
	for _, m := range listed {
		if m.ID == incoming {
			found = true
			assert.Equal(aliceMID, m.FromName)
		}
		if m.ID == outgoing {
			foundOutgoing = true
			require.Len(m.ToParticipants, 1)
			assert.Positive(m.ToParticipants[0].ParticipantID)
			assert.Equal(aliceMID, m.ToParticipants[0].Display)
		}
	}
	assert.True(found)
	assert.True(foundOutgoing)
	hits, _, err := st.SearchMessagesContext(t.Context(), "hello", 0, 100)
	require.NoError(err)
	found = false
	for _, m := range hits {
		if m.ID == incoming {
			found = true
			assert.Equal(aliceMID, m.FromName)
		}
	}
	assert.True(found)
	_, err = st.EnsureParticipantByIdentifierContext(t.Context(), SourceType, aliceMID, "Alice")
	require.NoError(err)
	msg, err = st.GetMessageContext(t.Context(), incoming)
	require.NoError(err)
	assert.Equal("Alice", msg.FromName)
}

type cancelWriter struct{ cancel context.CancelFunc }

func (w cancelWriter) Write(p []byte) (int, error) { w.cancel(); return len(p), nil }

func TestImportFreshCancellationReplacesCheckpoint(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	dir := linetest.Write(t, t.TempDir(), sampleFixture())
	_, err := Import(t.Context(), st, ImportOptions{Path: dir, Limit: 1})
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE sync_runs SET cursor_before = '{"version":999}'`)
	require.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = Import(ctx, st, ImportOptions{Path: dir, NoResume: true, Logger: slog.New(slog.NewTextHandler(cancelWriter{cancel}, nil))})
	require.ErrorIs(err, context.Canceled)
	summary, err := Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	assert.True(summary.WasResumed)
	assert.EqualValues(8, summary.MessagesAdded)
}

func TestImportRetriesUnreadableMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, id, query, expected string
		metadata                  map[string]string
	}{
		{"rich text", "108", `SELECT b.body_text FROM messages m JOIN message_bodies b ON b.message_id = m.id WHERE m.source_message_id = ?`, " exact rich text ", map[string]string{"ALT_TEXT": " exact rich text ", "MARKUP_JSON": "{}"}},
		{"reply", "101", `SELECT p.source_message_id FROM messages m JOIN messages p ON p.id = m.reply_to_message_id WHERE m.source_message_id = ?`, aliceMID + ":100", map[string]string{"replyOriginalMessageId": "100"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			dir := linetest.Write(t, t.TempDir(), sampleFixture())
			db, err := sql.Open("sqlite3", filepath.Join(dir, "Line.sqlite"))
			require.NoError(err)
			defer func() { require.NoError(db.Close()) }()
			_, err = db.ExecContext(t.Context(), `UPDATE ZMESSAGE SET ZCONTENTMETADATA = ? WHERE ZID = ?`, []byte("unreadable"), tc.id)
			require.NoError(err)
			_, err = Import(t.Context(), st, ImportOptions{Path: dir})
			require.ErrorContains(err, tc.id)
			require.ErrorContains(err, "metadata plist is unreadable")
			require.Equal(1, count(t, st, `SELECT count(*) FROM messages WHERE source_message_id = ?`, ownerMID+":900"))
			var status, cursor string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status, cursor_before FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status, &cursor))
			require.Equal("failed", status)
			var cp lineCheckpoint
			require.NoError(json.Unmarshal([]byte(cursor), &cp))
			require.Equal(1, cp.NextChat)
			_, err = db.ExecContext(t.Context(), `UPDATE ZMESSAGE SET ZCONTENTMETADATA = ? WHERE ZID = ?`, linetest.KeyedArchive(t, tc.metadata), tc.id)
			require.NoError(err)
			result, err := Import(t.Context(), st, ImportOptions{Path: dir})
			require.NoError(err)
			require.False(result.WasResumed)
			require.Equal(1, count(t, st, `SELECT count(*) FROM messages WHERE source_message_id = ?`, ownerMID+":900"))
			require.Equal(1, count(t, st, `SELECT count(*) FROM messages WHERE source_message_id = ?`, aliceMID+":"+tc.id))
			var actual string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(tc.query), aliceMID+":"+tc.id).Scan(&actual))
			require.Equal(tc.expected, actual)
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status))
			require.Equal("completed", status)
		})
	}
}

func TestImportLimitStopsBeforeUnreadableMetadata(t *testing.T) {
	for _, tc := range []struct {
		limit  int
		pauses bool
	}{{1, true}, {2, false}} {
		t.Run(fmt.Sprintf("limit-%d", tc.limit), func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			f := sampleFixture()
			f.Chats[0].Messages = append(f.Chats[0].Messages, linetest.Message{ID: "901", Millis: 1700000000001, Text: "later"})
			dir := linetest.Write(t, t.TempDir(), f)
			db, err := sql.Open("sqlite3", filepath.Join(dir, "Line.sqlite"))
			require.NoError(err)
			_, err = db.ExecContext(t.Context(), `UPDATE ZMESSAGE SET ZCONTENTMETADATA = ? WHERE ZID = ?`, []byte("unreadable"), "901")
			require.NoError(err)
			require.NoError(db.Close())
			result, err := Import(t.Context(), st, ImportOptions{Path: dir, Limit: tc.limit})
			if tc.pauses {
				require.NoError(err)
				require.True(result.LimitReached)
			} else {
				require.ErrorContains(err, "901")
			}
			require.Equal(1, count(t, st, `SELECT count(*) FROM messages WHERE source_message_id = ?`, ownerMID+":900"))
			if tc.pauses {
				_, err = Import(t.Context(), st, ImportOptions{Path: dir, Limit: 1})
				require.ErrorContains(err, "901")
			}
		})
	}
}

func TestImportSearchMatchesStoredParticipants(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	pid, err := st.EnsureParticipantByIdentifierContext(t.Context(), SourceType, aliceMID, "Alice")
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE participants SET phone_number = ? WHERE id = ?`), "5551234567", pid)
	require.NoError(err)
	dir := linetest.Write(t, t.TempDir(), sampleFixture())
	_, err = Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	_, before, err := st.SearchMessages("5551234567", 0, 100)
	require.NoError(err)
	_, err = st.RebuildFTS(nil)
	require.NoError(err)
	_, after, err := st.SearchMessages("5551234567", 0, 100)
	require.NoError(err)
	require.Positive(after)
	require.Equal(after, before)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE participants SET phone_number = ? WHERE id = ?`), "5557654321", pid)
	require.NoError(err)
	_, err = Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	_, refreshed, err := st.SearchMessages("5557654321", 0, 100)
	require.NoError(err)
	require.Equal(after, refreshed)
}

func TestImportClearsObservedGroupTitle(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	dir := linetest.Write(t, t.TempDir(), sampleFixture())
	_, err := Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	groupPath := filepath.Join(dir, "UnifiedGroup.sqlite")
	group, err := os.ReadFile(groupPath)
	require.NoError(err)
	require.NoError(os.Remove(groupPath))
	_, err = Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	var title string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COALESCE(title, '') FROM conversations WHERE source_conversation_id = ?`), groupMID).Scan(&title))
	require.Equal("Book club", title)
	require.NoError(os.WriteFile(groupPath, group, 0600))
	db, err := sql.Open("sqlite3", groupPath)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `UPDATE ZUNIFIEDGROUP SET ZNAME = ''`)
	require.NoError(err)
	require.NoError(db.Close())
	_, err = Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COALESCE(title, '') FROM conversations WHERE source_conversation_id = ?`), groupMID).Scan(&title))
	require.Empty(title)
}

func TestImportSkipsUnreadableMediaMetadata(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	dir := linetest.Write(t, t.TempDir(), sampleFixture())
	db, err := sql.Open("sqlite3", filepath.Join(dir, "Line.sqlite"))
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `UPDATE ZMESSAGE SET ZCONTENTMETADATA = ? WHERE ZID = '102'`, []byte("unreadable"))
	require.NoError(err)
	require.NoError(db.Close())
	summary, err := Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	require.EqualValues(1, summary.MediaSkipped)
	require.Equal(1, count(t, st, `SELECT count(*) FROM messages WHERE source_message_id = ?`, roomMID+":300"))
}
