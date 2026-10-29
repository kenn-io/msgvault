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
	fixturePersonalChatID = 7000000000000555555
	fixtureGroupChatID    = -1009999999999
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
		WHERE m.source_message_id = '7000000000000555555:102'`,
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
		WHERE m.source_message_id IN ('7000000000000555555:103', '-1009999999999:201')
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

// TestImportFullExportWithoutOptionalChats pins root detection on the
// documented shape: chats and left_chats are optional containers, so a
// full export without them imports zero chats instead of being misread as
// one chat object.
func TestImportFullExportWithoutOptionalChats(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(root, "result.json"), []byte(`{
		"about": "Here is all your data",
		"personal_information": {
			"user_id": 7000000000000123456,
			"first_name": "Test",
			"last_name": "Owner"
		}
	}`), 0o644))

	summary, err := Import(context.Background(), st, ImportOptions{
		RootDir: root, AttachmentsDir: t.TempDir(),
	})
	require.NoError(err)
	assert.Zero(summary.ChatsProcessed)
	assert.Zero(summary.MessagesProcessed)

	var count int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE message_type = 'telegram'`).Scan(&count))
	assert.Zero(count)
}

func TestImportSingleChatRequiresOwnerOverride(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)

	_, err := Import(context.Background(), st, ImportOptions{
		RootDir: filepath.Join("testdata", "single"), AttachmentsDir: t.TempDir(),
	})
	require.ErrorIs(err, ErrNoOwner)

	summary := importFixture(t, st, "single", func(t *testing.T, opts *ImportOptions) {
		t.Helper()
		opts.Me = "7000000000000888888"
	})
	var fromMe int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE message_type = 'telegram' AND is_from_me`,
	).Scan(&fromMe))
	assert.Equal(2, fromMe, "--me override drives is_from_me")
	assert.Equal(1, summary.ChatsProcessed)
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
	appendMessage(t, second, "7000000000000555555", 105, "fresh message from a newer export")

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

	storedPath, storedHash := attachmentRow(t, st, "7000000000000555555:103")
	require.NotEmpty(storedPath, "photo bytes stored on first import")

	// A newer export drops the photo, rewrites the body, and turns the
	// message into a reply to one that did not exist before.
	setChatMessages(t, dir, "7000000000000555555", []map[string]any{
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
		WHERE m.source_message_id = '7000000000000555555:103'`).Scan(&body))
	assert.False(body.Valid, "an empty archived body stays empty")

	path, hash := attachmentRow(t, st, "7000000000000555555:103")
	assert.Equal(storedPath, path, "removed media keeps its stored row and bytes")
	assert.Equal(storedHash, hash)

	var replyTo *int64
	require.NoError(st.DB().QueryRow(`
		SELECT reply_to_message_id FROM messages
		WHERE source_message_id = '7000000000000555555:103'`).Scan(&replyTo))
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
// export determines: in a personal or bot chat the message's to recipient is
// the chat's other participant, owner and peer in both directions.
func TestImportDirectChatDerivesToRecipients(t *testing.T) {
	for _, chatType := range []string{"personal_chat", "bot_chat"} {
		t.Run(chatType, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			assert := assert.New(t)
			require := require.New(t)

			peer := map[string]any{
				"id": 1, "type": "message", "date_unixtime": "1610909800",
				"from": "Alice Example", "from_id": "user7000000000000555555", "text": "from alice",
			}
			owner := maps.Clone(peer)
			owner["id"] = 2
			owner["from"] = "Test Owner"
			owner["from_id"] = "user7000000000000123456"
			owner["text"] = "from owner"
			root := writeChatExport(t, t.TempDir(), fixturePersonalChatID, chatType, "Alice Example",
				[]map[string]any{peer, owner})
			importRoot(t, st, root)

			ownerPID, _, err := st.ParticipantByIdentifier("telegram", "7000000000000123456")
			require.NoError(err)
			peerPID, _, err := st.ParticipantByIdentifier("telegram", "7000000000000555555")
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
			assert.Equal(ownerPID, toRecipient("7000000000000555555:1"), "peer's message is to the owner")
			assert.Equal(peerPID, toRecipient("7000000000000555555:2"), "owner's message is to the peer")
		})
	}
}

