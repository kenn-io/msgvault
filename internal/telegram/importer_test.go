package telegram

import (
	"context"
	"database/sql"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	jsontext "encoding/json/jsontext"
	json "encoding/json/v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Chat ids of the full fixture, in export order.
const (
	fixturePersonalChatID = 7000000000000987654
	fixtureGroupChatID    = -1009999999999
	fixtureChannelChatID  = -1001111111111
	fixtureBotChatID      = 7000000000000444444
)

func importFixture(t *testing.T, st *store.Store, name string, mutate func(t *testing.T, opts *ImportOptions)) *ImportSummary {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", name))
	require.NoError(t, err)
	opts := ImportOptions{RootDir: dir, AttachmentsDir: t.TempDir()}
	if mutate != nil {
		mutate(t, &opts)
	}
	summary, err := Import(context.Background(), st, opts)
	require.NoError(t, err)
	return summary
}

func importRoot(t *testing.T, st *store.Store, dir string) *ImportSummary {
	t.Helper()
	summary, err := Import(context.Background(), st, ImportOptions{
		RootDir: dir, AttachmentsDir: t.TempDir(),
	})
	require.NoError(t, err)
	return summary
}

func TestImportFullExport(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)
	summary := importFixture(t, st, "full", nil)

	assert.Equal("telegram", SourceType)
	assert.Equal(4, summary.ChatsProcessed)
	assert.EqualValues(11, summary.MessagesProcessed, "eleven regular messages")
	assert.EqualValues(1, summary.ServiceMessagesSkipped, "service message dropped")
	assert.EqualValues(11, summary.MessagesAdded)

	var sourceType, sourceID string
	require.NoError(st.DB().QueryRow(
		`SELECT source_type, identifier FROM sources WHERE source_type = 'telegram'`,
	).Scan(&sourceType, &sourceID))
	assert.Equal("user7000000000000123456", sourceID, "source identifier is the exporting user")

	// Conversation keyed on the decimal chat id, typed by the export's type.
	var convType string
	require.NoError(st.DB().QueryRow(`
		SELECT conversation_type FROM conversations c
		JOIN sources s ON s.id = c.source_id
		WHERE s.source_type = 'telegram' AND c.source_conversation_id = '-1009999999999'`,
	).Scan(&convType))
	assert.Equal("group_chat", convType, "supergroup maps to group_chat")
	require.NoError(st.DB().QueryRow(`
		SELECT conversation_type FROM conversations c
		JOIN sources s ON s.id = c.source_id
		WHERE s.source_type = 'telegram' AND c.source_conversation_id = '-1001111111111'`,
	).Scan(&convType))
	assert.Equal("channel", convType, "channel maps to channel")

	// Message keyed "<chat-id>:<message-id>" with type telegram.
	var count int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*) FROM messages m
		JOIN sources s ON s.id = m.source_id
		WHERE s.source_type = 'telegram' AND m.message_type = 'telegram'`,
	).Scan(&count))
	assert.Equal(11, count)

	var body string
	require.NoError(st.DB().QueryRow(`
		SELECT b.body_text FROM message_bodies b
		JOIN messages m ON m.id = b.message_id
		WHERE m.source_message_id = '7000000000000987654:102'`,
	).Scan(&body))
	assert.Equal("see https://example.org/docs and @alice", body, "flattened entity text")

	// is_from_me from from_id == user<owner>.
	var fromMe int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*) FROM messages m
		JOIN sources s ON s.id = m.source_id
		WHERE s.source_type = 'telegram' AND m.is_from_me`,
	).Scan(&fromMe))
	assert.Equal(1, fromMe, "one message from the owner")

	// Attachment bytes stored for photo and voice file.
	var stored int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*) FROM attachments a
		JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id IN ('7000000000000987654:103', '-1009999999999:201')
		  AND a.storage_path <> ''`,
	).Scan(&stored))
	assert.Equal(2, stored, "photo and voice stored")
	assert.Equal(2, summary.AttachmentsStored)

	// Hostile media paths keep their message but never read a file.
	var missing int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*) FROM attachments a
		JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id IN ('-1009999999999:204', '-1009999999999:205')
		  AND COALESCE(a.storage_path, '') = ''`,
	).Scan(&missing))
	assert.Equal(2, missing, "traversal and absolute paths recorded as missing")
}

