package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/remoteimage"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func newDeferredLabelIMAPClient(
	t *testing.T, addr string, clientOpts ...imapclient.Option,
) *imapclient.Client {
	t.Helper()
	host, portString, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portString)
	require.NoError(t, err)
	client := imapclient.NewClient(&imapclient.Config{
		Host: host, Port: port, Username: testutil.IMAPTestUsername,
	}, testutil.IMAPTestPassword, clientOpts...)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func alternativeRelocationRaw(
	messageID, subject, from, to, cc, textBody, htmlBody string,
) []byte {
	return []byte(fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nCc: %s\r\nSubject: %s\r\n"+
			"Date: Mon, 01 Jan 2024 12:00:00 +0000\r\n"+
			"Message-ID: <%s>\r\nMIME-Version: 1.0\r\n"+
			"Content-Type: multipart/alternative; boundary=alt\r\n\r\n"+
			"--alt\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n"+
			"--alt\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s\r\n"+
			"--alt--\r\n",
		from, to, cc, subject, messageID, textBody, htmlBody,
	))
}

func TestIMAPRelocationAdoptsEditedSentSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP

	const messageID = "edited-draft@example.com"
	draftRaw := testemail.NewMessage().
		Subject("Draft subject").
		Header("Message-ID", "<"+messageID+">").
		Body("draftword").
		CRLF().
		Bytes()
	sentRaw := alternativeRelocationRaw(
		messageID,
		"Sent subject",
		"Final Sender <final-sender@example.com>",
		"Final Recipient <final-recipient@example.com>",
		"Final Copy <final-copy@example.com>",
		"finalword",
		"<p>htmlfinal</p>",
	)

	addr, user := testutil.StartIMAPMemServerWithSpecialUse(
		t,
		map[string]int{"Brouillons": 0, "Envoyes": 0, "INBOX": 0},
		map[string][]imapv2.MailboxAttr{
			"Brouillons": {imapv2.MailboxAttrDrafts},
			"Envoyes":    {imapv2.MailboxAttrSent},
		},
	)
	testutil.AppendIMAPRawMessage(t, user, "Brouillons", draftRaw)

	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1))})
	require.NoError(firstClient.Close())

	var initialID int64
	require.NoError(env.Store.DB().QueryRow(
		`SELECT id FROM messages WHERE source_message_id = ?`,
		"Brouillons|1",
	).Scan(&initialID))

	testutil.AppendIMAPRawMessage(t, user, "Envoyes", sentRaw)
	testutil.ExpungeIMAPMessage(t, addr, "Brouillons", imapv2.UID(1))

	secondClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(secondClient, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(0)),
		Updated: new(int64(1)),
	})
	require.NoError(secondClient.Close())

	message, err := env.Store.GetMessage(initialID)
	require.NoError(err)
	assert.Equal("Envoyes|1", message.SourceMessageID)
	assert.Equal("Sent subject", message.Subject)
	assert.Equal("final-sender@example.com", message.FromEmail)
	assert.Equal([]string{"Final Recipient <final-recipient@example.com>"}, message.To)
	assert.Equal([]string{"Final Copy <final-copy@example.com>"}, message.Cc)
	assert.Contains(message.BodyText, "finalword")
	assert.NotContains(message.BodyText, "draftword")
	assert.Contains(message.BodyHTML, "htmlfinal")
	assert.Empty(message.Snippet)
	assert.Equal(int64(len(sentRaw)), message.SizeEstimate)
	assert.ElementsMatch([]string{"Envoyes"}, message.Labels)
	raw, err := env.Store.GetMessageRaw(initialID)
	require.NoError(err)
	assert.Equal(sentRaw, raw)
	assertMessageCount(t, env.Store, 1)
	results, total, err := env.Store.SearchMessagesQuery(
		&search.Query{TextTerms: []string{"finalword"}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	require.Len(results, 1)
	assert.Equal(initialID, results[0].ID)
	_, total, err = env.Store.SearchMessagesQuery(
		&search.Query{TextTerms: []string{"draftword"}}, 0, 10)
	require.NoError(err)
	assert.Zero(total)

	thirdClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(thirdClient, env.Store, opts)
	summary = runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added:   new(int64(0)),
		Updated: new(int64(0)),
	})
	message, err = env.Store.GetMessage(initialID)
	require.NoError(err)
	assert.Contains(message.BodyText, "finalword")
	assertMessageCount(t, env.Store, 1)
	require.NoError(thirdClient.Close())
}