// TestImportGroupChatsHaveNoToRecipients pins the other half: group and
// channel messages stay sender-only, with no synthetic "to" recipients.
func TestImportGroupChatsHaveNoToRecipients(t *testing.T) {
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
	root := writeChatExport(t, t.TempDir(), fixtureGroupChatID, "private_group", "A Group",
		[]map[string]any{alice, owner})
	importRoot(t, st, root)

	var toCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_recipients mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_message_id IN ('-1009999999999:1', '-1009999999999:2')
		  AND mr.recipient_type = 'to'`)).Scan(&toCount))
	assert.Zero(toCount, "group messages carry no to recipients")
}

// TestImportDirectChatOutgoingOnlyDerivesPeerRecipient pins the peer
// derivation's stable source: in a personal or bot chat the export's chat
// id is the peer's own user id, so an export whose messages are all the
// owner's still records the peer as the "to" recipient.
func TestImportDirectChatOutgoingOnlyDerivesPeerRecipient(t *testing.T) {
	for _, chatType := range []string{"personal_chat", "bot_chat"} {
		t.Run(chatType, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			assert := assert.New(t)
			require := require.New(t)

			root := writeChatExport(t, t.TempDir(), fixturePersonalChatID, chatType, "Peer Name", []map[string]any{
				{
					"id": 1, "type": "message", "date_unixtime": "1610909800",
					"from": "Test Owner", "from_id": "user7000000000000123456",
					"text": "outgoing only",
				},
			})
			importRoot(t, st, root)

			peerPID, _, err := st.ParticipantByIdentifier("telegram", "7000000000000555555")
			require.NoError(err)
			var toPID int64
			require.NoError(st.DB().QueryRow(st.Rebind(`
				SELECT mr.participant_id FROM message_recipients mr
				JOIN messages m ON m.id = mr.message_id
				WHERE m.source_message_id = '7000000000000555555:1'
				  AND mr.recipient_type = 'to'`)).Scan(&toPID))
			assert.Equal(peerPID, toPID, "the chat id derives the peer without any peer message")
		})
	}
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
	first := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Reply Chat",
		[]map[string]any{parent})
	importRoot(t, st, first)

	reply := maps.Clone(parent)
	reply["id"] = 2
	reply["text"] = "a reply"
	reply["reply_to_message_id"] = 1
	second := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Reply Chat",
		[]map[string]any{reply})
	importRoot(t, st, second)

	var parentSrc string
	require.NoError(st.DB().QueryRow(`
		SELECT p.source_message_id FROM messages c
		JOIN messages p ON p.id = c.reply_to_message_id
		WHERE c.source_message_id = '7000000000000555555:2'`).Scan(&parentSrc))
	assert.Equal("7000000000000555555:1", parentSrc, "reply links to the earlier-export parent")
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
	root := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Forward Chat",
		[]map[string]any{reply, parent})
	importRoot(t, st, root)

	var linked bool
	require.NoError(st.DB().QueryRow(`
		SELECT reply_to_message_id IS NOT NULL FROM messages
		WHERE source_message_id = '7000000000000555555:1'`).Scan(&linked))
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
	root := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Edited Chat",
		[]map[string]any{plain, edited})
	importRoot(t, st, root)

	flagged := func() []bool {
		var flags []bool
		rows, err := st.DB().Query(`
			SELECT is_edited FROM messages
		WHERE source_message_id IN ('7000000000000555555:1', '7000000000000555555:2')
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
	setChatMessages(t, root, "7000000000000555555", []map[string]any{plain, edited})
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
	root := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Docs Chat", []map[string]any{{
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
		WHERE m.source_message_id = '7000000000000555555:1'`).Scan(&filename, &mimeType))
	assert.Equal("Quarterly Report.pdf", filename)
	assert.Equal("application/pdf", mimeType)
}

// TestImportUnavailableMediaWarningCreatesNoAttachment pins the
// source-warning rule: Telegram Desktop replaces a media path with
// parenthesized warning text when export size limits or media-type
// exclusions skipped the bytes, and that text must not become an
// attachment row or set the message's attachment flags.
func TestImportUnavailableMediaWarningCreatesNoAttachment(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)
	root := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Media Chat", []map[string]any{
		{
			"id": 1, "type": "message", "date_unixtime": "1610909800",
			"from": "Alice Example", "from_id": "user7000000000000555555",
			"photo": "(Photo unavailable, please try again later)", "text": "",
		},
		{
			"id": 2, "type": "message", "date_unixtime": "1610909810",
			"from": "Alice Example", "from_id": "user7000000000000555555",
			"file": "report.pdf (File not included. Change data exporting settings to download.)",
			"text": "",
		},
		{
			"id": 3, "type": "message", "date_unixtime": "1610909820",
			"from": "Alice Example", "from_id": "user7000000000000555555",
			"thumbnail": "(File exceeds maximum size. Change data exporting settings to download.)",
			"text":      "",
		},
	})
	importRoot(t, st, root)

	var rows int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*) FROM attachments a
		JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id LIKE '7000000000000555555:%'`).Scan(&rows))
	assert.Zero(rows, "warning text never becomes an attachment row")
	var flagged int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*) FROM messages
		WHERE source_message_id LIKE '7000000000000555555:%'
		  AND (has_attachments OR attachment_count > 0)`).Scan(&flagged))
	assert.Zero(flagged, "warning text leaves attachment flags unset")
}

// TestImportSnippetTakesFirstLine pins the shared preview behavior: the
// snippet is the body's first line, exactly like every other importer's
// textutil.SnippetFromBody rendering.
func TestImportSnippetTakesFirstLine(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Snippet Chat", []map[string]any{{
		"id": 1, "type": "message", "date_unixtime": "1610909800",
		"from": "Alice Example", "from_id": "user7000000000000555555",
		"text": "first preview line\nsecond body line",
	}})
	importRoot(t, st, root)

	var snippet sql.NullString
	require.NoError(t, st.DB().QueryRow(`
		SELECT snippet FROM messages WHERE source_message_id = '7000000000000555555:1'`).Scan(&snippet))
	assert.True(t, snippet.Valid, "a text body produces a snippet")
	assert.Equal(t, "first preview line", snippet.String)
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

// failAttachmentInserts installs a database trigger that fails every
// attachment insert, so a run commits its messages atomically and then
// fails a deferred attachment write. The returned func removes it.
func failAttachmentInserts(t *testing.T, st *store.Store) func() {
	t.Helper()
	create := `CREATE TRIGGER fail_attachment_inserts BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT, 'injected attachment failure'); END`
	drop := `DROP TRIGGER IF EXISTS fail_attachment_inserts`
	if st.IsPostgreSQL() {
		create = `CREATE FUNCTION fail_attachment_inserts() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected attachment failure'; END; $$ LANGUAGE plpgsql;