func TestImportSingleChatRequiresOwnerOverride(t *testing.T) {
	st := testutil.NewTestStore(t)

	_, err := Import(context.Background(), st, ImportOptions{
		RootDir: filepath.Join("testdata", "single"), AttachmentsDir: t.TempDir(),
	})
	require.ErrorIs(t, err, ErrNoOwner)

	summary := importFixture(t, st, "single", func(t *testing.T, opts *ImportOptions) {
		t.Helper()
		opts.Me = "7000000000000888888"
	})
	var fromMe int
	require.NoError(t, st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE message_type = 'telegram' AND is_from_me`,
	).Scan(&fromMe))
	assert.Equal(t, 2, fromMe, "--me override drives is_from_me")
	assert.Equal(t, 1, summary.ChatsProcessed)
}

func TestImportInvalidMeOverride(t *testing.T) {
	st := testutil.NewTestStore(t)

	_, err := Import(context.Background(), st, ImportOptions{
		RootDir: filepath.Join("testdata", "full"), AttachmentsDir: t.TempDir(),
		Me: "alice",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "numeric Telegram user ID")
}

func TestImportIdempotentOverlap(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)
	importFixture(t, st, "full", nil)
	summary := importFixture(t, st, "full", nil)

	var count int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE message_type = 'telegram'`,
	).Scan(&count))
	assert.Equal(11, count, "one row per <chat-id>:<message-id>")

	assert.EqualValues(0, summary.MessagesAdded, "re-import adds nothing")
	assert.EqualValues(11, summary.MessagesSkipped, "re-import skips every row")
	assert.False(summary.WasResumed, "a completed run is not resumed")
}

func TestImportDifferentRootAddsOnlyNew(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)
	first := t.TempDir()
	copyFullFixture(t, first)
	importRoot(t, st, first)

	// A newer export from a different directory adds one message.
	second := t.TempDir()
	copyFullFixture(t, second)
	appendMessage(t, second, "7000000000000987654", 105, "fresh message from a newer export")

	summary := importRoot(t, st, second)
	assert.EqualValues(1, summary.MessagesAdded, "only the new message is added")
	assert.EqualValues(11, summary.MessagesSkipped)
	assert.False(summary.WasResumed, "a different root starts fresh, not an error")

	var count int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE message_type = 'telegram'`,
	).Scan(&count))
	assert.Equal(12, count)
}

// TestImportOverlapNeverMutatesArchivedRows pins the append-only contract:
// a later export of the same chats changes nothing about messages an
// earlier import archived — bodies, recipients, attachments, and reply
// links all stay as first written.
func TestImportOverlapNeverMutatesArchivedRows(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	copyFullFixture(t, dir)
	importRoot(t, st, dir)

	storedPath, storedHash := attachmentRow(t, st, "7000000000000987654:103")
	require.NotEmpty(storedPath, "photo bytes stored on first import")

	// A newer export drops the photo, rewrites the body, and turns the
	// message into a reply to one that did not exist before.
	setChatMessages(t, dir, "7000000000000987654", []map[string]any{
		{
			"id": 103, "type": "message", "date_unixtime": "1610909950",
			"from": "Alice Example", "from_id": "user7000000000000555555",
			"text": "rewritten caption", "reply_to_message_id": 999,
		},
	})
	importRoot(t, st, dir)

	var body sql.NullString
	require.NoError(st.DB().QueryRow(`
		SELECT b.body_text FROM message_bodies b
		JOIN messages m ON m.id = b.message_id
		WHERE m.source_message_id = '7000000000000987654:103'`).Scan(&body))
	assert.False(body.Valid, "an empty archived body stays empty")

	path, hash := attachmentRow(t, st, "7000000000000987654:103")
	assert.Equal(storedPath, path, "removed media keeps its stored row and bytes")
	assert.Equal(storedHash, hash)

	var replyTo *int64
	require.NoError(st.DB().QueryRow(`
		SELECT reply_to_message_id FROM messages
		WHERE source_message_id = '7000000000000987654:103'`).Scan(&replyTo))
	assert.Nil(replyTo, "an archived message keeps its reply links as first written")
}

// attachmentRow returns one message's first attachment storage identity.
func attachmentRow(t *testing.T, st *store.Store, sourceMessageID string) (path, hash string) {
	t.Helper()
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COALESCE(a.storage_path, ''), COALESCE(a.content_hash, '')
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = ?`), sourceMessageID).Scan(&path, &hash))
	return
}

