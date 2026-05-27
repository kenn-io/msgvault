package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

// panicOnBatchAPI wraps a MockAPI and panics when GetMessagesRawBatch is called.
// Used to test that Full() recovers from panics gracefully.
type panicOnBatchAPI struct {
	*gmail.MockAPI
}

func (p *panicOnBatchAPI) GetMessagesRawBatch(_ context.Context, _ []string) ([]*gmail.RawMessage, error) {
	panic("unexpected nil pointer in batch processing")
}

func TestFullSync_PanicReturnsError(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 1, 12345, "msg1")

	// Replace the client with one that panics during batch fetch
	env.Syncer = New(&panicOnBatchAPI{MockAPI: env.Mock}, env.Store, nil)

	// Should return an error, NOT panic and crash the program
	_, err := env.Syncer.Full(env.Context, testEmail)
	requirepkg.Error(t, err, "expected error from panic recovery")
	assertpkg.ErrorContains(t, err, "panic")
}

// panicOnHistoryAPI wraps a MockAPI and panics when ListHistory is called.
// Used to test that Incremental() recovers from panics gracefully.
type panicOnHistoryAPI struct {
	*gmail.MockAPI
}

func (p *panicOnHistoryAPI) ListHistory(_ context.Context, _ uint64, _ string) (*gmail.HistoryResponse, error) {
	panic("unexpected nil pointer in history processing")
}

func TestIncrementalSync_PanicReturnsError(t *testing.T) {
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "12340")

	env.Mock.Profile.MessagesTotal = 10
	env.Mock.Profile.HistoryID = 12350

	// Replace the client with one that panics during history fetch
	env.Syncer = New(&panicOnHistoryAPI{MockAPI: env.Mock}, env.Store, nil)

	// Should return an error, NOT panic and crash the program
	_, err := env.Syncer.Incremental(env.Context, source)
	requirepkg.Error(t, err, "expected error from panic recovery")
	assertpkg.ErrorContains(t, err, "panic")
}

func TestFullSync(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 3, 12345, "msg1", "msg2", "msg3")
	env.Mock.Messages["msg2"].LabelIDs = []string{"INBOX", "SENT"}
	env.Mock.Messages["msg3"].LabelIDs = []string{"SENT"}

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(3)), Errors: new(int64(0))})
	assertpkg.Equal(t, uint64(12345), summary.FinalHistoryID, "history ID")

	assertMockCalls(t, env, 1, 1, 3)
	assertMessageCount(t, env.Store, 3)
}

func TestFullSyncResume(t *testing.T) {
	env := newTestEnv(t)

	// Create mock with pagination
	env.Mock.Profile.HistoryID = 12345
	seedPagedMessages(env, 4, 2, "msg")

	summary1 := runFullSync(t, env)
	assertSummary(t, summary1, WantSummary{Added: new(int64(4))})

	// Second sync should skip already-synced messages
	env.Mock.Reset()
	env.Mock.Profile = &gmail.Profile{
		EmailAddress:  testEmail,
		MessagesTotal: 4,
		HistoryID:     12346,
	}
	env.Mock.AddMessage("msg1", testMIME(), []string{"INBOX"})
	env.Mock.AddMessage("msg2", testMIME(), []string{"INBOX"})
	env.Mock.AddMessage("msg3", testMIME(), []string{"INBOX"})
	env.Mock.AddMessage("msg4", testMIME(), []string{"INBOX"})

	summary2 := runFullSync(t, env)
	assertSummary(t, summary2, WantSummary{Added: new(int64(0))})
}

func TestFullSyncWithErrors(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 3, 12345, "msg1", "msg2", "msg3")

	// Make msg2 fail to fetch
	env.Mock.GetMessageError["msg2"] = &gmail.NotFoundError{Path: "/messages/msg2"}

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(2)), Errors: new(int64(1))})
}

func TestMIMEParsing(t *testing.T) {
	env := newTestEnv(t)

	pdfData := []byte{0x25, 0x50, 0x44, 0x46, 0x2d, 0x31, 0x2e, 0x34, 0x0a, 0x25, 0xe2, 0xe3, 0xcf, 0xd3, 0x0a, 0x31, 0x20, 0x30, 0x20, 0x6f, 0x62, 0x6a, 0x0a, 0x3c, 0x3c, 0x2f, 0x54, 0x79, 0x70, 0x65, 0x2f, 0x43, 0x61, 0x74, 0x61, 0x6c, 0x6f, 0x67, 0x2f, 0x50, 0x61, 0x67, 0x65, 0x73, 0x20, 0x32, 0x20, 0x30, 0x20, 0x52, 0x3e, 0x3e, 0x0a, 0x65, 0x6e, 0x64, 0x6f, 0x62, 0x6a}
	complexMIME := testemail.NewMessage().
		From(`"John Doe" <john@example.com>`).
		To(`"Jane Smith" <jane@example.com>, bob@example.com`).
		Cc("cc@example.com").
		Subject("Re: Meeting Notes").
		Date("Tue, 15 Jan 2024 14:30:00 -0500").
		Header("Message-ID", "<msg123@example.com>").
		Header("In-Reply-To", "<msg122@example.com>").
		Body("Hello,\n\nThis is the message body.\n\nBest regards,\nJohn\n").
		WithAttachment("document.pdf", "application/pdf", pdfData).
		Bytes()

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("complex1", complexMIME, []string{"INBOX"})

	env.SetOptions(t, func(o *Options) {
		o.AttachmentsDir = filepath.Join(env.TmpDir, "attachments")
	})

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	assertAttachmentCount(t, env.Store, 1)
}

func TestStoreAttachment_ComputesHashWhenMissing(t *testing.T) {
	require := requirepkg.New(t)
	env := newTestEnv(t)

	attachmentsDir := filepath.Join(env.TmpDir, "attachments")
	env.SetOptions(t, func(o *Options) {
		o.AttachmentsDir = attachmentsDir
	})

	src := env.CreateSource(t)
	convID, err := env.Store.EnsureConversation(src.ID, "t1", "Thread")
	require.NoError(err, "EnsureConversation")
	messageID, err := env.Store.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m1",
		MessageType:     "email",
	})
	require.NoError(err, "UpsertMessage")

	content := []byte("hello")
	sum := sha256.Sum256(content)
	wantHash := hex.EncodeToString(sum[:])

	att := mime.Attachment{
		Filename:    "a.txt",
		ContentType: "text/plain",
		Size:        len(content),
		ContentHash: "",
		Content:     content,
	}
	require.NoError(env.Syncer.storeAttachment(messageID, &att), "storeAttachment")
	require.Equal(wantHash, att.ContentHash, "ContentHash")

	var gotHash, storagePath string
	require.NoError(env.Store.DB().QueryRow(`SELECT content_hash, storage_path FROM attachments WHERE message_id = ?`, messageID).Scan(&gotHash, &storagePath), "select attachment")
	require.Equal(wantHash, gotHash, "db content_hash")

	fullPath := filepath.Join(attachmentsDir, filepath.FromSlash(storagePath))
	b, err := os.ReadFile(fullPath)
	require.NoError(err, "read attachment file")
	require.Equal(string(content), string(b), "attachment file contents")
}