CREATE TRIGGER fail_attachment_inserts BEFORE INSERT ON attachments FOR EACH ROW EXECUTE FUNCTION fail_attachment_inserts()`
		drop = `DROP TRIGGER IF EXISTS fail_attachment_inserts ON attachments; DROP FUNCTION IF EXISTS fail_attachment_inserts()`
	}
	_, err := st.DB().Exec(create)
	require.NoError(t, err)
	return func() {
		_, err := st.DB().Exec(drop)
		require.NoError(t, err)
	}
}

// failReplyLinks installs a database trigger that fails every non-NULL
// reply_to_message_id write, so a run commits its messages atomically and
// then fails the post-chat reply pass. The returned func removes it.
func failReplyLinks(t *testing.T, st *store.Store) func() {
	t.Helper()
	create := `CREATE TRIGGER fail_reply_links BEFORE UPDATE OF reply_to_message_id ON messages WHEN NEW.reply_to_message_id IS NOT NULL BEGIN SELECT RAISE(ABORT, 'injected reply failure'); END`
	drop := `DROP TRIGGER IF EXISTS fail_reply_links`
	if st.IsPostgreSQL() {
		create = `CREATE FUNCTION fail_reply_links() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected reply failure'; END; $$ LANGUAGE plpgsql;
CREATE TRIGGER fail_reply_links BEFORE UPDATE OF reply_to_message_id ON messages FOR EACH ROW WHEN (NEW.reply_to_message_id IS NOT NULL) EXECUTE FUNCTION fail_reply_links()`
		drop = `DROP TRIGGER IF EXISTS fail_reply_links ON messages; DROP FUNCTION IF EXISTS fail_reply_links()`
	}
	_, err := st.DB().Exec(create)
	require.NoError(t, err)
	return func() {
		_, err := st.DB().Exec(drop)
		require.NoError(t, err)
	}
}

// TestImportLimitThenRerunRestoresReplyLinks re-pins the round-4 reply
// convergence regression at the boundary the atomic design has: a --limit
// cut commits a reply whose parent is not yet archived, leaves the chat
// incomplete, and the re-run's post-chat reply pass fills the skipped
// message's NULL link.
func TestImportLimitThenRerunRestoresReplyLinks(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)

	// The reply comes first, so the limited run archives the child alone.
	root := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Reply Chat", []map[string]any{
		{
			"id": 1, "type": "message", "date_unixtime": "1610909800",
			"from": "Alice Example", "from_id": "user7000000000000555555",
			"text": "a reply", "reply_to_message_id": 2,
		},
		{
			"id": 2, "type": "message", "date_unixtime": "1610909810",
			"from": "Alice Example", "from_id": "user7000000000000555555",
			"text": "the parent",
		},
	})
	limited, err := Import(context.Background(), st, ImportOptions{
		RootDir: root, AttachmentsDir: t.TempDir(), Limit: 1,
	})
	require.NoError(err)
	require.EqualValues(1, limited.MessagesAdded)

	var linked bool
	require.NoError(st.DB().QueryRow(`
		SELECT reply_to_message_id IS NOT NULL FROM messages
		WHERE source_message_id = '7000000000000555555:1'`).Scan(&linked))
	assert.False(linked, "the limited run leaves the reply unlinked")

	importRoot(t, st, root)
	require.NoError(st.DB().QueryRow(`
		SELECT reply_to_message_id IS NOT NULL FROM messages
		WHERE source_message_id = '7000000000000555555:1'`).Scan(&linked))
	assert.True(linked, "the re-run links the skipped reply to its parent")
}

// TestImportRetriesChatAfterDeferredAttachmentFailure re-pins the torn-write
// repair regressions at the boundary the atomic design has: a deferred
// attachment write fails after the messages committed, the chat is never
// marked complete, and the resumed run reprocesses it whole — committed
// messages skip, their media is re-ensured, and the chat finishes.
func TestImportRetriesChatAfterDeferredAttachmentFailure(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)

	root := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Media Chat", []map[string]any{
		{
			"id": 1, "type": "message", "date_unixtime": "1610909800",
			"from": "Alice Example", "from_id": "user7000000000000555555",
			"photo": "photos/photo_1.jpg", "text": "",
		},
	})
	require.NoError(os.MkdirAll(filepath.Join(root, "photos"), 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(root, "photos", "photo_1.jpg"), []byte("photo-bytes"), 0o644))

	remove := failAttachmentInserts(t, st)
	_, err := Import(context.Background(), st, ImportOptions{
		RootDir: root, AttachmentsDir: t.TempDir(),
	})
	require.Error(err, "the deferred attachment failure fails the run")
	assert.Contains(err.Error(), "store attachments")

	var messages int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE message_type = 'telegram'`).Scan(&messages))
	assert.Equal(1, messages, "the message itself committed atomically")

	remove()
	failSyncAt(t, st, root) // the failed run left the chat incomplete
	summary := importRoot(t, st, root)
	assert.True(summary.WasResumed, "the failed run's checkpoint resumes")
	assert.EqualValues(1, summary.MessagesSkipped, "the committed message is skipped")
	path, hash := attachmentRow(t, st, "7000000000000555555:1")
	assert.NotEmpty(path, "the retried chat stores the deferred attachment")
	assert.NotEmpty(hash)
}