// TestImportDirectChatDerivesToRecipients pins the only "to" derivation the
// export determines: in a personal chat the message's to recipient is the
// chat's other participant, owner and peer in both directions.
func TestImportDirectChatDerivesToRecipients(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)

	alice := map[string]any{
		"id": 1, "type": "message", "date_unixtime": "1610909800",
		"from": "Alice Example", "from_id": "user7000000000000555555", "text": "from alice",
	}
	owner := maps.Clone(alice)
	owner["id"] = 2
	owner["from"] = "Test Owner"
	owner["from_id"] = "user7000000000000123456"
	owner["text"] = "from owner"
	root := writeChatExport(t, t.TempDir(), "personal_chat", "Alice Example",
		[]map[string]any{alice, owner})
	importRoot(t, st, root)

	ownerPID, _, err := st.ParticipantByIdentifier("telegram", "7000000000000123456")
	require.NoError(err)
	alicePID, _, err := st.ParticipantByIdentifier("telegram", "7000000000000555555")
	require.NoError(err)

	toRecipient := func(sourceMessageID string) int64 {
		var pid int64
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT mr.participant_id FROM message_recipients mr
			JOIN messages m ON m.id = mr.message_id
			WHERE m.source_message_id = ? AND mr.recipient_type = 'to'`),
			sourceMessageID).Scan(&pid))
		return pid
	}
	assert.Equal(ownerPID, toRecipient("7000000000000777001:1"), "peer's message is to the owner")
	assert.Equal(alicePID, toRecipient("7000000000000777001:2"), "owner's message is to the peer")
}

// TestImportGroupAndUndeterminedChatsHaveNoToRecipients pins the other
// half: group and channel messages stay sender-only, and a personal chat
// whose messages never reveal the peer carries no "to" recipients.
func TestImportGroupAndUndeterminedChatsHaveNoToRecipients(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)

	ownerOnly := map[string]any{
		"id": 1, "type": "message", "date_unixtime": "1610909800",
		"from": "Test Owner", "from_id": "user7000000000000123456", "text": "just me",
	}
	solo := writeChatExport(t, t.TempDir(), "personal_chat", "Notes",
		[]map[string]any{ownerOnly})
	importRoot(t, st, solo)

	group := maps.Clone(ownerOnly)
	group["id"] = 2
	group["from"] = "Alice Example"
	group["from_id"] = "user7000000000000555555"
	groupRoot := writeChatExport(t, t.TempDir(), "private_group", "A Group",
		[]map[string]any{ownerOnly, group})
	importRoot(t, st, groupRoot)

	var toCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_recipients mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_message_id IN ('7000000000000777001:1', '7000000000000777001:2')
		  AND mr.recipient_type = 'to'`)).Scan(&toCount))
	assert.Zero(toCount, "no synthetic to recipients without an observed peer")
}

// TestImportBotChatToRecipient pins the bot_chat derivation on the full
// fixture: the bot's left-chat message is to the owner.
func TestImportBotChatToRecipient(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)
	importFixture(t, st, "full", nil)

	ownerPID, _, err := st.ParticipantByIdentifier("telegram", "7000000000000123456")
	require.NoError(err)
	var toPID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mr.participant_id FROM message_recipients mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_message_id = '7000000000000444444:301'
		  AND mr.recipient_type = 'to'`)).Scan(&toPID))
	assert.Equal(ownerPID, toPID, "bot_chat message is to the owner")

	// Group and channel messages stay sender-only.
	var groupTo int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_recipients mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_message_id LIKE '-1009999999999:%'
		  AND mr.recipient_type = 'to'`)).Scan(&groupTo))
	assert.Zero(groupTo, "group messages carry no to recipients")
}