func TestStoreAttachment_InvalidContentHash_ReturnsError(t *testing.T) {
	require := requirepkg.New(t)
	env := newTestEnv(t)

	attachmentsDir := filepath.Join(env.TmpDir, "attachments")
	env.SetOptions(t, func(o *Options) {
		o.AttachmentsDir = attachmentsDir
	})

	src := env.CreateSource(t)
	convID, err := env.Store.EnsureConversation(src.ID, "t1", "Thread")
	require.NoError(err, "EnsureConversation")
	messageID, err := env.Store.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m1",
		MessageType:     "email",
	})
	require.NoError(err, "UpsertMessage")

	content := []byte("hello")
	att := mime.Attachment{
		Filename:    "a.txt",
		ContentType: "text/plain",
		Size:        len(content),
		ContentHash: "nope", // malformed
		Content:     content,
	}
	require.Error(env.Syncer.storeAttachment(messageID, &att), "expected error")

	_, statErr := os.Stat(attachmentsDir)
	require.Error(statErr, "attachments dir should not have been created for invalid content hash")

	var count int
	require.NoError(env.Store.DB().QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`, messageID).Scan(&count), "count attachments")
	require.Zero(count, "count")
}

func TestFullSyncEmptyInbox(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 0
	env.Mock.Profile.HistoryID = 12345

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(0)), Found: new(int64(0))})
}

func TestFullSyncProfileError(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.ProfileError = fmt.Errorf("auth failed")

	_, err := env.Syncer.Full(env.Context, testEmail)
	assertpkg.Error(t, err, "expected error when profile fails")
}

func TestFullSyncAllDuplicates(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 3, 12345, "msg1", "msg2", "msg3")

	// First sync
	runFullSync(t, env)

	// Second sync with same messages - all should be skipped
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(0)), Skipped: new(int64(3))})
}

func TestFullSyncNoResume(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 2, 12345, "msg1", "msg2")

	env.SetOptions(t, func(o *Options) {
		o.NoResume = true
	})

	summary := runFullSync(t, env)
	assertpkg.False(t, summary.WasResumed, "expected WasResumed to be false with NoResume option")
	assertSummary(t, summary, WantSummary{Added: new(int64(2))})
}

func TestFullSyncAllErrors(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 3, 12345, "msg1", "msg2", "msg3")

	env.Mock.GetMessageError["msg1"] = &gmail.NotFoundError{Path: "/messages/msg1"}
	env.Mock.GetMessageError["msg2"] = &gmail.NotFoundError{Path: "/messages/msg2"}
	env.Mock.GetMessageError["msg3"] = &gmail.NotFoundError{Path: "/messages/msg3"}

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(0)), Errors: new(int64(3))})
}

func TestFullSyncWithQuery(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 2, 12345, "msg1", "msg2")

	env.SetOptions(t, func(o *Options) {
		o.Query = "before:2024/06/01"
	})

	summary := runFullSync(t, env)

	assertpkg.Equal(t, "before:2024/06/01", env.Mock.LastQuery, "query")
	assertSummary(t, summary, WantSummary{Added: new(int64(2))})
}

func TestFullSyncPagination(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.HistoryID = 12345
	seedPagedMessages(env, 6, 2, "msg")

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(6))})
	assertListMessagesCalls(t, env, 3)
}

func TestSyncerWithLogger(t *testing.T) {
	env := newTestEnv(t)
	syncer := env.Syncer.WithLogger(nil)
	assertpkg.NotNil(t, syncer, "WithLogger should return syncer for chaining")
}

func TestSyncerWithProgress(t *testing.T) {
	env := newTestEnv(t)
	syncer := env.Syncer.WithProgress(gmail.NullProgress{})
	assertpkg.NotNil(t, syncer, "WithProgress should return syncer for chaining")
}

// Tests for incremental sync

func TestIncrementalSyncNilSource(t *testing.T) {
	env := newTestEnv(t)

	_, err := env.Syncer.Incremental(env.Context, nil)
	assertpkg.Error(t, err, "expected error for nil source")
}

func TestIncrementalSyncNoHistoryID(t *testing.T) {
	env := newTestEnv(t)

	source := env.CreateSource(t)

	_, err := env.Syncer.Incremental(env.Context, source)
	assertpkg.Error(t, err, "expected error for incremental sync without history ID")
}

func TestIncrementalSyncAlreadyUpToDate(t *testing.T) {
	env := newTestEnv(t)
	env.CreateSourceWithHistory(t, "12345")

	env.Mock.Profile.MessagesTotal = 10
	env.Mock.Profile.HistoryID = 12345 // Same as cursor

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(0))})
}

func TestIncrementalSyncWithChanges(t *testing.T) {
	env := newTestEnv(t)
	env.CreateSourceWithHistory(t, "12340")

	env.Mock.Profile.MessagesTotal = 10
	env.Mock.Profile.HistoryID = 12350
	env.Mock.AddMessage("new-msg-1", testMIME(), []string{"INBOX"})
	env.Mock.AddMessage("new-msg-2", testMIME(), []string{"INBOX"})

	env.SetHistory(12350,
		historyAdded("new-msg-1"),
		historyAdded("new-msg-2"),
	)

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(2))})
}

func TestIncrementalSyncWithDeletions(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 2, 12340, "msg1", "msg2")

	runFullSync(t, env)

	// Now simulate deletion via incremental
	env.SetHistory(12350, historyDeleted("msg1"))

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(1))})

	// Verify deletion was persisted
	assertDeletedFromSource(t, env.Store, "msg1", true)
	assertDeletedFromSource(t, env.Store, "msg2", false)
}

func TestIncrementalSyncHistoryExpired(t *testing.T) {
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "1000")

	env.Mock.Profile.MessagesTotal = 10
	env.Mock.Profile.HistoryID = 12350
	env.Mock.HistoryError = &gmail.NotFoundError{Path: "/history"}

	_, err := env.Syncer.Incremental(env.Context, source)
	assertpkg.Error(t, err, "expected error for expired history")
}

func TestIncrementalSyncProfileError(t *testing.T) {
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "12345")
	env.Mock.ProfileError = fmt.Errorf("auth failed")

	_, err := env.Syncer.Incremental(env.Context, source)
	assertpkg.Error(t, err, "expected error when profile fails")
}

func TestIncrementalSyncWithLabelAdded(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12340
	env.Mock.AddMessage("msg1", testMIME(), []string{"INBOX"})

	runFullSync(t, env)

	// Record call count after full sync
	callsAfterFull := len(env.Mock.GetMessageCalls)

	// Now simulate label addition via incremental
	env.SetHistory(12350, historyLabelAdded("msg1", "STARRED"))

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(1))})

	// No additional GetMessageRaw calls should have been made for the existing message
	callsAfterIncr := len(env.Mock.GetMessageCalls)
	assertpkg.Equal(t, callsAfterFull, callsAfterIncr,
		"expected 0 GetMessageRaw calls during incremental")

	// Verify the label was actually added in the database
	assertMessageHasLabel(t, env.Store, "msg1", "STARRED")
}

func TestIncrementalSyncWithLabelRemoved(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12340
	env.Mock.AddMessage("msg1", testMIME(), []string{"INBOX", "STARRED"})

	runFullSync(t, env)

	// Verify STARRED exists after full sync
	assertMessageHasLabel(t, env.Store, "msg1", "STARRED")

	// Record call count after full sync
	callsAfterFull := len(env.Mock.GetMessageCalls)

	// Now simulate label removal via incremental
	env.SetHistory(12350, historyLabelRemoved("msg1", "STARRED"))

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(1))})

	// No additional GetMessageRaw calls should have been made
	callsAfterIncr := len(env.Mock.GetMessageCalls)
	assertpkg.Equal(t, callsAfterFull, callsAfterIncr,
		"expected 0 GetMessageRaw calls during incremental")

	// Verify the label was actually removed in the database
	assertMessageNotHasLabel(t, env.Store, "msg1", "STARRED")
	// INBOX should still be there
	assertMessageHasLabel(t, env.Store, "msg1", "INBOX")
}

func TestIncrementalSyncLabelAddedToNewMessage(t *testing.T) {
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "12340")
	_, err := env.Store.EnsureLabel(source.ID, "INBOX", "Inbox", "system")
	requirepkg.NoError(t, err, "EnsureLabel INBOX")
	_, err = env.Store.EnsureLabel(source.ID, "STARRED", "Starred", "system")
	requirepkg.NoError(t, err, "EnsureLabel STARRED")

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12350
	env.Mock.AddMessage("new-msg", testMIME(), []string{"INBOX", "STARRED"})

	env.SetHistory(12350, historyLabelAdded("new-msg", "STARRED"))

	_, err = env.Syncer.Incremental(env.Context, source)
	requirepkg.NoError(t, err, "incremental sync")

	assertMessageCount(t, env.Store, 1)
}

func TestIncrementalSyncLabelRemovedFromMissingMessage(t *testing.T) {
	env := newTestEnv(t)
	env.CreateSourceWithHistory(t, "12340")

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12350

	env.SetHistory(12350, historyLabelRemoved("unknown-msg", "STARRED"))

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(0))})
}

func TestFullSyncWithAttachment(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("msg-with-attachment", testMIMEWithAttachment(), []string{"INBOX"})

	attachDir := withAttachmentsDir(t, env)

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})

	_, statErr := os.Stat(attachDir)
	assertpkg.False(t, os.IsNotExist(statErr), "attachments directory should have been created")

	assertAttachmentCount(t, env.Store, 1)
}

func TestFullSyncWithEmptyAttachment(t *testing.T) {
	env := newTestEnv(t)

	emptyAttachMIME := testemail.NewMessage().
		Subject("Empty Attachment").
		Body("Body text.").
		WithAttachment("empty.bin", "application/octet-stream", nil).
		Bytes()

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("msg-empty-attach", emptyAttachMIME, []string{"INBOX"})

	withAttachmentsDir(t, env)

	runFullSync(t, env)
	assertAttachmentCount(t, env.Store, 0)
}

func TestFullSyncAttachmentDeduplication(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 2
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("msg1-attach", testMIMEWithAttachment(), []string{"INBOX"})
	env.Mock.AddMessage("msg2-attach", testMIMEWithAttachment(), []string{"INBOX"})

	attachDir := withAttachmentsDir(t, env)

	runFullSync(t, env)
	assertAttachmentCount(t, env.Store, 2)

	assertpkg.Equal(t, 1, countFiles(t, attachDir), "files in attachments dir (deduped)")
}

// TestFullSync_MessageVariations consolidates tests for various MIME message formats.
func TestFullSync_MessageVariations(t *testing.T) {
	tests := []struct {
		name  string
		mime  func() []byte
		check func(*testing.T, *TestEnv)
	}{
		{
			name: "NoSubject",
			mime: testMIMENoSubject,
		},
		{
			name: "MultipleRecipients",
			mime: testMIMEMultipleRecipients,
		},
		{
			name: "HTMLOnly",
			mime: func() []byte {
				return testemail.NewMessage().
					Subject("HTML Only").
					ContentType(`text/html; charset="utf-8"`).
					Body("<html><body><p>This is HTML only content.</p></body></html>").
					Bytes()
			},
		},
		{
			name: "DuplicateRecipients",
			mime: testMIMEDuplicateRecipients,
			check: func(t *testing.T, env *TestEnv) {
				assertRecipientCount(t, env.Store, "msg", "to", 2)
				assertRecipientCount(t, env.Store, "msg", "cc", 1)
				assertRecipientCount(t, env.Store, "msg", "bcc", 1)
				assertDisplayName(t, env.Store, "msg", "to", "duplicate@example.com", "Duplicate Person")
				assertDisplayName(t, env.Store, "msg", "cc", "cc-dup@example.com", "CC Duplicate")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			seedMessages(env, 1, 12345, "msg")
			raw := tt.mime()
			env.Mock.Messages["msg"].Raw = raw
			env.Mock.Messages["msg"].SizeEstimate = int64(len(raw))

			summary := runFullSync(t, env)
			assertSummary(t, summary, WantSummary{Added: new(int64(1)), Errors: new(int64(0))})
			assertMessageCount(t, env.Store, 1)

			if tt.check != nil {
				tt.check(t, env)
			}
		})
	}
}

func TestFullSync_Latin1InFromName(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	env := newTestEnv(t)
	seedMessages(env, 1, 12345, "msg")

	// Build a MIME message with an RFC 2047 encoded From name that claims UTF-8
	// but actually contains Latin-1 bytes. This is a real-world scenario where a
	// sender's MUA mis-labels the charset, producing invalid UTF-8 after decoding.
	// The =C9 byte is Latin-1 É, which is not valid UTF-8 when surrounded by ASCII.
	raw := []byte("From: =?UTF-8?Q?Jane_Doe=C9ric?= <sender@example.com>\n" +
		"To: recipient@example.com\n" +
		"Subject: Test\n" +
		"Date: Mon, 01 Jan 2024 12:00:00 +0000\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\n" +
		"\n" +
		"Body text.\n")

	env.Mock.Messages["msg"].Raw = raw
	env.Mock.Messages["msg"].SizeEstimate = int64(len(raw))

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1)), Errors: new(int64(0))})

	// Verify the participant display_name in the participants table is valid UTF-8.
	// Before the fix, raw Latin-1 bytes would be stored as-is, causing DuckDB errors
	// when exporting to Parquet.
	displayName, err := env.Store.InspectParticipantDisplayName("sender@example.com")
	require.NoError(err, "InspectParticipantDisplayName")
	// EnsureUTF8 should convert the Latin-1 \xC9 to the UTF-8 É (U+00C9)
	want := "Jane DoeÉric"
	assert.Equal(want, displayName, "participant display_name")

	// Also verify the message_recipients display_name is valid
	recipDisplayName, err := env.Store.InspectDisplayName("msg", "from", "sender@example.com")
	require.NoError(err, "InspectDisplayName")
	assert.Equal(want, recipDisplayName, "recipient display_name")
}

func TestFullSync_InvalidUTF8InAllAddressFields(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	env := newTestEnv(t)
	seedMessages(env, 1, 12345, "msg")

	// Test that UTF-8 validation applies to all address fields (To, Cc, Bcc),
	// not just From. Uses Windows-1252 smart quotes (\x93, \x94) mis-labeled as UTF-8,
	// a common real-world scenario from Outlook emails.
	raw := []byte("From: =?UTF-8?Q?=93From=94_Name?= <from@example.com>\n" +
		"To: =?UTF-8?Q?=93To=94_Name?= <to@example.com>\n" +
		"Cc: =?UTF-8?Q?=93Cc=94_Name?= <cc@example.com>\n" +
		"Bcc: =?UTF-8?Q?=93Bcc=94_Name?= <bcc@example.com>\n" +
		"Subject: Test\n" +
		"Date: Mon, 01 Jan 2024 12:00:00 +0000\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\n" +
		"\n" +
		"Body text.\n")

	env.Mock.Messages["msg"].Raw = raw
	env.Mock.Messages["msg"].SizeEstimate = int64(len(raw))

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1)), Errors: new(int64(0))})

	// EnsureUTF8 should detect Windows-1252 and convert smart quotes to their
	// proper Unicode equivalents: \x93 → U+201C ("), \x94 → U+201D (")
	tests := []struct {
		recipType string
		email     string
	}{
		{"from", "from@example.com"},
		{"to", "to@example.com"},
		{"cc", "cc@example.com"},
		{"bcc", "bcc@example.com"},
	}
	for _, tt := range tests {
		// Verify participants table has valid UTF-8
		displayName, err := env.Store.InspectParticipantDisplayName(tt.email)
		require.NoError(err, "InspectParticipantDisplayName(%s)", tt.email)
		titled := strings.ToUpper(tt.recipType[:1]) + tt.recipType[1:]
		want := "\u201c" + titled + "\u201d Name"
		assert.Equal(want, displayName, "participant %s display_name", tt.email)

		// Verify message_recipients table has valid UTF-8
		recipName, err := env.Store.InspectDisplayName("msg", tt.recipType, tt.email)
		require.NoError(err, "InspectDisplayName(%s, %s)", tt.recipType, tt.email)
		assert.Equal(want, recipName, "recipient %s/%s display_name", tt.recipType, tt.email)
	}
}

func TestFullSync_InvalidUTF8InAttachmentFilename(t *testing.T) {
	assert := assertpkg.New(t)
	env := newTestEnv(t)

	// Construct a MIME message with raw Latin-1 byte \xE9 (é) in the attachment
	// filename. Enmime sanitizes invalid bytes to U+FFFD before our code sees them;
	// the sync-level EnsureUTF8 call is defense-in-depth for any future parser changes.
	raw := []byte("From: sender@example.com\n" +
		"To: recipient@example.com\n" +
		"Subject: Attachment Test\n" +
		"Date: Mon, 01 Jan 2024 12:00:00 +0000\n" +
		"MIME-Version: 1.0\n" +
		"Content-Type: multipart/mixed; boundary=\"b\"\n" +
		"\n" +
		"--b\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\n\nBody text.\n" +
		"--b\n" +
		"Content-Type: application/pdf; name=\"caf\xe9.pdf\"\n" +
		"Content-Disposition: attachment; filename=\"caf\xe9.pdf\"\n" +
		"Content-Transfer-Encoding: base64\n\n" +
		"SGVsbG8gV29ybGQh\n" +
		"--b--\n")

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("msg-attach", raw, []string{"INBOX"})

	withAttachmentsDir(t, env)

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	assertAttachmentCount(t, env.Store, 1)

	filename, mimeType, err := env.Store.InspectAttachment("msg-attach")
	requirepkg.NoError(t, err, "InspectAttachment")

	// Enmime replaces the invalid \xE9 byte with U+FFFD (replacement character).
	// Our EnsureUTF8 would convert it to the proper é if enmime didn't sanitize first.
	// Either way, the stored filename must be valid UTF-8 and preserve the base name.
	assert.True(utf8.ValidString(filename), "attachment filename %q is not valid UTF-8", filename)
	assert.True(strings.HasPrefix(filename, "caf"), "attachment filename = %q, want caf*.pdf pattern", filename)
	assert.True(strings.HasSuffix(filename, ".pdf"), "attachment filename = %q, want caf*.pdf pattern", filename)

	// Content-type should be the clean base MIME type
	assert.Equal("application/pdf", mimeType, "attachment mime_type")
}

func TestFullSync_MultipleEncodingIssuesSameMessage(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	env := newTestEnv(t)
	seedMessages(env, 1, 12345, "msg")

	// Real-world scenario: a single email with multiple encoding issues.
	// Latin-1 É (\xC9) in From name, and Windows-1252 smart quote (\x93) in To name.
	raw := []byte("From: =?UTF-8?Q?Doe=C9ric?= <from@example.com>\n" +
		"To: =?UTF-8?Q?=93Quoted=94?= <to@example.com>\n" +
		"Subject: =?UTF-8?Q?Caf=E9?=\n" +
		"Date: Mon, 01 Jan 2024 12:00:00 +0000\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\n" +
		"\n" +
		"Body text.\n")

	env.Mock.Messages["msg"].Raw = raw
	env.Mock.Messages["msg"].SizeEstimate = int64(len(raw))

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1)), Errors: new(int64(0))})

	// From name: Latin-1 \xC9 → UTF-8 É
	fromName, err := env.Store.InspectParticipantDisplayName("from@example.com")
	require.NoError(err, "InspectParticipantDisplayName(from)")
	assert.Equal("DoeÉric", fromName, "from display_name")

	// To name: Windows-1252 \x93/\x94 → Unicode left/right double quotes
	toName, err := env.Store.InspectParticipantDisplayName("to@example.com")
	require.NoError(err, "InspectParticipantDisplayName(to)")
	assert.Equal("\u201cQuoted\u201d", toName, "to display_name")

	// Subject: Latin-1 \xE9 → UTF-8 é (already validated by existing code path)
	insp, err := env.Store.InspectMessage("msg")
	require.NoError(err, "InspectMessage")
	assert.Contains(insp.RecipientDisplayName["from:from@example.com"], "É", "from recipient display_name should contain É")
}

func TestFullSyncWithMIMEParseError(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 2
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("msg-good", testMIME(), []string{"INBOX"})
	env.Mock.Messages["msg-bad"] = &gmail.RawMessage{
		ID:           "msg-bad",
		ThreadID:     "thread_msg-bad",
		LabelIDs:     []string{"INBOX"},
		Raw:          []byte("not valid mime at all - just garbage"),
		Snippet:      "This is the snippet preview",
		SizeEstimate: 100,
	}

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(2)), Errors: new(int64(0))})

	// Verify the bad message was stored with placeholder content
	assertBodyContains(t, env.Store, "msg-bad", "MIME parsing failed")
	assertRawDataExists(t, env.Store, "msg-bad")
}

func TestFullSyncMessageFetchError(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 2
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("msg-good", testMIME(), []string{"INBOX"})

	env.Mock.MessagePages = [][]string{{"msg-good", "msg-missing"}}

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
}

func TestIncrementalSyncLabelsError(t *testing.T) {
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "12340")

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12350
	env.Mock.LabelsError = fmt.Errorf("labels API error")

	_, err := env.Syncer.Incremental(env.Context, source)
	assertpkg.Error(t, err, "expected error when labels sync fails")
}

func TestFullSyncResumeWithCursor(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	env := newTestEnv(t)
	env.Mock.Profile.HistoryID = 12345
	seedPagedMessages(env, 4, 2, "msg")

	source := env.CreateSource(t)

	// Process just page 1
	env.Mock.MessagePages = [][]string{{"msg1", "msg2"}}
	runFullSync(t, env)
	assertMessageCount(t, env.Store, 2)

	// Restore both pages and create an "interrupted" sync
	env.Mock.MessagePages = [][]string{
		{"msg1", "msg2"},
		{"msg3", "msg4"},
	}
	env.Mock.ListMessagesCalls = 0

	syncID, err := env.Store.StartSync(source.ID, "full")
	require.NoError(err, "StartSync")

	checkpoint := &store.Checkpoint{
		PageToken:         "page_1",
		MessagesProcessed: 2,
		MessagesAdded:     2,
	}
	require.NoError(env.Store.UpdateSyncCheckpoint(syncID, checkpoint), "UpdateSyncCheckpoint")

	summary := runFullSync(t, env)

	assert.True(summary.WasResumed, "expected WasResumed = true")
	assert.Equal("page_1", summary.ResumedFromToken, "ResumedFromToken")
	assertSummary(t, summary, WantSummary{Added: new(int64(4))})

	assertListMessagesCalls(t, env, 1)
	assertMessageCount(t, env.Store, 4)
}

func TestFullSyncDateFallbackToInternalDate(t *testing.T) {
	env := newTestEnv(t)

	badDateMIME := testemail.NewMessage().
		Subject("Bad Date").
		Date("This is not a valid date").
		Body("Message with invalid date header.").
		Bytes()

	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.Messages["msg-bad-date"] = &gmail.RawMessage{
		ID:           "msg-bad-date",
		ThreadID:     "thread-bad-date",
		LabelIDs:     []string{"INBOX"},
		Raw:          badDateMIME,
		InternalDate: 1705320000000, // 2024-01-15T12:00:00Z
	}
	env.Mock.MessagePages = [][]string{{"msg-bad-date"}}

	runFullSync(t, env)

	assertDateFallback(t, env.Store, "msg-bad-date", "2024-01-15", "12:00:00")
}

func TestFullSyncEmptyRawMIME(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 2
	env.Mock.Profile.HistoryID = 12345

	env.Mock.AddMessage("msg-good", testMIME(), []string{"INBOX"})
	env.Mock.Messages["msg-empty-raw"] = &gmail.RawMessage{
		ID:           "msg-empty-raw",
		ThreadID:     "thread-empty-raw",
		LabelIDs:     []string{"INBOX"},
		Raw:          []byte{},
		SizeEstimate: 0,
	}

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1)), Errors: new(int64(1))})
}

func TestFullSyncEmptyThreadID(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.UseRawThreadID = true

	raw := testMIME()
	env.Mock.Messages["msg-no-thread"] = &gmail.RawMessage{
		ID:           "msg-no-thread",
		ThreadID:     "",
		LabelIDs:     []string{"INBOX"},
		Raw:          raw,
		SizeEstimate: int64(len(raw)),
	}
	env.Mock.MessagePages = [][]string{{"msg-no-thread"}}

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1)), Errors: new(int64(0))})

	assertThreadSourceID(t, env.Store, "msg-no-thread", "msg-no-thread")
}

func TestFullSyncListEmptyThreadIDRawPresent(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345

	raw := testMIME()
	env.Mock.ListThreadIDOverride = map[string]string{
		"msg-list-empty": "",
	}
	env.Mock.Messages["msg-list-empty"] = &gmail.RawMessage{
		ID:           "msg-list-empty",
		ThreadID:     "actual-thread-from-raw",
		LabelIDs:     []string{"INBOX"},
		Raw:          raw,
		SizeEstimate: int64(len(raw)),
	}
	env.Mock.MessagePages = [][]string{{"msg-list-empty"}}

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1)), Errors: new(int64(0))})

	assertThreadSourceID(t, env.Store, "msg-list-empty", "actual-thread-from-raw")
}

// Tests for initSyncState

func TestInitSyncState_NewSync(t *testing.T) {
	assert := assertpkg.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)

	state, err := env.Syncer.initSyncState(source.ID)
	requirepkg.NoError(t, err, "initSyncState")

	assert.False(state.wasResumed, "expected wasResumed = false for new sync")
	assert.Empty(state.pageToken, "pageToken")
	assert.NotZero(state.syncID, "expected non-zero syncID")
	assert.Equal(int64(0), state.checkpoint.MessagesProcessed, "MessagesProcessed")
}

func TestInitSyncState_Resume(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)

	// Create an active sync with checkpoint
	syncID, err := env.Store.StartSync(source.ID, "full")
	require.NoError(err, "StartSync")
	checkpoint := &store.Checkpoint{
		PageToken:         "resume_token_123",
		MessagesProcessed: 50,
		MessagesAdded:     45,
		MessagesUpdated:   3,
		ErrorsCount:       2,
	}
	require.NoError(env.Store.UpdateSyncCheckpoint(syncID, checkpoint), "UpdateSyncCheckpoint")

	state, err := env.Syncer.initSyncState(source.ID)
	require.NoError(err, "initSyncState")

	assert.True(state.wasResumed, "expected wasResumed = true")
	assert.Equal("resume_token_123", state.pageToken, "pageToken")
	assert.Equal(syncID, state.syncID, "syncID")
	assert.Equal(int64(50), state.checkpoint.MessagesProcessed, "MessagesProcessed")
	assert.Equal(int64(45), state.checkpoint.MessagesAdded, "MessagesAdded")
}

func TestInitSyncState_NoResumeOption(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	env := newTestEnv(t)
	env.SetOptions(t, func(o *Options) {
		o.NoResume = true
	})
	source := env.CreateSource(t)

	// Create an active sync with checkpoint
	syncID, err := env.Store.StartSync(source.ID, "full")
	require.NoError(err, "StartSync")
	checkpoint := &store.Checkpoint{
		PageToken:         "resume_token_123",
		MessagesProcessed: 50,
	}
	require.NoError(env.Store.UpdateSyncCheckpoint(syncID, checkpoint), "UpdateSyncCheckpoint")

	state, err := env.Syncer.initSyncState(source.ID)
	require.NoError(err, "initSyncState")

	assert.False(state.wasResumed, "expected wasResumed = false with NoResume option")
	assert.Empty(state.pageToken, "pageToken with NoResume")
	assert.NotEqual(syncID, state.syncID, "expected new syncID, not the existing one")
}

// Tests for processBatch

func TestProcessBatch_EmptyBatch(t *testing.T) {
	assert := assertpkg.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	labelMap := make(map[string]int64)
	checkpoint := &store.Checkpoint{}
	summary := &gmail.SyncSummary{}

	listResp := &gmail.MessageListResponse{
		Messages: nil,
	}

	result, err := env.Syncer.processBatch(env.Context, source.ID, listResp, labelMap, checkpoint, summary)
	requirepkg.NoError(t, err, "processBatch")

	assert.Equal(int64(0), result.processed, "processed")
	assert.Equal(int64(0), result.added, "added")
	assert.Equal(int64(0), result.skipped, "skipped")
}

func TestProcessBatch_AllNew(t *testing.T) {
	assert := assertpkg.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	labelMap, _ := env.Store.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{
		"INBOX": {Name: "Inbox", Type: "system"},
	})
	checkpoint := &store.Checkpoint{}
	summary := &gmail.SyncSummary{}

	env.Mock.AddMessage("msg1", testMIME(), []string{"INBOX"})
	env.Mock.AddMessage("msg2", testMIME(), []string{"INBOX"})

	listResp := &gmail.MessageListResponse{
		Messages: []gmail.MessageID{
			{ID: "msg1", ThreadID: "thread1"},
			{ID: "msg2", ThreadID: "thread2"},
		},
	}

	result, err := env.Syncer.processBatch(env.Context, source.ID, listResp, labelMap, checkpoint, summary)
	requirepkg.NoError(t, err, "processBatch")

	assert.Equal(int64(2), result.processed, "processed")
	assert.Equal(int64(2), result.added, "added")
	assert.Equal(int64(0), result.skipped, "skipped")
}

func TestProcessBatch_AllExisting(t *testing.T) {
	assert := assertpkg.New(t)
	env := newTestEnv(t)
	seedMessages(env, 2, 12345, "msg1", "msg2")

	// First sync to add messages
	runFullSync(t, env)

	source, _ := env.Store.GetOrCreateSource("gmail", testEmail)
	labelMap, _ := env.Store.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{
		"INBOX": {Name: "Inbox", Type: "system"},
	})
	checkpoint := &store.Checkpoint{}
	summary := &gmail.SyncSummary{}

	listResp := &gmail.MessageListResponse{
		Messages: []gmail.MessageID{
			{ID: "msg1", ThreadID: "thread1"},
			{ID: "msg2", ThreadID: "thread2"},
		},
	}

	result, err := env.Syncer.processBatch(env.Context, source.ID, listResp, labelMap, checkpoint, summary)
	requirepkg.NoError(t, err, "processBatch")

	assert.Equal(int64(2), result.processed, "processed")
	assert.Equal(int64(0), result.added, "added (all existing)")
	assert.Equal(int64(2), result.skipped, "skipped")
}

func TestProcessBatch_MixedNewAndExisting(t *testing.T) {
	assert := assertpkg.New(t)
	env := newTestEnv(t)
	seedMessages(env, 1, 12345, "msg1")

	// First sync to add msg1
	runFullSync(t, env)

	source, _ := env.Store.GetOrCreateSource("gmail", testEmail)
	labelMap, _ := env.Store.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{
		"INBOX": {Name: "Inbox", Type: "system"},
	})
	checkpoint := &store.Checkpoint{}
	summary := &gmail.SyncSummary{}

	// Add msg2 to mock
	env.Mock.AddMessage("msg2", testMIME(), []string{"INBOX"})

	listResp := &gmail.MessageListResponse{
		Messages: []gmail.MessageID{
			{ID: "msg1", ThreadID: "thread1"},
			{ID: "msg2", ThreadID: "thread2"},
		},
	}

	result, err := env.Syncer.processBatch(env.Context, source.ID, listResp, labelMap, checkpoint, summary)
	requirepkg.NoError(t, err, "processBatch")

	assert.Equal(int64(2), result.processed, "processed")
	assert.Equal(int64(1), result.added, "added")
	assert.Equal(int64(1), result.skipped, "skipped")
}

func TestProcessBatch_OldestDatePropagation(t *testing.T) {
	assert := assertpkg.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	labelMap, _ := env.Store.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{
		"INBOX": {Name: "Inbox", Type: "system"},
	})
	checkpoint := &store.Checkpoint{}
	summary := &gmail.SyncSummary{}

	// Add messages with specific internal dates
	// msg1: Jan 15, 2024, msg2: Jan 10, 2024 (older)
	env.Mock.Messages["msg1"] = &gmail.RawMessage{
		ID:           "msg1",
		ThreadID:     "thread1",
		LabelIDs:     []string{"INBOX"},
		Raw:          testMIME(),
		InternalDate: 1705320000000, // 2024-01-15T12:00:00Z
	}
	env.Mock.Messages["msg2"] = &gmail.RawMessage{
		ID:           "msg2",
		ThreadID:     "thread2",
		LabelIDs:     []string{"INBOX"},
		Raw:          testMIME(),
		InternalDate: 1704888000000, // 2024-01-10T12:00:00Z
	}

	listResp := &gmail.MessageListResponse{
		Messages: []gmail.MessageID{
			{ID: "msg1", ThreadID: "thread1"},
			{ID: "msg2", ThreadID: "thread2"},
		},
	}

	result, err := env.Syncer.processBatch(env.Context, source.ID, listResp, labelMap, checkpoint, summary)
	requirepkg.NoError(t, err, "processBatch")

	// oldestDate should be Jan 10, 2024
	assert.False(result.oldestDate.IsZero(), "expected oldestDate to be set")
	gotYear, gotMonth, gotDay := result.oldestDate.Year(), int(result.oldestDate.Month()), result.oldestDate.Day()
	assert.Equal(2024, gotYear, "year")
	assert.Equal(1, gotMonth, "month")
	assert.Equal(10, gotDay, "day")
}

func TestProcessBatch_ErrorsCount(t *testing.T) {
	env := newTestEnv(t)
	source := env.CreateSource(t)
	labelMap, _ := env.Store.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{
		"INBOX": {Name: "Inbox", Type: "system"},
	})
	checkpoint := &store.Checkpoint{}
	summary := &gmail.SyncSummary{}

	env.Mock.AddMessage("msg1", testMIME(), []string{"INBOX"})
	// msg2 will return nil (simulating fetch failure)
	env.Mock.GetMessageError["msg2"] = &gmail.NotFoundError{Path: "/messages/msg2"}

	listResp := &gmail.MessageListResponse{
		Messages: []gmail.MessageID{
			{ID: "msg1", ThreadID: "thread1"},
			{ID: "msg2", ThreadID: "thread2"},
		},
	}

	result, err := env.Syncer.processBatch(env.Context, source.ID, listResp, labelMap, checkpoint, summary)
	requirepkg.NoError(t, err, "processBatch")

	assertpkg.Equal(t, int64(1), result.added, "added")
	assertpkg.Equal(t, int64(1), checkpoint.ErrorsCount, "ErrorsCount")
}

// TestAttachmentFilePermissions verifies that attachment files are saved with
// restrictive permissions (0600) to protect email content.
func TestAttachmentFilePermissions(t *testing.T) {
	require := requirepkg.New(t)
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("msg-with-attachment", testMIMEWithAttachment(), []string{"INBOX"})

	attachDir := withAttachmentsDir(t, env)

	runFullSync(t, env)

	// Find the attachment file
	var attachmentPath string
	err := filepath.WalkDir(attachDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			attachmentPath = path
		}
		return nil
	})
	require.NoError(err, "WalkDir(%s)", attachDir)
	require.NotEmpty(attachmentPath, "no attachment file found")

	info, err := os.Stat(attachmentPath)
	require.NoError(err, "Stat(%s)", attachmentPath)

	// File should have 0600 permissions (owner read/write only)
	// Windows does not support Unix permissions.
	if runtime.GOOS != "windows" {
		assertpkg.Equal(t, os.FileMode(0600), info.Mode().Perm(), "attachment file permissions")
	}
}

// TestIncrementalSyncLabelAddAndRemoveOnExisting verifies that adding and removing
// labels on the same existing message in a single history page applies correctly
// and makes NO API calls to re-fetch the message.
func TestIncrementalSyncLabelAddAndRemoveOnExisting(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12340
	env.Mock.AddMessage("msg1", testMIME(), []string{"INBOX", "STARRED"})

	runFullSync(t, env)

	// Verify starting labels
	assertMessageHasLabel(t, env.Store, "msg1", "INBOX")
	assertMessageHasLabel(t, env.Store, "msg1", "STARRED")

	callsAfterFull := len(env.Mock.GetMessageCalls)

	// Simulate: TRASH added + INBOX removed (what Gmail does for delete)
	env.SetHistory(12350,
		historyLabelAdded("msg1", "TRASH"),
		historyLabelRemoved("msg1", "INBOX"),
	)

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(1))})

	// Zero additional API calls
	callsAfterIncr := len(env.Mock.GetMessageCalls)
	assertpkg.Equal(t, callsAfterFull, callsAfterIncr, "expected 0 GetMessageRaw calls during incremental")

	// Verify label state: TRASH and STARRED remain, INBOX removed
	assertMessageHasLabel(t, env.Store, "msg1", "TRASH")
	assertMessageHasLabel(t, env.Store, "msg1", "STARRED")
	assertMessageNotHasLabel(t, env.Store, "msg1", "INBOX")
}

// TestIncrementalSyncBatchDeletions verifies that multiple deletions in a single
// history page are applied in batch.
func TestIncrementalSyncBatchDeletions(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 4, 12340, "msg1", "msg2", "msg3", "msg4")

	runFullSync(t, env)
	assertMessageCount(t, env.Store, 4)

	// Delete 3 messages in a single history page
	env.SetHistory(12350,
		historyDeleted("msg1"),
		historyDeleted("msg2"),
		historyDeleted("msg4"),
	)

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(3))})

	assertDeletedFromSource(t, env.Store, "msg1", true)
	assertDeletedFromSource(t, env.Store, "msg2", true)
	assertDeletedFromSource(t, env.Store, "msg3", false)
	assertDeletedFromSource(t, env.Store, "msg4", true)
}

// TestIncrementalSyncBatchNewMessages verifies that multiple new messages in a
// single history page are fetched via GetMessagesRawBatch (not one at a time).
func TestIncrementalSyncBatchNewMessages(t *testing.T) {
	env := newTestEnv(t)
	env.CreateSourceWithHistory(t, "12340")

	env.Mock.Profile.MessagesTotal = 5
	env.Mock.Profile.HistoryID = 12350
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("new-%d", i)
		env.Mock.AddMessage(id, testMIME(), []string{"INBOX"})
	}

	env.SetHistory(12350,
		historyAdded("new-1"),
		historyAdded("new-2"),
		historyAdded("new-3"),
		historyAdded("new-4"),
		historyAdded("new-5"),
	)

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(5))})
	assertMessageCount(t, env.Store, 5)
}

// TestIncrementalSyncMixedOperations tests a history page with adds, deletes,
// and label changes all at once.
func TestIncrementalSyncMixedOperations(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 2, 12340, "existing-1", "existing-2")

	runFullSync(t, env)
	assertMessageCount(t, env.Store, 2)

	callsAfterFull := len(env.Mock.GetMessageCalls)

	// Add new messages to mock
	env.Mock.AddMessage("new-1", testMIME(), []string{"INBOX"})

	// Mixed history: add a new msg, delete an existing msg, change labels on another
	env.SetHistory(12350,
		historyAdded("new-1"),
		historyDeleted("existing-1"),
		historyLabelAdded("existing-2", "STARRED"),
	)

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(3)), Added: new(int64(1))})

	// 1 new message fetched (batch), 0 for label change on existing
	callsAfterIncr := len(env.Mock.GetMessageCalls)
	// GetMessagesRawBatch calls GetMessageRaw internally in MockAPI, so 1 call for new-1
	newCalls := callsAfterIncr - callsAfterFull
	assertpkg.Equal(t, 1, newCalls, "GetMessageRaw call count for new message")

	assertDeletedFromSource(t, env.Store, "existing-1", true)
	assertMessageHasLabel(t, env.Store, "existing-2", "STARRED")
	// GetStats now applies the live-message predicate: source-deleted rows are
	// excluded. Count is 1 surviving original + 1 new = 2.
	assertMessageCount(t, env.Store, 2)
}

// TestDeriveThreadKey verifies the MIME-based thread key derivation used for
// IMAP sources that lack server-side threading.
func TestDeriveThreadKey(t *testing.T) {
	tests := []struct {
		name      string
		msg       *mime.Message
		wantKey   string
		wantEmpty bool
	}{
		{
			name:    "References uses first entry (thread root)",
			msg:     &mime.Message{References: []string{"root@ex", "mid@ex"}, InReplyTo: "<mid@ex>", MessageID: "<self@ex>"},
			wantKey: "root@ex",
		},
		{
			name:    "InReplyTo fallback when no References",
			msg:     &mime.Message{InReplyTo: "<parent@ex>", MessageID: "<self@ex>"},
			wantKey: "parent@ex",
		},
		{
			name:    "MessageID fallback for standalone",
			msg:     &mime.Message{MessageID: "<self@ex>"},
			wantKey: "self@ex",
		},
		{
			name:    "Multi-ID InReplyTo uses first entry",
			msg:     &mime.Message{InReplyTo: "<a@ex> <b@ex>", MessageID: "<self@ex>"},
			wantKey: "a@ex",
		},
		{
			name:    "InReplyTo with leading comment",
			msg:     &mime.Message{InReplyTo: "(comment) <root@ex>"},
			wantKey: "root@ex",
		},
		{
			name:    "InReplyTo with folded whitespace",
			msg:     &mime.Message{InReplyTo: "\r\n <root@ex>"},
			wantKey: "root@ex",
		},
		{
			name:      "Bare token without angle brackets ignored",
			msg:       &mime.Message{InReplyTo: "bare-token"},
			wantEmpty: true,
		},
		{
			name:      "Empty when no threading info",
			msg:       &mime.Message{},
			wantEmpty: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveThreadKey(tt.msg)
			if tt.wantEmpty {
				assertpkg.Empty(t, got, "expected empty")
			} else {
				assertpkg.Equal(t, tt.wantKey, got, "thread key")
			}
		})
	}
}

// TestIMAPThreading verifies that IMAP messages sharing an email thread
// (via References/In-Reply-To headers) are grouped into the same conversation.
func TestIMAPThreading(t *testing.T) {
	env := newTestEnv(t)
	env.SetOptions(t, func(o *Options) {
		o.SourceType = "imap"
	})

	// Build three messages in a thread:
	// msg-root -> msg-reply -> msg-reply2
	rootMIME := testemail.NewMessage().
		Subject("Thread root").
		Header("Message-ID", "<root@example.com>").
		Body("Root message.").
		Bytes()

	replyMIME := testemail.NewMessage().
		Subject("Re: Thread root").
		Header("Message-ID", "<reply@example.com>").
		Header("In-Reply-To", "<root@example.com>").
		Header("References", "<root@example.com>").
		Body("Reply message.").
		Bytes()

	reply2MIME := testemail.NewMessage().
		Subject("Re: Thread root").
		Header("Message-ID", "<reply2@example.com>").
		Header("In-Reply-To", "<reply@example.com>").
		Header("References", "<root@example.com> <reply@example.com>").
		Body("Second reply.").
		Bytes()

	// Standalone message (no threading headers except Message-ID)
	standaloneMIME := testemail.NewMessage().
		Subject("Unrelated").
		Header("Message-ID", "<standalone@example.com>").
		Body("Standalone message.").
		Bytes()

	env.Mock.Profile.MessagesTotal = 4
	env.Mock.Profile.HistoryID = 100
	env.Mock.AddMessage("INBOX|1", rootMIME, []string{"INBOX"})
	env.Mock.AddMessage("INBOX|2", replyMIME, []string{"INBOX"})
	env.Mock.AddMessage("INBOX|3", reply2MIME, []string{"INBOX"})
	env.Mock.AddMessage("INBOX|4", standaloneMIME, []string{"INBOX"})

	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(4))})

	// All three thread messages should share the same conversation
	// (thread key = References[0] = root@example.com, brackets stripped)
	assertThreadSourceID(t, env.Store, "INBOX|1", "root@example.com")
	assertThreadSourceID(t, env.Store, "INBOX|2", "root@example.com")
	assertThreadSourceID(t, env.Store, "INBOX|3", "root@example.com")

	// Standalone should use its own Message-ID (brackets stripped)
	assertThreadSourceID(t, env.Store, "INBOX|4", "standalone@example.com")

	// Verify conversation grouping: thread msgs share 1 conversation,
	// standalone gets its own.
	var convCount int
	err := env.Store.DB().QueryRow(`SELECT COUNT(DISTINCT conversation_id) FROM messages`).Scan(&convCount)
	requirepkg.NoError(t, err, "count conversations")
	assertpkg.Equal(t, 2, convCount, "expected 2 conversations (1 thread + 1 standalone)")
}

// TestIMAPCrossSyncDedup verifies that a message imported from one mailbox
// is not re-imported when it appears under a different mailbox|uid on a
// subsequent sync (e.g. moved from All Mail to Trash).
func TestIMAPCrossSyncDedup(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	env := newTestEnv(t)
	env.SetOptions(t, func(o *Options) {
		o.SourceType = "imap"
	})

	msg := testemail.NewMessage().
		Subject("Dedup test").
		Header("Message-ID", "<dedup@example.com>").
		Body("Same message, different mailbox.").
		Bytes()

	// First sync: message is in INBOX
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 100
	env.Mock.AddMessage("INBOX|42", msg, []string{"INBOX"})
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	assertMessageHasLabel(t, env.Store, "INBOX|42", "INBOX")

	// Second sync: message moved to Trash (different composite ID)
	delete(env.Mock.Messages, "INBOX|42")
	env.Mock.AddMessage("TRASH|99", msg, []string{"TRASH"})
	summary = runFullSync(t, env)
	// Should be skipped via RFC822 Message-ID dedup, not re-imported
	assertSummary(t, summary, WantSummary{Added: new(int64(0))})

	// Only one message should exist in the database
	var count int
	err := env.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM messages`).Scan(&count)
	require.NoError(err, "count messages")
	assert.Equal(1, count, "expected 1 message (duplicate imported)")

	// The existing row's source_message_id should be updated to the
	// new composite ID so future syncs don't re-download the message.
	var srcMsgID string
	err = env.Store.DB().QueryRow(
		`SELECT source_message_id FROM messages LIMIT 1`,
	).Scan(&srcMsgID)
	require.NoError(err, "get source_message_id")
	assert.Equal("TRASH|99", srcMsgID, "source_message_id not updated")

	// Labels should reflect the new mailbox.
	assertMessageHasLabel(t, env.Store, "TRASH|99", "TRASH")
	assertMessageNotHasLabel(t, env.Store, "TRASH|99", "INBOX")
}