func TestIMAPPartialRelocationMergesLabelsAndRefreshesContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	const messageID = "partial-relocation@example.com"
	draftRaw := testemail.NewMessage().
		Subject("Partial draft").
		Header("Message-ID", "<"+messageID+">").
		Body("draftword").CRLF().Bytes()
	finalRaw := testemail.NewMessage().
		Subject("Partial final").
		Header("Message-ID", "<"+messageID+">").
		Body("finalword").CRLF().Bytes()
	addr, user := testutil.StartIMAPMemServerWithSpecialUse(
		t,
		map[string]int{"Brouillons": 0, "Envoyes": 0, "INBOX": 0},
		map[string][]imapv2.MailboxAttr{"Envoyes": {imapv2.MailboxAttrSent}},
	)
	testutil.AppendIMAPRawMessage(t, user, "Brouillons", draftRaw)
	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{Added: new(int64(1))})
	require.NoError(firstClient.Close())

	var internalID int64
	require.NoError(env.Store.DB().QueryRow(
		`SELECT id FROM messages WHERE source_message_id = ?`, "Brouillons|1",
	).Scan(&internalID))
	source, err := env.Store.GetSourceByIdentifier(testEmail)
	require.NoError(err)
	archiveLabelID, err := env.Store.EnsureLabel(source.ID, "Archive", "Archive", "user")
	require.NoError(err)
	_, err = env.Store.DB().Exec(
		`INSERT INTO message_labels (message_id, label_id) VALUES (?, ?)`,
		internalID, archiveLabelID)
	require.NoError(err)

	testutil.AppendIMAPRawMessage(t, user, "Envoyes", finalRaw)
	testutil.ExpungeIMAPMessage(t, addr, "Brouillons", imapv2.UID(1))
	opts.Limit = 1
	partialClient := newSyncTestIMAPClient(
		t, addr,
		imapclient.WithFolderFilter([]string{"Brouillons", "Envoyes"}, nil),
	)
	env.Syncer = New(partialClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added: new(int64(0)), Updated: new(int64(1)), Errors: new(int64(0)),
	})
	message, err := env.Store.GetMessage(internalID)
	require.NoError(err)
	assert.Equal("Envoyes|1", message.SourceMessageID)
	assert.Equal("Partial final", message.Subject)
	assert.Contains(message.BodyText, "finalword")
	assert.ElementsMatch([]string{"Archive", "Brouillons", "Envoyes"}, message.Labels)
	require.NoError(partialClient.Close())
}

func TestIMAPDeferredRelocationPreservesLabelsUntilFinalization(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	const messageID = "deferred-relocation@example.com"
	draftRaw := testemail.NewMessage().
		Subject("Deferred draft").
		Header("Message-ID", "<"+messageID+">").
		Body("draftword").CRLF().Bytes()
	finalRaw := testemail.NewMessage().
		Subject("Deferred final").
		Header("Message-ID", "<"+messageID+">").
		Body("finalword").CRLF().Bytes()
	addr, user := testutil.StartIMAPMemServerWithSpecialUse(
		t,
		map[string]int{"Brouillons": 0, "Envoyes": 0, "INBOX": 0},
		map[string][]imapv2.MailboxAttr{"Envoyes": {imapv2.MailboxAttrSent}},
	)
	testutil.AppendIMAPRawMessage(t, user, "Brouillons", draftRaw)
	firstClient := newDeferredLabelIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{Added: new(int64(1))})
	require.NoError(firstClient.Close())
	var internalID int64
	require.NoError(env.Store.DB().QueryRow(
		`SELECT id FROM messages WHERE source_message_id = ?`, "Brouillons|1",
	).Scan(&internalID))

	testutil.AppendIMAPRawMessage(t, user, "Envoyes", finalRaw)
	testutil.ExpungeIMAPMessage(t, addr, "Brouillons", imapv2.UID(1))
	secondClient := newDeferredLabelIMAPClient(t, addr)
	env.Syncer = New(secondClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{
		Added: new(int64(0)), Updated: new(int64(1)), Errors: new(int64(0)),
	})
	message, err := env.Store.GetMessage(internalID)
	require.NoError(err)
	assert.Equal("Envoyes|1", message.SourceMessageID)
	assert.Contains(message.BodyText, "finalword")
	assert.ElementsMatch([]string{"Brouillons"}, message.Labels,
		"Syncer leaves labels untouched until the command-layer mailbox delta transaction")
	require.NoError(secondClient.Close())
}