// TestImportReplyLinksAcrossExports pins reply resolution through the
// store: a parent archived by an earlier export links to a reply imported
// by a later, disjoint export of the same chat.
func TestImportReplyLinksAcrossExports(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)

	parent := map[string]any{
		"id": 1, "type": "message", "date_unixtime": "1610909800",
		"from": "Alice Example", "from_id": "user7000000000000555555", "text": "original",
	}
	first := writeChatExport(t, t.TempDir(), "personal_chat", "Reply Chat",
		[]map[string]any{parent})
	importRoot(t, st, first)

	reply := maps.Clone(parent)
	reply["id"] = 2
	reply["text"] = "a reply"
	reply["reply_to_message_id"] = 1
	second := writeChatExport(t, t.TempDir(), "personal_chat", "Reply Chat",
		[]map[string]any{reply})
	importRoot(t, st, second)

	var parentSrc string
	require.NoError(st.DB().QueryRow(`
		SELECT p.source_message_id FROM messages c
		JOIN messages p ON p.id = c.reply_to_message_id
		WHERE c.source_message_id = '7000000000000777001:2'`).Scan(&parentSrc))
	assert.Equal("7000000000000777001:1", parentSrc, "reply links to the earlier-export parent")
}

// TestImportReplyLinksForwardReference pins the post-chat reply pass: a
// reply whose parent appears later in the same chat still links.
func TestImportReplyLinksForwardReference(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)

	base := map[string]any{
		"type": "message", "date_unixtime": "1610909800",
		"from": "Alice Example", "from_id": "user7000000000000555555",
	}
	reply := maps.Clone(base)
	reply["id"] = 1
	reply["text"] = "a reply"
	reply["reply_to_message_id"] = 2
	parent := maps.Clone(base)
	parent["id"] = 2
	parent["text"] = "original"
	root := writeChatExport(t, t.TempDir(), "personal_chat", "Forward Chat",
		[]map[string]any{reply, parent})
	importRoot(t, st, root)

	var linked bool
	require.NoError(st.DB().QueryRow(`
		SELECT reply_to_message_id IS NOT NULL FROM messages
		WHERE source_message_id = '7000000000000777001:1'`).Scan(&linked))
	assert.True(linked, "a forward reference links once the parent is imported")
}

// TestImportEditedStateSetOnceOnFirstImport pins the edited flag and its
// append-only handling: the flag is written on first import and a later
// export cannot change it.
func TestImportEditedStateSetOnceOnFirstImport(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)

	plain := map[string]any{
		"id": 1, "type": "message", "date_unixtime": "1610909800",
		"from": "Alice Example", "from_id": "user7000000000000555555", "text": "plain",
	}
	edited := maps.Clone(plain)
	edited["id"] = 2
	edited["text"] = "now edited"
	edited["edited"] = "2021-01-17T19:13:19:00"
	edited["edited_unixtime"] = "1610910799"
	root := writeChatExport(t, t.TempDir(), "personal_chat", "Edited Chat",
		[]map[string]any{plain, edited})
	importRoot(t, st, root)

	flagged := func() []bool {
		var flags []bool
		rows, err := st.DB().Query(`
			SELECT is_edited FROM messages
			WHERE source_message_id IN ('7000000000000777001:1', '7000000000000777001:2')
			ORDER BY source_message_id`)
		require.NoError(err)
		defer func() { require.NoError(rows.Close()) }()
		for rows.Next() {
			var flag bool
			require.NoError(rows.Scan(&flag))
			flags = append(flags, flag)
		}
		require.NoError(rows.Err())
		return flags
	}
	assert.Equal([]bool{false, true}, flagged())

	// A newer export removes the edit marker; the archived flag stays.
	setChatMessages(t, root, "7000000000000777001", []map[string]any{plain, edited})
	doc := loadFullExport(t, root)
	require.Len(doc.Chats.List, 1)
	delete(doc.Chats.List[0].Messages[1], "edited")
	delete(doc.Chats.List[0].Messages[1], "edited_unixtime")
	saveFullExport(t, root, doc)
	importRoot(t, st, root)
	assert.Equal([]bool{false, true}, flagged(), "edited state never mutates after first import")
}