// TestImportRetriesChatAfterDeferredReplyFailure pins the same boundary for
// the reply pass: a deferred reply-link write fails after the messages
// committed, the chat stays incomplete, and the next run's reply pass fills
// the still-NULL links.
func TestImportRetriesChatAfterDeferredReplyFailure(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)

	root := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Reply Chat", []map[string]any{
		{
			"id": 1, "type": "message", "date_unixtime": "1610909800",
			"from": "Alice Example", "from_id": "user7000000000000555555",
			"text": "a reply", "reply_to_message_id": 2,
		},
		{
			"id": 2, "type": "message", "date_unixtime": "1610909810",
			"from": "Alice Example", "from_id": "user7000000000000555555",
			"text": "the parent",
		},
	})

	remove := failReplyLinks(t, st)
	_, err := Import(context.Background(), st, ImportOptions{
		RootDir: root, AttachmentsDir: t.TempDir(),
	})
	require.Error(err, "the deferred reply failure fails the run")
	assert.Contains(err.Error(), "link reply")

	var messages int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE message_type = 'telegram'`).Scan(&messages))
	assert.Equal(2, messages, "both messages committed atomically")

	remove()
	failSyncAt(t, st, root) // the failed run left the chat incomplete
	importRoot(t, st, root)

	var parentSrc string
	require.NoError(st.DB().QueryRow(`
		SELECT p.source_message_id FROM messages c
		JOIN messages p ON p.id = c.reply_to_message_id
		WHERE c.source_message_id = '7000000000000555555:1'`).Scan(&parentSrc))
	assert.Equal("7000000000000555555:2", parentSrc, "the retried pass links the reply")
}

// TestImportRetryWithoutCheckpointRestoresAttachment pins that attachment
// recovery does not depend on checkpoint state: a run that failed after
// the message committed but before any checkpoint — the chat never
// completed and the mid-chat interval never fired — must still have its
// deferred attachment restored by the next ordinary, non-resumed run.
func TestImportRetryWithoutCheckpointRestoresAttachment(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)

	root := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Media Chat", []map[string]any{
		{
			"id": 1, "type": "message", "date_unixtime": "1610909800",
			"from": "Alice Example", "from_id": "user7000000000000555555",
			"photo": "photos/photo_1.jpg", "text": "",
		},
	})
	require.NoError(os.MkdirAll(filepath.Join(root, "photos"), 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(root, "photos", "photo_1.jpg"), []byte("photo-bytes"), 0o644))

	remove := failAttachmentInserts(t, st)
	_, err := Import(context.Background(), st, ImportOptions{
		RootDir: root, AttachmentsDir: t.TempDir(),
	})
	require.Error(err, "the deferred attachment failure fails the run")
	remove()

	// No failSyncAt: the failed run left no checkpoint, so the retry is
	// an ordinary fresh scan, not a resume.
	summary := importRoot(t, st, root)
	assert.False(summary.WasResumed, "no checkpoint exists to resume")
	assert.EqualValues(1, summary.MessagesSkipped, "the committed message is skipped")
	path, hash := attachmentRow(t, st, "7000000000000555555:1")
	assert.NotEmpty(path, "the ordinary re-run restores the deferred attachment")
	assert.NotEmpty(hash)
}

// failSenderResolution installs a database trigger that fails the
// participant-identifier insert for one Telegram user, so sender
// resolution for that user's messages errors. The returned func removes
// it.
func failSenderResolution(t *testing.T, st *store.Store, identifier string) func() {
	t.Helper()
	create := `CREATE TRIGGER fail_sender_resolution BEFORE INSERT ON participant_identifiers