// Preferred \All adoption rekeys the canonical location but must not replace
// the archived snapshot: \All placement holds received mail too, so a forged
// same-Message-ID copy mirrored there carries no provider-authored evidence.
// The rekey still adopts the stable canonical ID and keeps every attachment.
func TestIMAPPreferredAllMailAdoptionPreservesSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	opts.AttachmentsDir = filepath.Join(env.TmpDir, "attachments")
	const messageID = "preferred-relocation@example.com"
	draftRaw := testemail.NewMessage().
		Subject("Preferred draft").
		Header("Message-ID", "<"+messageID+">").
		Body("draftword").
		WithAttachment("old.bin", "application/octet-stream", []byte("old bytes")).
		CRLF().Bytes()
	finalRaw := testemail.NewMessage().
		From("Final Sender <final@example.com>").
		To("Final Recipient <recipient@example.com>").
		Subject("Preferred final").
		Header("Message-ID", "<"+messageID+">").
		Body("finalword").
		CRLF().Bytes()
	addr, user := testutil.StartIMAPMemServerWithSpecialUse(
		t,
		map[string]int{"All Mail": 0, "INBOX": 0},
		map[string][]imapv2.MailboxAttr{"All Mail": {imapv2.MailboxAttrAll}},
	)
	testutil.AppendIMAPRawMessage(t, user, "INBOX", draftRaw)
	firstClient := newSyncTestIMAPClient(
		t, addr, imapclient.WithFolderFilter([]string{"INBOX"}, nil))
	env.Syncer = New(firstClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{Added: new(int64(1))})
	require.NoError(firstClient.Close())

	var internalID int64
	require.NoError(env.Store.DB().QueryRow(
		`SELECT id FROM messages WHERE source_message_id = ?`, "INBOX|1",
	).Scan(&internalID))
	require.NoError(env.Store.UpsertAttachmentRecord(t.Context(), internalID, store.AttachmentWrite{
		Filename: "provider.bin", MIMEType: "application/octet-stream",
		StoragePath: "provider.bin", ContentHash: "provider-hash", Size: 8,
		SourceAttachmentID: "provider:1", SourcePartKey: "provider:part:1",
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceProviderExplicit,
	}))
	require.NoError(env.Store.RecomputeMessageAttachmentStats(internalID))

	testutil.AppendIMAPRawMessage(t, user, "All Mail", finalRaw)
	secondClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(secondClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{
		Added: new(int64(0)), Updated: new(int64(1)), Errors: new(int64(0)),
	})
	message, err := env.Store.GetMessage(internalID)
	require.NoError(err)
	assert.Equal("All Mail|1", message.SourceMessageID)
	assert.Equal("Preferred draft", message.Subject)
	assert.Contains(message.BodyText, "draftword")
	assert.NotContains(message.BodyText, "finalword")
	assert.NotEqual("final@example.com", message.FromEmail)
	raw, err := env.Store.GetMessageRaw(internalID)
	require.NoError(err)
	assert.Equal(draftRaw, raw)
	assert.ElementsMatch([]string{"All Mail", "INBOX"}, message.Labels)
	require.Len(message.Attachments, 2)
	filenames := []string{message.Attachments[0].Filename, message.Attachments[1].Filename}
	assert.ElementsMatch([]string{"old.bin", "provider.bin"}, filenames)
	assert.True(message.HasAttachments)
	var providerCount int
	require.NoError(env.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM attachments WHERE message_id = ? AND source_attachment_id = ?`,
		internalID, "provider:1",
	).Scan(&providerCount))
	assert.Equal(1, providerCount)
	results, total, err := env.Store.SearchMessagesQuery(
		&search.Query{TextTerms: []string{"draftword"}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	require.Len(results, 1)
	assert.Equal(internalID, results[0].ID)
	_, total, err = env.Store.SearchMessagesQuery(
		&search.Query{TextTerms: []string{"finalword"}}, 0, 10)
	require.NoError(err)
	assert.Zero(total, "the unverified copy must not reach the search index")
	var attachmentCount int
	require.NoError(env.Store.DB().QueryRow(
		`SELECT attachment_count FROM messages WHERE id = ?`, internalID,
	).Scan(&attachmentCount))
	assert.Equal(2, attachmentCount)
	require.NoError(secondClient.Close())
}

func TestIMAPRelocationSQLFailureRetainsSnapshotAndRetries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to inject relocation failure")
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	opts.AttachmentsDir = filepath.Join(env.TmpDir, "attachments")
	const messageID = "retry-relocation@example.com"
	draftRaw := testemail.NewMessage().Subject("Retry draft").
		Header("Message-ID", "<"+messageID+">").Body("draftword").CRLF().Bytes()
	finalRaw := testemail.NewMessage().Subject("Retry final").
		Header("Message-ID", "<"+messageID+">").Body("finalword").
		WithAttachment("retry.bin", "application/octet-stream", []byte("retry bytes")).
		CRLF().Bytes()
	addr, user := testutil.StartIMAPMemServerWithSpecialUse(
		t,
		map[string]int{"Brouillons": 0, "Envoyes": 0, "INBOX": 0},
		map[string][]imapv2.MailboxAttr{"Envoyes": {imapv2.MailboxAttrSent}},
	)
	testutil.AppendIMAPRawMessage(t, user, "Brouillons", draftRaw)
	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{Added: new(int64(1))})
	require.NoError(firstClient.Close())
	var internalID int64
	require.NoError(env.Store.DB().QueryRow(
		`SELECT id FROM messages WHERE source_message_id = ?`, "Brouillons|1",
	).Scan(&internalID))
	before, err := env.Store.GetMessage(internalID)
	require.NoError(err)
	beforeRaw, err := env.Store.GetMessageRaw(internalID)
	require.NoError(err)
	parsedFinal, err := mime.Parse(finalRaw)
	require.NoError(err)
	require.Len(parsedFinal.Attachments, 1)

	testutil.AppendIMAPRawMessage(t, user, "Envoyes", finalRaw)
	testutil.ExpungeIMAPMessage(t, addr, "Brouillons", imapv2.UID(1))
	_, err = env.Store.DB().Exec(fmt.Sprintf(`
		CREATE TRIGGER fail_imap_relocation
		BEFORE UPDATE ON messages
		WHEN OLD.id = %d AND NEW.source_message_id = 'Envoyes|1'
		BEGIN
			SELECT RAISE(ABORT, 'relocation unavailable');
		END`, internalID))
	require.NoError(err)
	t.Cleanup(func() { _, _ = env.Store.DB().Exec(`DROP TRIGGER IF EXISTS fail_imap_relocation`) })

	acknowledged := make(map[string]imapclient.FolderState)
	failedClient := newSyncTestIMAPClient(t, addr, imapclient.WithFolderStateSave(
		func(mailbox string, state imapclient.FolderState) { acknowledged[mailbox] = state },
	))
	env.Syncer = New(failedClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{
		Added: new(int64(0)), Updated: new(int64(0)), Errors: new(int64(1)),
	})
	afterFailure, err := env.Store.GetMessage(internalID)
	require.NoError(err)
	afterFailureRaw, err := env.Store.GetMessageRaw(internalID)
	require.NoError(err)
	assert.Equal(before.SourceMessageID, afterFailure.SourceMessageID)
	assert.Equal(before.Subject, afterFailure.Subject)
	assert.Equal(before.BodyText, afterFailure.BodyText)
	assert.Equal(before.Labels, afterFailure.Labels)
	assert.Equal(beforeRaw, afterFailureRaw)
	assert.NotContains(acknowledged, "Envoyes")
	blobPath := filepath.Join(
		opts.AttachmentsDir,
		parsedFinal.Attachments[0].ContentHash[:2],
		parsedFinal.Attachments[0].ContentHash,
	)
	assert.FileExists(blobPath, "failed relocation retains a published CAS blob")
	require.NoError(failedClient.Close())

	_, err = env.Store.DB().Exec(`DROP TRIGGER fail_imap_relocation`)
	require.NoError(err)
	retryClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(retryClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{
		Added: new(int64(0)), Updated: new(int64(1)), Errors: new(int64(0)),
	})
	afterRetry, err := env.Store.GetMessage(internalID)
	require.NoError(err)
	assert.Equal("Envoyes|1", afterRetry.SourceMessageID)
	assert.Equal("Retry final", afterRetry.Subject)
	assert.Contains(afterRetry.BodyText, "finalword")
	require.NoError(retryClient.Close())
}

func TestIMAPRelocationStrictParseFailurePreservesOldSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	const messageID = "invalid-relocation@example.com"
	draftRaw := testemail.NewMessage().Subject("Valid draft").
		Header("Message-ID", "<"+messageID+">").Body("draftword").CRLF().Bytes()
	invalidRaw := []byte(
		"From: invalid-final@example.com\r\nTo: invalid-recipient@example.com\r\n" +
			"Subject: Invalid final\r\nMessage-ID: <" + messageID + ">\r\n" +
			"MIME-Version: 1.0\r\nContent-Type: multipart mixed; boundary=outer\r\n\r\n" +
			"--outer\r\nContent-Type: text/plain\r\n\r\nfinalword\r\n--outer--\r\n")
	addr, user := testutil.StartIMAPMemServerWithSpecialUse(
		t,
		map[string]int{"Brouillons": 0, "Envoyes": 0, "INBOX": 0},
		map[string][]imapv2.MailboxAttr{"Envoyes": {imapv2.MailboxAttrSent}},
	)
	testutil.AppendIMAPRawMessage(t, user, "Brouillons", draftRaw)
	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{Added: new(int64(1))})
	require.NoError(firstClient.Close())
	var internalID int64
	require.NoError(env.Store.DB().QueryRow(
		`SELECT id FROM messages WHERE source_message_id = ?`, "Brouillons|1",
	).Scan(&internalID))
	var participantsBefore int
	require.NoError(env.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM participants`,
	).Scan(&participantsBefore))
	testutil.AppendIMAPRawMessage(t, user, "Envoyes", invalidRaw)
	testutil.ExpungeIMAPMessage(t, addr, "Brouillons", imapv2.UID(1))
	secondClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(secondClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{
		Added: new(int64(0)), Updated: new(int64(0)), Errors: new(int64(1)),
	})
	message, err := env.Store.GetMessage(internalID)
	require.NoError(err)
	assert.Equal("Brouillons|1", message.SourceMessageID)
	assert.Equal("Valid draft", message.Subject)
	assert.Contains(message.BodyText, "draftword")
	raw, err := env.Store.GetMessageRaw(internalID)
	require.NoError(err)
	assert.Equal(draftRaw, raw)
	var participantsAfter int
	require.NoError(env.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM participants`,
	).Scan(&participantsAfter))
	assert.Equal(participantsBefore, participantsAfter,
		"strict relocation failure must not resolve prepared participants")
	require.NoError(secondClient.Close())
}

func TestIMAPRelocationWithoutAttachmentsDirPreservesSnapshotAndRetries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	opts.AttachmentsDir = ""
	const messageID = "attachment-failure-relocation@example.com"
	draftRaw := testemail.NewMessage().Subject("Attachment draft").
		Header("Message-ID", "<"+messageID+">").Body("draftword").CRLF().Bytes()
	finalRaw := testemail.NewMessage().Subject("Attachment final").
		Header("Message-ID", "<"+messageID+">").Body("finalword").
		WithAttachment("final.bin", "application/octet-stream", []byte("final bytes")).
		CRLF().Bytes()
	addr, user := testutil.StartIMAPMemServerWithSpecialUse(
		t,
		map[string]int{"Brouillons": 0, "Envoyes": 0, "INBOX": 0},
		map[string][]imapv2.MailboxAttr{"Envoyes": {imapv2.MailboxAttrSent}},
	)
	testutil.AppendIMAPRawMessage(t, user, "Brouillons", draftRaw)
	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{Added: new(int64(1))})
	require.NoError(firstClient.Close())
	var internalID int64
	require.NoError(env.Store.DB().QueryRow(
		`SELECT id FROM messages WHERE source_message_id = ?`, "Brouillons|1",
	).Scan(&internalID))

	testutil.AppendIMAPRawMessage(t, user, "Envoyes", finalRaw)
	testutil.ExpungeIMAPMessage(t, addr, "Brouillons", imapv2.UID(1))
	acknowledged := make(map[string]imapclient.FolderState)
	secondClient := newSyncTestIMAPClient(t, addr, imapclient.WithFolderStateSave(
		func(mailbox string, state imapclient.FolderState) { acknowledged[mailbox] = state },
	))
	env.Syncer = New(secondClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{
		Added: new(int64(0)), Updated: new(int64(0)), Errors: new(int64(1)),
	})
	message, err := env.Store.GetMessage(internalID)
	require.NoError(err)
	assert.Equal("Brouillons|1", message.SourceMessageID)
	assert.Equal("Attachment draft", message.Subject)
	assert.Contains(message.BodyText, "draftword")
	raw, err := env.Store.GetMessageRaw(internalID)
	require.NoError(err)
	assert.Equal(draftRaw, raw)
	assert.NotContains(acknowledged, "Envoyes")
	require.NoError(secondClient.Close())

	opts.AttachmentsDir = filepath.Join(env.TmpDir, "attachments")
	retryClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(retryClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{
		Added: new(int64(0)), Updated: new(int64(1)), Errors: new(int64(0)),
	})
	afterRetry, err := env.Store.GetMessage(internalID)
	require.NoError(err)
	assert.Equal("Envoyes|1", afterRetry.SourceMessageID)
	assert.Equal("Attachment final", afterRetry.Subject)
	assert.Contains(afterRetry.BodyText, "finalword")
	require.Len(afterRetry.Attachments, 1)
	assert.Equal("final.bin", afterRetry.Attachments[0].Filename)
	afterRetryRaw, err := env.Store.GetMessageRaw(internalID)
	require.NoError(err)
	assert.Equal(finalRaw, afterRetryRaw)
	require.NoError(retryClient.Close())
}

// A relocated snapshot's refreshed HTML must go through the same remote-image
// archival hook as ordinary ingest, so an edited copy that gains tracked image
// URLs archives them under the retained internal ID.
func TestIMAPRelocationArchivesRemoteImages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nsynthetic"))
	}))
	defer upstream.Close()
	fetcher := remoteimage.NewFetcher()
	fetcher.LookupNetIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	fetcher.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "http://"))
	}

	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	opts.AttachmentsDir = t.TempDir()
	opts.RemoteImages = fetcher

	const messageID = "remote-image-relocation@example.com"
	draftRaw := testemail.NewMessage().
		Subject("Draft subject").
		Header("Message-ID", "<"+messageID+">").
		Body("draftword").
		CRLF().
		Bytes()
	sentRaw := alternativeRelocationRaw(
		messageID,
		"Sent subject",
		"Final Sender <final-sender@example.com>",
		"Final Recipient <final-recipient@example.com>",
		"Final Copy <final-copy@example.com>",
		"finalword",
		"<p>htmlfinal <img src=\"http://images.example/chart\"></p>",
	)
	addr, user := testutil.StartIMAPMemServerWithSpecialUse(
		t,
		map[string]int{"Brouillons": 0, "Envoyes": 0, "INBOX": 0},
		map[string][]imapv2.MailboxAttr{
			"Brouillons": {imapv2.MailboxAttrDrafts},
			"Envoyes":    {imapv2.MailboxAttrSent},
		},
	)
	testutil.AppendIMAPRawMessage(t, user, "Brouillons", draftRaw)
	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{Added: new(int64(1))})
	require.NoError(firstClient.Close())
	var internalID int64
	require.NoError(env.Store.DB().QueryRow(
		`SELECT id FROM messages WHERE source_message_id = ?`, "Brouillons|1",
	).Scan(&internalID))
	refs, err := env.Store.MessageRemoteImages(internalID)
	require.NoError(err)
	assert.Empty(refs, "draft without tracked images archives nothing")
	assert.Zero(requests.Load())

	testutil.AppendIMAPRawMessage(t, user, "Envoyes", sentRaw)
	testutil.ExpungeIMAPMessage(t, addr, "Brouillons", imapv2.UID(1))
	secondClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(secondClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{
		Added:   new(int64(0)),
		Updated: new(int64(1)),
	})
	require.NoError(secondClient.Close())

	message, err := env.Store.GetMessage(internalID)
	require.NoError(err)
	assert.Equal("Envoyes|1", message.SourceMessageID)
	assert.Contains(message.BodyHTML, "images.example/chart")
	refs, err = env.Store.MessageRemoteImages(internalID)
	require.NoError(err)
	digest := sha256.Sum256([]byte("http://images.example/chart"))
	key := "remote-image:" + hex.EncodeToString(digest[:])
	_, archived := refs[key]
	assert.True(archived, "relocated snapshot archives its remote image")
	assert.Equal(int64(1), requests.Load())

	thirdClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(thirdClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{
		Added:   new(int64(0)),
		Updated: new(int64(0)),
	})
	require.NoError(thirdClient.Close())
	refs, err = env.Store.MessageRemoteImages(internalID)
	require.NoError(err)
	assert.Len(refs, 1)
	assert.Equal(int64(1), requests.Load(), "repeat sync must not refetch the archived image")
}

// An untrusted destination adopts only the location: the rejected survivor's
// HTML must not reach the remote-image fetcher, because processing bytes a
// sender controls would leak requests to sender-chosen servers.
func TestIMAPRelocationUntrustedDestinationSkipsRemoteImages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nsynthetic"))
	}))
	defer upstream.Close()
	fetcher := remoteimage.NewFetcher()
	fetcher.LookupNetIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	fetcher.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "http://"))
	}

	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	opts.AttachmentsDir = t.TempDir()
	opts.RemoteImages = fetcher

	const messageID = "untrusted-remote-images@example.com"
	// No special-use advertisement: the survivor's placement is untrusted.
	addr, user := testutil.StartIMAPMemServer(t, map[string]int{
		"Brouillons": 0, "Envoyes": 0, "INBOX": 0,
	})
	draftRaw := testemail.NewMessage().
		Subject("Untrusted draft").
		Header("Message-ID", "<"+messageID+">").
		Body("draftword").CRLF().Bytes()
	testutil.AppendIMAPRawMessage(t, user, "Brouillons", draftRaw)
	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{Added: new(int64(1))})
	require.NoError(firstClient.Close())
	var internalID int64
	require.NoError(env.Store.DB().QueryRow(
		`SELECT id FROM messages WHERE source_message_id = ?`, "Brouillons|1",
	).Scan(&internalID))

	finalRaw := alternativeRelocationRaw(
		messageID,
		"Untrusted final",
		"Final Sender <final-sender@example.com>",
		"Final Recipient <final-recipient@example.com>",
		"Final Copy <final-copy@example.com>",
		"finalword",
		"<p>htmlfinal <img src=\"http://images.example/chart\"></p>",
	)
	testutil.AppendIMAPRawMessage(t, user, "Envoyes", finalRaw)
	testutil.ExpungeIMAPMessage(t, addr, "Brouillons", imapv2.UID(1))
	secondClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(secondClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Errors: new(int64(0))})
	require.NoError(secondClient.Close())

	message, err := env.Store.GetMessage(internalID)
	require.NoError(err)
	assert.Equal("Envoyes|1", message.SourceMessageID)
	assert.Contains(message.BodyText, "draftword", "the snapshot is preserved")
	assert.NotContains(message.BodyText, "finalword")
	raw, err := env.Store.GetMessageRaw(internalID)
	require.NoError(err)
	assert.Equal(draftRaw, raw)
	refs, err := env.Store.MessageRemoteImages(internalID)
	require.NoError(err)
	assert.Empty(refs, "rejected bytes must not reach remote-image processing")
	assert.Zero(requests.Load())
}

// A preferred adoption whose label reconciliation fails late must keep the
// old composite key: the rekey and the label write share one guarded
// transaction, so a failing or canceled label step rolls the adoption back
// instead of consuming the key the next retry needs.
func TestIMAPPreferredAdoptionKeepsOldKeyOnLabelFailure(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to fail the label write")
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP

	const messageID = "preferred-atomic@example.com"
	raw := testemail.NewMessage().
		Subject("Atomic preferred").
		Header("Message-ID", "<"+messageID+">").
		Body("atomicword").CRLF().Bytes()
	addr, user := testutil.StartIMAPMemServerWithSpecialUse(
		t,
		map[string]int{"All Mail": 0, "INBOX": 0},
		map[string][]imapv2.MailboxAttr{"All Mail": {imapv2.MailboxAttrAll}},
	)
	testutil.AppendIMAPRawMessage(t, user, "INBOX", raw)
	firstClient := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(firstClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{Added: new(int64(1))})
	require.NoError(firstClient.Close())
	var internalID int64
	require.NoError(env.Store.DB().QueryRow(
		`SELECT id FROM messages WHERE source_message_id = ?`, "INBOX|1",
	).Scan(&internalID))

	// The immediate-label client runs the preferred adoption's label step in
	// the same call as the rekey; the trigger fails every label write for the
	// guarded row.
	testutil.AppendIMAPRawMessage(t, user, "All Mail", raw)
	_, triggerErr := env.Store.DB().Exec(fmt.Sprintf(`
		CREATE TRIGGER fail_preferred_labels_insert
		BEFORE INSERT ON message_labels
		WHEN NEW.message_id = %d
		BEGIN
			SELECT RAISE(ABORT, 'preferred label write unavailable');
		END`, internalID))
	require.NoError(triggerErr)
	_, triggerErr = env.Store.DB().Exec(fmt.Sprintf(`
		CREATE TRIGGER fail_preferred_labels_delete
		BEFORE DELETE ON message_labels
		WHEN OLD.message_id = %d
		BEGIN
			SELECT RAISE(ABORT, 'preferred label write unavailable');
		END`, internalID))
	require.NoError(triggerErr)
	t.Cleanup(func() {
		_, _ = env.Store.DB().Exec(`DROP TRIGGER IF EXISTS fail_preferred_labels_insert`)
		_, _ = env.Store.DB().Exec(`DROP TRIGGER IF EXISTS fail_preferred_labels_delete`)
	})

	immediateClient := &immediateLabelIMAPClient{Client: newSyncTestIMAPClient(t, addr).Client}
	env.Syncer = New(immediateClient, env.Store, opts)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Errors: new(int64(1))})

	message, err := env.Store.GetMessage(internalID)
	require.NoError(err)
	assert.Equal("INBOX|1", message.SourceMessageID,
		"a failed preferred adoption must not consume the old composite key")
	assert.Equal([]string{"INBOX"}, message.Labels,
		"a failed preferred adoption must not mutate labels")
	assert.Contains(message.BodyText, "atomicword")

	// Removing the fault lets the retry adopt key and labels together.
	_, err = env.Store.DB().Exec(`DROP TRIGGER fail_preferred_labels_insert`)
	require.NoError(err)
	_, err = env.Store.DB().Exec(`DROP TRIGGER fail_preferred_labels_delete`)
	require.NoError(err)
	retryClient := &immediateLabelIMAPClient{Client: newSyncTestIMAPClient(t, addr).Client}
	env.Syncer = New(retryClient, env.Store, opts)
	assertSummary(t, runFullSync(t, env), WantSummary{Errors: new(int64(0))})
	message, err = env.Store.GetMessage(internalID)
	require.NoError(err)
	assert.Equal("All Mail|1", message.SourceMessageID)
	assert.ElementsMatch([]string{"All Mail", "INBOX"}, message.Labels)
}