// TestImportPreservesDocumentMetadata pins authoritative document metadata:
// the export's official file_name and mime_type reach the attachment row
// instead of a basename-derived guess.
func TestImportPreservesDocumentMetadata(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)
	root := writeChatExport(t, t.TempDir(), "personal_chat", "Docs Chat", []map[string]any{{
		"id": 1, "type": "message", "date_unixtime": "1610909800",
		"from": "Alice Example", "from_id": "user7000000000000555555",
		"file": "documents/report_1.bin", "file_name": "Quarterly Report.pdf",
		"mime_type": "application/pdf", "media_type": "document", "text": "",
	}})
	importRoot(t, st, root)

	var filename, mimeType string
	require.NoError(st.DB().QueryRow(`
		SELECT a.filename, a.mime_type FROM attachments a
		JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = '7000000000000777001:1'`).Scan(&filename, &mimeType))
	assert.Equal("Quarterly Report.pdf", filename)
	assert.Equal("application/pdf", mimeType)
}

// TestParticipantsResolvedCountsAllSenders pins the summary counter:
// ParticipantsResolved counts every distinct participant the run resolved —
// the owner plus every sender across chats, including channel and anonymous
// group senders.
func TestParticipantsResolvedCountsAllSenders(t *testing.T) {
	st := testutil.NewTestStore(t)
	summary := importFixture(t, st, "full", nil)
	assert.Equal(t, 6, summary.ParticipantsResolved,
		"owner, Alice, Bob, the channel, the anonymous group admin, and the bot")
}

// TestParticipantSummaryCountsResolvedSenders pins the same counter on a
// small export: the owner plus two other senders.
func TestParticipantSummaryCountsResolvedSenders(t *testing.T) {
	st := testutil.NewTestStore(t)
	base := map[string]any{
		"type": "message", "date_unixtime": "1610909800", "text": "hello",
	}
	alice := maps.Clone(base)
	alice["id"] = 1
	alice["from"] = "Alice Example"
	alice["from_id"] = "user7000000000000555555"
	bob := maps.Clone(base)
	bob["id"] = 2
	bob["from"] = "Bob Sample"
	bob["from_id"] = "user7000000000000666666"
	owner := maps.Clone(base)
	owner["id"] = 3
	owner["from"] = "Test Owner"
	owner["from_id"] = "user7000000000000123456"
	root := writeChatExport(t, t.TempDir(), "private_group", "Counting Group",
		[]map[string]any{alice, bob, owner})
	summary := importRoot(t, st, root)
	assert.Equal(t, 3, summary.ParticipantsResolved, "the owner and both senders")
}

func TestImportLimitMidChatAndResumeCompletes(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)
	dir, err := filepath.Abs("testdata/full")
	require.NoError(err)

	limited, err := Import(context.Background(), st, ImportOptions{
		RootDir: dir, AttachmentsDir: t.TempDir(), Limit: 2,
	})
	require.NoError(err)
	assert.EqualValues(2, limited.MessagesProcessed, "--limit caps processed messages")

	rest, err := Import(context.Background(), st, ImportOptions{
		RootDir: dir, AttachmentsDir: t.TempDir(),
	})
	require.NoError(err)
	// A limited run completes, so the follow-up scans fresh; the scan skips
	// the two already-imported messages and adds the remaining nine.
	assert.False(rest.WasResumed)
	assert.EqualValues(11, rest.MessagesProcessed)
	assert.EqualValues(9, rest.MessagesAdded, "remaining messages import after the limited run")

	var count int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE message_type = 'telegram'`,
	).Scan(&count))
	assert.Equal(11, count)
}

// failSyncAt simulates an interrupted run: a failed sync carrying a
// checkpoint over the export at dir with the given completed chats.
func failSyncAt(t *testing.T, st *store.Store, dir string, completed ...int64) {
	t.Helper()
	source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(7000000000000123456))
	require.NoError(t, err)
	sourceID := source.ID
	r, err := OpenExport(dir)
	require.NoError(t, err)
	fingerprint := r.Fingerprint()
	require.NoError(t, r.Close())
	cursor, err := json.Marshal(telegramCheckpoint{
		RootDir: dir, Fingerprint: fingerprint, CompletedChats: completed,
	}, json.Deterministic(true))
	require.NoError(t, err)
	syncID, err := st.StartSync(sourceID, syncType)
	require.NoError(t, err)
	require.NoError(t, st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{PageToken: string(cursor)}))
	require.NoError(t, st.FailSync(syncID, "interrupted"))
}

func TestImportResumesCheckpointFromSameRoot(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)
	dir, err := filepath.Abs("testdata/full")
	require.NoError(err)
	importRoot(t, st, dir)

	failSyncAt(t, st, dir, fixturePersonalChatID)

	summary := importRoot(t, st, dir)
	assert.True(summary.WasResumed, "same-root checkpoint over identical bytes resumes")
	assert.Equal(3, summary.ChatsProcessed, "resumed run skips completed chats")
	assert.EqualValues(8, summary.MessagesProcessed, "only chats after the checkpoint process")
	assert.EqualValues(0, summary.MessagesAdded, "everything was already archived")

	var count int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE message_type = 'telegram'`,
	).Scan(&count))
	assert.Equal(11, count)
}