WHEN NEW.identifier_value = '` + identifier + `'
BEGIN SELECT RAISE(ABORT, 'injected sender failure'); END`
	drop := `DROP TRIGGER IF EXISTS fail_sender_resolution`
	if st.IsPostgreSQL() {
		create = `CREATE FUNCTION fail_sender_resolution() RETURNS trigger AS $$
BEGIN
	IF NEW.identifier_value = '` + identifier + `' THEN
		RAISE EXCEPTION 'injected sender failure';
	END IF;
	RETURN NEW;
END; $$ LANGUAGE plpgsql;
CREATE TRIGGER fail_sender_resolution BEFORE INSERT ON participant_identifiers
FOR EACH ROW EXECUTE FUNCTION fail_sender_resolution()`
		drop = `DROP TRIGGER IF EXISTS fail_sender_resolution ON participant_identifiers; DROP FUNCTION IF EXISTS fail_sender_resolution()`
	}
	_, err := st.DB().Exec(create)
	require.NoError(t, err)
	return func() {
		_, err := st.DB().Exec(drop)
		require.NoError(t, err)
	}
}

// TestImportSenderFailureIsNotPermanent pins that a recoverable sender
// resolution error is never turned into a permanent NULL sender: the chat
// aborts before the message commits, so the retry resolves the sender and
// archives the message whole.
func TestImportSenderFailureIsNotPermanent(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)

	root := writeChatExport(t, t.TempDir(), fixtureGroupChatID, "private_group", "Sender Group", []map[string]any{
		{
			"id": 1, "type": "message", "date_unixtime": "1610909800",
			"from": "Alice Example", "from_id": "user7000000000000555555",
			"text": "from alice",
		},
	})

	remove := failSenderResolution(t, st, "7000000000000555555")
	_, err := Import(context.Background(), st, ImportOptions{
		RootDir: root, AttachmentsDir: t.TempDir(),
	})
	require.Error(err, "a sender resolution failure fails the chat")
	assert.Contains(err.Error(), "resolve sender")
	var committed int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE message_type = 'telegram'`).Scan(&committed))
	assert.Zero(committed, "no message commits with an unresolved sender")

	remove()
	importRoot(t, st, root)

	alicePID, _, err := st.ParticipantByIdentifier("telegram", "7000000000000555555")
	require.NoError(err)
	var sender sql.NullInt64
	require.NoError(st.DB().QueryRow(`
		SELECT sender_id FROM messages WHERE source_message_id = '-1009999999999:1'`).Scan(&sender))
	assert.True(sender.Valid, "the retried chat archives the message with its sender")
	assert.Equal(alicePID, sender.Int64)
	var fromPID int64
	require.NoError(st.DB().QueryRow(`
		SELECT mr.participant_id FROM message_recipients mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_message_id = '-1009999999999:1'
		  AND mr.recipient_type = 'from'`).Scan(&fromPID))
	assert.Equal(alicePID, fromPID, "the retried message records its from recipient")
}

// failCheckpointWrites installs a database trigger that fails checkpoint
// persistence only — sync_runs updates that write a new cursor_before —
// while the sync-generation fence, completion, and failure updates keep
// succeeding. The returned func removes it.
func failCheckpointWrites(t *testing.T, st *store.Store) func() {
	t.Helper()
	create := `CREATE TRIGGER fail_checkpoint_writes BEFORE UPDATE ON sync_runs
WHEN NEW.cursor_before IS NOT NULL AND NEW.cursor_before != COALESCE(OLD.cursor_before, '')
BEGIN SELECT RAISE(ABORT, 'injected checkpoint failure'); END`
	drop := `DROP TRIGGER IF EXISTS fail_checkpoint_writes`
	if st.IsPostgreSQL() {
		create = `CREATE FUNCTION fail_checkpoint_writes() RETURNS trigger AS $$
BEGIN
	IF NEW.cursor_before IS DISTINCT FROM OLD.cursor_before THEN
		RAISE EXCEPTION 'injected checkpoint failure';
	END IF;
	RETURN NEW;
END; $$ LANGUAGE plpgsql;
CREATE TRIGGER fail_checkpoint_writes BEFORE UPDATE ON sync_runs
FOR EACH ROW EXECUTE FUNCTION fail_checkpoint_writes()`
		drop = `DROP TRIGGER IF EXISTS fail_checkpoint_writes ON sync_runs; DROP FUNCTION IF EXISTS fail_checkpoint_writes()`
	}
	_, err := st.DB().Exec(create)
	require.NoError(t, err)
	return func() {
		_, err := st.DB().Exec(drop)
		require.NoError(t, err)
	}
}