// TestIncrementalSyncLabelRemovedWithMissingRaw verifies that removing a label
// from a message whose raw MIME data is missing still succeeds. The label-removal
// path operates on the message_labels table directly and never touches raw data.
func TestIncrementalSyncLabelRemovedWithMissingRaw(t *testing.T) {
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12340
	env.Mock.AddMessage("msg1", testMIME(), []string{"INBOX", "STARRED"})

	runFullSync(t, env)

	// Verify starting state
	assertMessageHasLabel(t, env.Store, "msg1", "STARRED")
	assertRawDataExists(t, env.Store, "msg1")

	// Delete raw MIME data to simulate missing raw
	_, err := env.Store.DB().Exec(`
		DELETE FROM message_raw WHERE message_id = (
			SELECT id FROM messages WHERE source_message_id = 'msg1'
		)`)
	requirepkg.NoError(t, err, "delete raw data")

	// Record raw fetch count before incremental sync
	callsBeforeIncr := len(env.Mock.GetMessageCalls)

	// Now simulate label removal via incremental sync
	env.SetHistory(12350, historyLabelRemoved("msg1", "STARRED"))

	summary := runIncrementalSync(t, env)
	assertSummary(t, summary, WantSummary{Found: new(int64(1))})

	// No raw fetches should occur for label-only changes
	callsAfterIncr := len(env.Mock.GetMessageCalls)
	assertpkg.Equal(t, callsBeforeIncr, callsAfterIncr, "expected 0 GetMessageRaw calls for label removal")

	// Label should be removed despite missing raw data
	assertMessageNotHasLabel(t, env.Store, "msg1", "STARRED")
	assertMessageHasLabel(t, env.Store, "msg1", "INBOX")
}