// TestImportResumeReprocessesInterruptedChatIdempotently pins the
// chat-granular resume: an interrupted chat's already-archived messages are
// skipped and only the unseen remainder imports.
func TestImportResumeReprocessesInterruptedChatIdempotently(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	copyFullFixture(t, dir)

	// Two messages of the first chat landed before the interruption.
	limited, err := Import(context.Background(), st, ImportOptions{
		RootDir: dir, AttachmentsDir: t.TempDir(), Limit: 2,
	})
	require.NoError(err)
	require.EqualValues(2, limited.MessagesAdded)
	failSyncAt(t, st, dir) // no chat completed

	summary := importRoot(t, st, dir)
	assert.True(summary.WasResumed)
	var count int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE message_type = 'telegram'`,
	).Scan(&count))
	assert.Equal(11, count)
}

// TestImportResumeReEnsuresTornMedia pins the resume half of media
// handling: reprocessing an interrupted chat restores an attachment row an
// interruption cost, without touching anything else.
func TestImportResumeReEnsuresTornMedia(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	copyFullFixture(t, dir)
	importRoot(t, st, dir)

	// Tear the write: the photo message keeps its row, its attachment row
	// is gone, and only its chat is incomplete.
	_, deleteErr := st.DB().Exec(st.Rebind(`
		DELETE FROM attachments WHERE message_id = (
			SELECT id FROM messages WHERE source_message_id = '7000000000000987654:103')`))
	require.NoError(deleteErr)
	failSyncAt(t, st, dir, fixtureGroupChatID, fixtureChannelChatID, fixtureBotChatID)

	importRoot(t, st, dir)
	path, hash := attachmentRow(t, st, "7000000000000987654:103")
	assert.NotEmpty(path, "torn media row is restored from the export file")
	assert.NotEmpty(hash)
}

// TestImportResumeChangedExportAtSameRoot pins the resume guard: replacing
// result.json at the checkpointed path with changed content restarts the
// scan instead of trusting the completed set of the old file.
func TestImportResumeChangedExportAtSameRoot(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	copyFullFixture(t, dir)
	importRoot(t, st, dir)

	failSyncAt(t, st, dir, fixturePersonalChatID)

	// The export at the same path is replaced by a newer version that
	// carries fresh content in a completed chat.
	appendMessage(t, dir, "7000000000000987654", 105, "fresh content in a completed chat")

	summary := importRoot(t, st, dir)
	assert.False(summary.WasResumed, "a changed export is not a resume of the old bytes")
	assert.EqualValues(1, summary.MessagesAdded, "changed content is re-scanned, not skipped")

	var count int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE message_type = 'telegram'`,
	).Scan(&count))
	assert.Equal(12, count)
}

func TestImportNoResumeIgnoresCheckpoint(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)
	dir, err := filepath.Abs("testdata/full")
	require.NoError(err)

	failSyncAt(t, st, dir, fixturePersonalChatID)

	summary, err := Import(context.Background(), st, ImportOptions{
		RootDir: dir, AttachmentsDir: t.TempDir(), NoResume: true,
	})
	require.NoError(err)
	assert.False(summary.WasResumed, "--no-resume ignores the checkpoint")
	assert.Equal(4, summary.ChatsProcessed)
	assert.EqualValues(11, summary.MessagesAdded)
}