// TestImportCheckpointFailureFailsRun pins that checkpoint-write errors
// reach the caller: a run whose progress could not persist must fail —
// at the mid-chat interval and after a chat completes — instead of
// leaving a silently unresumable import.
func TestImportCheckpointFailureFailsRun(t *testing.T) {
	messages := []map[string]any{
		{
			"id": 1, "type": "message", "date_unixtime": "1610909800",
			"from": "Alice Example", "from_id": "user7000000000000555555", "text": "one",
		},
		{
			"id": 2, "type": "message", "date_unixtime": "1610909810",
			"from": "Alice Example", "from_id": "user7000000000000555555", "text": "two",
		},
	}
	for _, tc := range []struct {
		name    string
		options func(*ImportOptions)
	}{
		{name: "mid-chat interval", options: func(o *ImportOptions) { o.CheckpointEvery = 1 }},
		{name: "after chat completes", options: func(o *ImportOptions) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			root := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Chat", messages)

			remove := failCheckpointWrites(t, st)
			opts := ImportOptions{RootDir: root, AttachmentsDir: t.TempDir()}
			tc.options(&opts)
			_, err := Import(context.Background(), st, opts)
			remove()
			require.Error(err, "a checkpoint that did not persist fails the run")
			assert.Contains(err.Error(), "checkpoint")

			// With the trigger gone the same export imports clean.
			summary := importRoot(t, st, root)
			var count int
			require.NoError(st.DB().QueryRow(
				`SELECT COUNT(*) FROM messages WHERE message_type = 'telegram'`).Scan(&count))
			assert.Equal(2, count)
			assert.False(summary.WasResumed, "no checkpoint was ever persisted")
		})
	}
}

// failMessagePersist installs a database trigger that fails the atomic
// persist of one message — the observable effect cancellation inside the
// message transaction has: the transaction rolls back and the importer
// hears the error. The returned func removes it.
func failMessagePersist(t *testing.T, st *store.Store, sourceMessageID string) func() {
	t.Helper()
	create := `CREATE TRIGGER fail_message_persist BEFORE INSERT ON messages
WHEN NEW.source_message_id = '` + sourceMessageID + `'
BEGIN SELECT RAISE(ABORT, 'injected persist failure'); END`
	drop := `DROP TRIGGER IF EXISTS fail_message_persist`
	if st.IsPostgreSQL() {
		create = `CREATE FUNCTION fail_message_persist() RETURNS trigger AS $$
BEGIN
	IF NEW.source_message_id = '` + sourceMessageID + `' THEN
		RAISE EXCEPTION 'injected persist failure';
	END IF;
	RETURN NEW;
END; $$ LANGUAGE plpgsql;
CREATE TRIGGER fail_message_persist BEFORE INSERT ON messages
FOR EACH ROW EXECUTE FUNCTION fail_message_persist()`
		drop = `DROP TRIGGER IF EXISTS fail_message_persist ON messages; DROP FUNCTION IF EXISTS fail_message_persist()`
	}
	_, err := st.DB().Exec(create)
	require.NoError(t, err)
	return func() {
		_, err := st.DB().Exec(drop)
		require.NoError(t, err)
	}
}