// TestImportMediaRoles pins the explicit media roles: stickers and
// thumbnails carry their provider-explicit roles, documents stay standalone.
func TestImportMediaRoles(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(dir, "export", "stickers"), 0o755))
	require.NoError(os.MkdirAll(filepath.Join(dir, "export", "thumbnails"), 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(dir, "export", "stickers", "sticker_1.webm"), []byte("sticker-bytes"), 0o644))
	require.NoError(os.WriteFile(
		filepath.Join(dir, "export", "thumbnails", "thumb_1.jpg"), []byte("thumb-bytes"), 0o644))

	sticker := map[string]any{
		"id": 1, "type": "message", "date_unixtime": "1610909800",
		"from": "Alice Example", "from_id": "user7000000000000555555",
		"file": "stickers/sticker_1.webm", "sticker_emoji": "👍", "text": "",
	}
	photo := map[string]any{
		"id": 2, "type": "message", "date_unixtime": "1610909810",
		"from": "Alice Example", "from_id": "user7000000000000555555",
		"photo": "photos/photo_1.jpg", "thumbnail": "thumbnails/thumb_1.jpg", "text": "",
	}
	root := writeChatExport(t, dir, "personal_chat", "Media Chat",
		[]map[string]any{sticker, photo})
	require.NoError(os.MkdirAll(filepath.Join(root, "photos"), 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(root, "photos", "photo_1.jpg"), []byte("photo-bytes"), 0o644))
	importRoot(t, st, root)

	roleFor := func(sourceMessageID string) map[string]string {
		rows, err := st.DB().Query(st.Rebind(`
			SELECT a.attachment_role, a.storage_path FROM attachments a
			JOIN messages m ON m.id = a.message_id
			WHERE m.source_message_id = ?`), sourceMessageID)
		require.NoError(err)
		defer func() { require.NoError(rows.Close()) }()
		out := map[string]string{}
		for rows.Next() {
			var role, path string
			require.NoError(rows.Scan(&role, &path))
			out[role] = path
		}
		require.NoError(rows.Err())
		return out
	}
	stickerRoles := roleFor("7000000000000777001:1")
	assert.Len(stickerRoles, 1, "the sticker message has one attachment")
	assert.NotEmpty(stickerRoles["sticker"], "sticker carries the sticker role with stored bytes")

	photoRoles := roleFor("7000000000000777001:2")
	assert.Len(photoRoles, 2, "photo and thumbnail rows")
	assert.NotEmpty(photoRoles["thumbnail"], "thumbnail bytes stored")
	assert.NotEmpty(photoRoles["standalone"], "photo stays standalone")
}

// TestImportSymlinkedMediaStaysContained pins symlink resolution: a media
// path that resolves outside the export root is rejected without reading
// the target, while a symlink that stays inside the root is followed.
func TestImportSymlinkedMediaStaysContained(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	root := writeChatExport(t, dir, "personal_chat", "Symlink Chat", []map[string]any{{
		"id": 1, "type": "message", "date_unixtime": "1610909800",
		"from": "Alice Example", "from_id": "user7000000000000555555",
		"photo": "photos/escape.jpg", "text": "",
	}})
	require.NoError(os.MkdirAll(filepath.Join(root, "photos"), 0o755))
	outside := filepath.Join(dir, "outside.jpg")
	require.NoError(os.WriteFile(outside, []byte("outside-bytes"), 0o644))
	require.NoError(os.Symlink(outside, filepath.Join(root, "photos", "escape.jpg")))
	importRoot(t, st, root)

	path, hash := attachmentRow(t, st, "7000000000000777001:1")
	assert.Empty(path, "an escaping symlink stores nothing")
	assert.Empty(hash)

	// A symlink that stays inside the root is followed to its target.
	require.NoError(os.Remove(filepath.Join(root, "photos", "escape.jpg")))
	require.NoError(os.WriteFile(filepath.Join(root, "photos", "real.jpg"), []byte("inside-bytes"), 0o644))
	require.NoError(os.Symlink("real.jpg", filepath.Join(root, "photos", "escape.jpg")))
	doc := loadFullExport(t, root)
	require.Len(doc.Chats.List, 1)
	doc.Chats.List[0].Messages = append(doc.Chats.List[0].Messages, map[string]any{
		"id": 2, "type": "message", "date_unixtime": "1610909810",
		"from": "Alice Example", "from_id": "user7000000000000555555",
		"photo": "photos/escape.jpg", "text": "",
	})
	saveFullExport(t, root, doc)
	importRoot(t, st, root)
	path, hash = attachmentRow(t, st, "7000000000000777001:2")
	assert.NotEmpty(path, "an inside symlink is followed")
	assert.NotEmpty(hash)
}

// copyFullFixture copies the full-export fixture into dst.
func copyFullFixture(t *testing.T, dst string) {
	t.Helper()
	require.NoError(t, os.CopyFS(dst, os.DirFS(filepath.Join("testdata", "full"))))
}

// appendMessage writes one more message into a copied export's chat. The
// typed decode keeps chat ids int64 and personal_information raw: a
// map[string]any round trip would lossy-float the 19-digit ids and change
// the export's owner.
func appendMessage(t *testing.T, root string, chatID string, messageID int64, text string) {
	t.Helper()
	doc := loadFullExport(t, root)
	for i, chat := range doc.Chats.List {
		if strconv.FormatInt(chat.ID, 10) != chatID {
			continue
		}
		doc.Chats.List[i].Messages = append(doc.Chats.List[i].Messages, map[string]any{
			"id":            messageID,
			"type":          "message",
			"date_unixtime": "1610915000",
			"from":          "Alice Example",
			"from_id":       "user7000000000000555555",
			"text":          text,
		})
	}
	saveFullExport(t, root, doc)
}

// fullExportChat is one chats.list entry during an in-place fixture edit.
type fullExportChat struct {
	ID       int64            `json:"id"`
	Type     string           `json:"type"`
	Name     string           `json:"name"`
	Messages []map[string]any `json:"messages"`
}

// fullExportDoc mirrors the full export's shape for in-place edits. The raw
// personal_information block survives the round trip unchanged, and chat ids
// stay int64: a map[string]any round trip would lossy-float the 19-digit ids
// and change the export's owner.
type fullExportDoc struct {
	PersonalInformation jsontext.Value `json:"personal_information"`
	Chats               struct {
		List []fullExportChat `json:"list"`
	} `json:"chats"`
	LeftChats *struct {
		List []fullExportChat `json:"list"`
	} `json:"left_chats,omitempty"`
}

func loadFullExport(t *testing.T, root string) *fullExportDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "result.json"))
	require.NoError(t, err)
	var doc fullExportDoc
	require.NoError(t, json.Unmarshal(raw, &doc))
	return &doc
}

func saveFullExport(t *testing.T, root string, doc *fullExportDoc) {
	t.Helper()
	out, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "result.json"), out, 0o644))
}

// setChatMessages replaces one chat's messages in a copied export.
func setChatMessages(t *testing.T, root string, chatID string, messages []map[string]any) {
	t.Helper()
	doc := loadFullExport(t, root)
	for i, chat := range doc.Chats.List {
		if strconv.FormatInt(chat.ID, 10) == chatID {
			doc.Chats.List[i].Messages = messages
			saveFullExport(t, root, doc)
			return
		}
	}
	t.Fatalf("chat %s not found in %s", chatID, root)
}

// writeChatExport writes a full-shaped export whose single chat carries the
// given message objects and returns its root directory. The owner is the
// fixture owner, so no --me override is needed.
func writeChatExport(t *testing.T, dir, chatType, chatName string, messages []map[string]any) string {
	t.Helper()
	doc := fullExportDoc{
		PersonalInformation: jsontext.Value(`{"user_id":7000000000000123456,"first_name":"Test","last_name":"Owner"}`),
	}
	doc.Chats.List = []fullExportChat{{
		ID: 7000000000000777001, Type: chatType, Name: chatName, Messages: messages,
	}}
	b, err := json.Marshal(doc)
	require.NoError(t, err)
	root := filepath.Join(dir, "export")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "result.json"), b, 0o644))
	return root
}