// TestImportPersistFailureKeepsChatRetryable pins the failure semantics of
// the message transaction itself: a persist error that rolls the message
// back fails the chat instead of being counted past, so the chat is never
// checkpointed complete and a retried run imports the message the failed
// transaction lost.
func TestImportPersistFailureKeepsChatRetryable(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)

	// The personal chat's final message fails to persist; the malformed
	// chat behind it then kills the run — after the swallowed error would
	// have checkpointed the chat complete.
	root := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Persist Chat", []map[string]any{
		{
			"id": 1, "type": "message", "date_unixtime": "1610909800",
			"from": "Alice Example", "from_id": "user7000000000000555555", "text": "committed",
		},
		{
			"id": 2, "type": "message", "date_unixtime": "1610909810",
			"from": "Alice Example", "from_id": "user7000000000000555555",
			"text": "lost to the failed transaction",
		},
	})
	doc := loadFullExport(t, root)
	doc.Chats.List = append(doc.Chats.List, fullExportChat{
		ID: -1002222222222, Type: "personal_chat", Name: "Broken Chat",
		Messages: []map[string]any{{
			"id": 1, "type": "message", "date_unixtime": "not-a-timestamp", "text": "broken",
		}},
	})
	saveFullExport(t, root, doc)

	remove := failMessagePersist(t, st, "7000000000000555555:2")
	_, err := Import(context.Background(), st, ImportOptions{
		RootDir: root, AttachmentsDir: t.TempDir(),
	})
	remove()
	require.Error(err, "a message persist failure fails the chat")
	assert.Contains(err.Error(), "persist message")

	// The retried run reprocesses the chat whole: the rolled-back message
	// was never archived, so a resume that skipped the chat would lose it.
	_, err = Import(context.Background(), st, ImportOptions{
		RootDir: root, AttachmentsDir: t.TempDir(),
	})
	require.Error(err, "the malformed chat still fails the retried run")
	assert.Contains(err.Error(), "parse date_unixtime")
	var count int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE source_message_id LIKE '7000000000000555555:%'`).Scan(&count))
	assert.Equal(2, count, "the retried run imports the rolled-back message")
}

// TestImportOverlappingExportKeepsArchivedAttachmentBytes pins the
// append-only recovery contract for media: an overlapping export whose file
// at the same export-relative path carries different bytes must not replace
// the stored bytes, hash, or row of an archived message's attachment.
func TestImportOverlappingExportKeepsArchivedAttachmentBytes(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)

	chat := []map[string]any{{
		"id": 1, "type": "message", "date_unixtime": "1610909800",
		"from": "Alice Example", "from_id": "user7000000000000555555",
		"photo": "photos/photo_1.jpg", "text": "",
	}}
	first := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Media Chat", chat)
	require.NoError(os.MkdirAll(filepath.Join(first, "photos"), 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(first, "photos", "photo_1.jpg"), []byte("original-bytes"), 0o644))
	importRoot(t, st, first)

	var rowID int64
	var path, hash string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT a.id, a.storage_path, a.content_hash
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = ?`), "7000000000000555555:1").Scan(&rowID, &path, &hash))
	require.NotEmpty(path, "the first import stores the photo")
	require.NotEmpty(hash)

	// A newer export from a different directory carries the same message
	// with changed bytes at the same export-relative path.
	second := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Media Chat", chat)
	require.NoError(os.MkdirAll(filepath.Join(second, "photos"), 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(second, "photos", "photo_1.jpg"), []byte("changed-bytes"), 0o644))
	importRoot(t, st, second)

	var afterID int64
	var afterPath, afterHash string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT a.id, a.storage_path, a.content_hash
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = ?`), "7000000000000555555:1").Scan(&afterID, &afterPath, &afterHash))
	assert.Equal(rowID, afterID, "the archived occurrence keeps its row")
	assert.Equal(path, afterPath, "changed bytes never replace the stored file")
	assert.Equal(hash, afterHash, "changed bytes never replace the stored hash")
}

// TestImportAudioTitleDoesNotReplaceFilename pins the source-field
// semantics: Telegram documents a file's title as the music title, so it
// must not become the attachment's storage filename. The export's
// file_name wins when present; otherwise the storage basename keeps the
// extension.
func TestImportAudioTitleDoesNotReplaceFilename(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)
	root := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Music Chat", []map[string]any{
		{
			"id": 1, "type": "message", "date_unixtime": "1610909800",
			"from": "Alice Example", "from_id": "user7000000000000555555",
			"file": "music/song_1.mp3", "media_type": "audio_file",
			"title": "A Song Title", "performer": "An Artist", "text": "",
		},
		{
			"id": 2, "type": "message", "date_unixtime": "1610909810",
			"from": "Alice Example", "from_id": "user7000000000000555555",
			"file": "music/song_2.mp3", "file_name": "Shared Recording.mp3",
			"media_type": "audio_file", "title": "Another Title", "text": "",
		},
	})
	require.NoError(os.MkdirAll(filepath.Join(root, "music"), 0o755))
	require.NoError(os.WriteFile(filepath.Join(root, "music", "song_1.mp3"), []byte("one"), 0o644))
	require.NoError(os.WriteFile(filepath.Join(root, "music", "song_2.mp3"), []byte("two"), 0o644))
	importRoot(t, st, root)

	filenameFor := func(sourceMessageID string) string {
		var filename string
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT a.filename FROM attachments a
			JOIN messages m ON m.id = a.message_id
			WHERE m.source_message_id = ?`), sourceMessageID).Scan(&filename))
		return filename
	}
	assert.Equal("song_1.mp3", filenameFor("7000000000000555555:1"),
		"the storage basename keeps the extension when file_name is absent")
	assert.Equal("Shared Recording.mp3", filenameFor("7000000000000555555:2"),
		"the export's file_name stays authoritative")
}

// TestImportMalformedPersonalChatErrors pins the chat-id peer derivation
// exactly: a personal or bot chat whose id is not a peer user id is
// malformed and fails the chat, rather than guessing a peer from message
// senders.
func TestImportMalformedPersonalChatErrors(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)
	root := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Malformed Chat", []map[string]any{
		{
			"id": 1, "type": "message", "date_unixtime": "1610909800",
			"from": "Alice Example", "from_id": "user7000000000000555555", "text": "hello",
		},
	})
	doc := loadFullExport(t, root)
	doc.Chats.List[0].ID = -1001234567890
	saveFullExport(t, root, doc)

	_, err := Import(context.Background(), st, ImportOptions{
		RootDir: root, AttachmentsDir: t.TempDir(),
	})
	require.Error(err, "a personal chat without a peer user id is malformed")
	assert.Contains(err.Error(), "carries no peer user id")

	var count int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE message_type = 'telegram'`).Scan(&count))
	assert.Zero(count, "the malformed chat commits nothing")
}

// TestImportErrorRefreshesCompletedChatStats pins derived state on the
// error path: a chat committed earlier in the run carries fresh stats even
// when a later malformed chat fails the whole run.
func TestImportErrorRefreshesCompletedChatStats(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := writeChatExport(t, t.TempDir(), fixturePersonalChatID, "personal_chat", "Stats Chat", []map[string]any{{
		"id": 1, "type": "message", "date_unixtime": "1610909800",
		"from": "Alice Example", "from_id": "user7000000000000555555",
		"text": "committed before the failure",
	}})
	doc := loadFullExport(t, root)
	doc.Chats.List = append(doc.Chats.List, fullExportChat{
		ID: -1002222222222, Type: "private_group", Name: "Broken Chat",
		Messages: []map[string]any{{
			"id": 1, "type": "message", "date_unixtime": "not-a-timestamp", "text": "broken",
		}},
	})
	saveFullExport(t, root, doc)

	_, err := Import(context.Background(), st, ImportOptions{
		RootDir: root, AttachmentsDir: t.TempDir(),
	})
	require.Error(t, err, "the malformed chat fails the run")

	var count int
	require.NoError(t, st.DB().QueryRow(`
		SELECT message_count FROM conversations c
		JOIN sources s ON s.id = c.source_id
		WHERE s.source_type = 'telegram' AND c.source_conversation_id = '7000000000000555555'`).Scan(&count))
	assert.Equal(t, 1, count, "the committed chat's stats refresh on the error path")
}

// TestImportCancellationRefreshesCompletedChatStats pins the same on the
// cancellation path: a cancelled run still refreshes stats for chats the
// archive already carries.
func TestImportCancellationRefreshesCompletedChatStats(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	copyFullFixture(t, dir)

	// A run archived the first chat's messages but was cancelled before it
	// could refresh derived state, leaving the counter stale.
	limited, err := Import(context.Background(), st, ImportOptions{
		RootDir: dir, AttachmentsDir: t.TempDir(), Limit: 3,
	})
	require.NoError(err)
	require.EqualValues(3, limited.MessagesAdded)
	_, err = st.DB().Exec(`
		UPDATE conversations SET message_count = 0 WHERE source_conversation_id = '7000000000000555555'`)
	require.NoError(err)
	failSyncAt(t, st, dir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Import(ctx, st, ImportOptions{RootDir: dir, AttachmentsDir: t.TempDir()})
	require.ErrorIs(err, context.Canceled)

	var count int
	require.NoError(st.DB().QueryRow(`
		SELECT message_count FROM conversations c
		JOIN sources s ON s.id = c.source_id
		WHERE s.source_type = 'telegram' AND c.source_conversation_id = '7000000000000555555'`).Scan(&count))
	assert.Equal(3, count, "the cancellation path still refreshes committed chats' stats")
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
	appendMessage(t, dir, "7000000000000555555", 105, "fresh content in a completed chat")

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
	root := writeChatExport(t, dir, fixturePersonalChatID, "personal_chat", "Media Chat",
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
	stickerRoles := roleFor("7000000000000555555:1")
	assert.Len(stickerRoles, 1, "the sticker message has one attachment")
	assert.NotEmpty(stickerRoles["sticker"], "sticker carries the sticker role with stored bytes")

	photoRoles := roleFor("7000000000000555555:2")
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
	root := writeChatExport(t, dir, fixturePersonalChatID, "personal_chat", "Symlink Chat", []map[string]any{{
		"id": 1, "type": "message", "date_unixtime": "1610909800",
		"from": "Alice Example", "from_id": "user7000000000000555555",
		"photo": "photos/escape.jpg", "text": "",
	}})
	require.NoError(os.MkdirAll(filepath.Join(root, "photos"), 0o755))
	outside := filepath.Join(dir, "outside.jpg")
	require.NoError(os.WriteFile(outside, []byte("outside-bytes"), 0o644))
	require.NoError(os.Symlink(outside, filepath.Join(root, "photos", "escape.jpg")))
	importRoot(t, st, root)

	path, hash := attachmentRow(t, st, "7000000000000555555:1")
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
	path, hash = attachmentRow(t, st, "7000000000000555555:2")
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
	require.Failf(t, "setChatMessages: chat not found", "chat %s not found in %s", chatID, root)
}

// writeChatExport writes a full-shaped export whose single chat — chatID in
// Telegram's ID space: the peer's own user id for direct chats, negative for
// groups and channels — carries the given message objects, and returns its
// root directory. The owner is the fixture owner, so no --me override is
// needed.
func writeChatExport(t *testing.T, dir string, chatID int64, chatType, chatName string, messages []map[string]any) string {
	t.Helper()
	doc := fullExportDoc{
		PersonalInformation: jsontext.Value(`{"user_id":7000000000000123456,"first_name":"Test","last_name":"Owner"}`),
	}
	doc.Chats.List = []fullExportChat{{
		ID: chatID, Type: chatType, Name: chatName, Messages: messages,
	}}
	b, err := json.Marshal(doc)
	require.NoError(t, err)
	root := filepath.Join(dir, "export")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "result.json"), b, 0o644))
	return root
}
