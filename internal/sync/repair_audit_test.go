package sync

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestAuditGmailMessagesReportsCrossedFieldsAndInconclusiveRaw(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	crossedID := seedRepairRow(t, env.Store, source.ID, "gmail-a", "thread-a", "stored")
	missingID := seedRepairRow(t, env.Store, source.ID, "gmail-b", "thread-b", "missing")
	badID := seedRepairRow(t, env.Store, source.ID, "gmail-c", "thread-c", "bad")

	crossedRaw := testemail.NewMessage().
		From("other@example.com").To("different@example.com").
		Subject("other").Header("Message-ID", "<other@example.com>").Body("other body").Bytes()
	require.NoError(env.Store.UpsertMessageRaw(crossedID, crossedRaw))
	_, err := env.Store.DB().Exec(`DELETE FROM message_raw WHERE message_id = ?`, missingID)
	require.NoError(err)
	require.NoError(env.Store.UpsertMessageRaw(badID, []byte("not MIME\x00")))

	results := collectAudit(t, env.Syncer, source.ID)
	require.Len(results, 3)
	byID := auditByID(results)
	assert.Equal(AuditMismatch, byID[crossedID].Status)
	assert.Equal(AuditMismatch, byID[crossedID].Fields[AuditFieldSubject])
	assert.Equal(AuditMismatch, byID[crossedID].Fields[AuditFieldBodyText])
	assert.Equal(AuditMismatch, byID[crossedID].Fields[AuditFieldTo])
	assert.Equal(AuditInconclusive, byID[missingID].Status)
	assert.Equal(AuditInconclusive, byID[missingID].Fields[AuditFieldRawMIME])
	assert.Equal(AuditInconclusive, byID[badID].Status)
	assert.Equal(AuditInconclusive, byID[badID].Fields[AuditFieldRawMIME])
}

func TestAuditGmailMessagesTreatsParsedEmptyFieldsAsAuthoritative(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	id := seedRepairRow(t, env.Store, source.ID, "gmail-a", "thread-a", "stored")
	raw := testemail.NewMessage().NoSubject().
		Header("Message-ID", "<gmail-a@example.com>").Body("stored body").Bytes()
	require.NoError(env.Store.UpsertMessageRaw(id, raw))

	results := collectAudit(t, env.Syncer, source.ID)
	require.Len(results, 1)
	assert.Equal(AuditMismatch, results[0].Status)
	assert.Equal(AuditMismatch, results[0].Fields[AuditFieldSubject])
}

func TestAuditGmailMessagesMarksNonMIMEStoredRawInconclusive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	id := seedRepairRow(t, env.Store, source.ID, "gmail-a", "thread-a", "stored")

	_, err := env.Store.DB().Exec(
		`UPDATE message_raw SET raw_format = 'gcal_json' WHERE message_id = ?`, id)
	require.NoError(err)

	results := collectAudit(t, env.Syncer, source.ID)
	require.Len(results, 1)
	assert.Equal(AuditInconclusive, results[0].Status)
	assert.Equal(AuditInconclusive, results[0].Fields[AuditFieldRawMIME])
}

func TestAuditGmailMessagesOmitsCoherentLegacyEvidence(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	env.Mock.Messages["gmail-a"] = repairRaw("gmail-a", "thread-a", "coherent", "coherent body", []byte("legacy attachment"))
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 10
	env.SetOptions(t, func(options *Options) { options.AttachmentsDir = filepath.Join(env.TmpDir, "attachments") })
	runFullSync(t, env)
	var id int64
	require.NoError(env.Store.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = 'gmail-a'`, source.ID).Scan(&id))
	participantID, err := env.Store.EnsureParticipant("merged@example.com", "Merged", "example.com")
	require.NoError(err)
	_, err = env.Store.DB().Exec(`UPDATE messages SET sender_id = ? WHERE id = ?`, participantID, id)
	require.NoError(err)
	_, err = env.Store.DB().Exec(`UPDATE message_recipients SET email_address = NULL WHERE message_id = ?`, id)
	require.NoError(err)
	_, err = env.Store.DB().Exec(`UPDATE attachments SET source_part_key = NULL WHERE message_id = ?`, id)
	require.NoError(err)

	assert.Empty(t, collectAudit(t, env.Syncer, source.ID))
}

func TestAuditGmailMessagesComparesAttachmentHashesWithoutOccurrenceKeys(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	raw := repairRaw("gmail-a", "thread-a", "subject", "body", []byte("actual attachment"))
	env.Mock.Messages["gmail-a"] = raw
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 10
	env.SetOptions(t, func(options *Options) { options.AttachmentsDir = filepath.Join(env.TmpDir, "attachments") })
	runFullSync(t, env)
	var id int64
	require.NoError(env.Store.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = 'gmail-a'`, source.ID).Scan(&id))
	_, err := env.Store.DB().Exec(`UPDATE attachments SET content_hash = 'wrong', source_part_key = NULL WHERE message_id = ?`, id)
	require.NoError(err)

	results := collectAudit(t, env.Syncer, source.ID)
	require.Len(results, 1)
	assert.Equal(AuditMismatch, results[0].Fields[AuditFieldAttachmentHashes])
	assert.Equal(AuditInconclusive, results[0].Fields[AuditFieldAttachmentPartKeys])
}

func TestAuditGmailMessagesAttachmentHashMultisetPreservesDuplicateCounts(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	raw := testemail.NewMessage().
		Subject("duplicates").Body("body").
		WithAttachment("first.bin", "application/octet-stream", []byte("same bytes")).
		WithAttachment("second.bin", "application/octet-stream", []byte("same bytes")).Bytes()
	env.Mock.AddMessage("gmail-a", raw, []string{"INBOX"})
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 10
	env.SetOptions(t, func(options *Options) { options.AttachmentsDir = filepath.Join(env.TmpDir, "attachments") })
	runFullSync(t, env)
	var id int64
	require.NoError(env.Store.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = 'gmail-a'`, source.ID).Scan(&id))
	var attachmentID int64
	require.NoError(env.Store.DB().QueryRow(`SELECT id FROM attachments WHERE message_id = ? ORDER BY id LIMIT 1`, id).Scan(&attachmentID))
	_, err := env.Store.DB().Exec(`DELETE FROM attachments WHERE id = ?`, attachmentID)
	require.NoError(err)

	results := collectAudit(t, env.Syncer, source.ID)
	require.Len(results, 1)
	assert.Equal(t, AuditMismatch, results[0].Fields[AuditFieldAttachmentHashes])
}

func TestAuditGmailMessagesAcceptsRepeatedPartStoredOnceWithoutOccurrenceKeys(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	raw := testemail.NewMessage().
		Subject("repeated logo").Body("body").
		WithAttachment("logo.png", "image/png", []byte("same bytes")).
		WithAttachment("logo-again.png", "image/png", []byte("same bytes")).Bytes()
	env.Mock.AddMessage("gmail-a", raw, []string{"INBOX"})
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 10
	env.SetOptions(t, func(options *Options) { options.AttachmentsDir = filepath.Join(env.TmpDir, "attachments") })
	runFullSync(t, env)
	var id int64
	require.NoError(env.Store.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = 'gmail-a'`, source.ID).Scan(&id))
	// Archives written before occurrence keys existed hold one unkeyed row per
	// (message, content hash), so the repeated part was stored exactly once.
	var keep int64
	require.NoError(env.Store.DB().QueryRow(`SELECT id FROM attachments WHERE message_id = ? ORDER BY id LIMIT 1`, id).Scan(&keep))
	_, err := env.Store.DB().Exec(`DELETE FROM attachments WHERE message_id = ? AND id <> ?`, id, keep)
	require.NoError(err)
	_, err = env.Store.DB().Exec(`UPDATE attachments SET source_part_key = NULL WHERE id = ?`, keep)
	require.NoError(err)

	assert.Empty(t, collectAudit(t, env.Syncer, source.ID))
}

func TestAuditGmailMessagesTreatsLegacyOnlyAttachmentAsInconclusive(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	raw := repairRaw("gmail-a", "thread-a", "subject", "body", []byte("actual attachment"))
	env.Mock.Messages["gmail-a"] = raw
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 10
	env.SetOptions(t, func(options *Options) { options.AttachmentsDir = filepath.Join(env.TmpDir, "attachments") })
	runFullSync(t, env)
	var id int64
	require.NoError(env.Store.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = 'gmail-a'`, source.ID).Scan(&id))
	// Older ingests recorded small nameless text parts as attachments. Such a
	// row has no counterpart in the current parse but is not corruption.
	_, err := env.Store.DB().Exec(`UPDATE attachments SET source_part_key = NULL WHERE message_id = ?`, id)
	require.NoError(err)
	require.NoError(env.Store.UpsertAttachmentRecord(t.Context(), id, store.AttachmentWrite{
		Filename: "", MIMEType: "text/plain", StoragePath: "ab/abcd", ContentHash: "abcd", Size: 150,
	}))

	assert.Empty(t, collectAudit(t, env.Syncer, source.ID))
	fields := auditFieldsForMessage(t, env, id)
	assert.Equal(t, AuditInconclusive, fields[AuditFieldAttachmentHashes])
}

func TestAuditGmailMessagesFlagsSwappedAttachmentHashes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	raw := testemail.NewMessage().
		Subject("swap").Body("body").
		WithAttachment("first.bin", "application/octet-stream", []byte("first bytes")).
		WithAttachment("second.bin", "application/octet-stream", []byte("second bytes")).Bytes()
	env.Mock.AddMessage("gmail-a", raw, []string{"INBOX"})
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 10
	env.SetOptions(t, func(options *Options) { options.AttachmentsDir = filepath.Join(env.TmpDir, "attachments") })
	runFullSync(t, env)
	var id int64
	require.NoError(env.Store.DB().QueryRow(
		`SELECT id FROM messages WHERE source_id = ? AND source_message_id = 'gmail-a'`, source.ID).Scan(&id))
	rows, err := env.Store.DB().Query(
		`SELECT id, content_hash FROM attachments WHERE message_id = ? ORDER BY id`, id)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	type storedAttachment struct {
		id   int64
		hash string
	}
	var stored []storedAttachment
	for rows.Next() {
		var attachment storedAttachment
		require.NoError(rows.Scan(&attachment.id, &attachment.hash))
		stored = append(stored, attachment)
	}
	require.NoError(rows.Err())
	require.Len(stored, 2)
	require.NotEqual(stored[0].hash, stored[1].hash)
	_, err = env.Store.DB().Exec(`UPDATE attachments SET content_hash = ? WHERE id = ?`, stored[1].hash, stored[0].id)
	require.NoError(err)
	_, err = env.Store.DB().Exec(`UPDATE attachments SET content_hash = ? WHERE id = ?`, stored[0].hash, stored[1].id)
	require.NoError(err)

	results := collectAudit(t, env.Syncer, source.ID)
	require.Len(results, 1)
	assert.Equal(AuditMismatch, results[0].Status)
	assert.Equal(AuditMismatch, results[0].Fields[AuditFieldAttachmentHashes],
		"hashes swapped between part keys must not compare as a coherent multiset")
	assert.Equal(AuditMatch, results[0].Fields[AuditFieldAttachmentPartKeys])
}

func TestAuditGmailMessagesIsReadOnlyAndKeysetPaged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	testutil.SkipIfPostgres(t, "filesystem and SQLite query-only proof")
	env := newTestEnv(t)
	source := env.CreateSource(t)
	var firstID, secondPageID int64
	for i := range auditPageSize + 1 {
		id := seedRepairRow(t, env.Store, source.ID, fmt.Sprintf("gmail-%03d", i), "thread", "coherent")
		if i == 0 {
			firstID = id
		}
		if i == auditPageSize {
			secondPageID = id
		}
	}
	_, err := env.Store.DB().Exec(`UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?`, []byte("not-zlib"), firstID)
	require.NoError(err)
	require.NoError(env.Store.UpsertMessageRaw(secondPageID,
		testemail.NewMessage().Subject("second-page-mismatch").Body("different").Bytes()))
	dbPath := filepath.Join(env.TmpDir, "test.db")
	before, err := os.Stat(dbPath)
	require.NoError(err)
	readOnly, err := store.OpenReadOnly(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = readOnly.Close() })
	auditor := New(env.Mock, readOnly, &Options{AttachmentsDir: filepath.Join(env.TmpDir, "attachments")})

	results := collectAudit(t, auditor, source.ID)
	require.Len(results, 2)
	assert.Equal(firstID, results[0].InternalID)
	assert.Equal(AuditInconclusive, results[0].Status)
	assert.Contains(results[0].Error, "zlib")
	assert.Equal(secondPageID, results[1].InternalID,
		"an emitted candidate must prove the second keyset page was processed")
	assert.Equal(AuditMismatch, results[1].Status)
	after, err := os.Stat(dbPath)
	require.NoError(err)
	assert.Equal(before.Size(), after.Size())
	assert.Equal(before.ModTime(), after.ModTime())
	_, statErr := os.Stat(filepath.Join(env.TmpDir, "attachments"))
	assert.ErrorIs(statErr, os.ErrNotExist)
}

func TestAuditGmailMessagesStopsLoadedPageOnCancellation(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	firstID := seedRepairRow(t, env.Store, source.ID, "gmail-a", "thread", "stored")
	secondID := seedRepairRow(t, env.Store, source.ID, "gmail-b", "thread", "stored")
	require.NoError(env.Store.UpsertMessageRaw(firstID, testemail.NewMessage().Subject("first mismatch").Body("different").Bytes()))
	require.NoError(env.Store.UpsertMessageRaw(secondID, testemail.NewMessage().Subject("second mismatch").Body("different").Bytes()))
	ctx, cancel := context.WithCancel(t.Context())
	var emitted []int64

	err := env.Syncer.AuditGmailMessages(ctx, source.ID, func(result RepairAuditResult) error {
		emitted = append(emitted, result.InternalID)
		cancel()
		return nil
	})
	require.ErrorIs(err, context.Canceled)
	assert.Equal(t, []int64{firstID}, emitted)
}

type cancelAfterArmedChecks struct {
	context.Context

	cancel context.CancelFunc
	armed  atomic.Bool
	checks atomic.Int32
}

func (c *cancelAfterArmedChecks) Err() error {
	if c.armed.Load() && c.checks.Add(1) == 3 {
		c.cancel()
	}
	return c.Context.Err()
}

func (c *cancelAfterArmedChecks) arm() {
	c.checks.Store(0)
	c.armed.Store(true)
}

func TestAuditGmailMessagesReturnsCancellationAfterCoherentFinalRow(t *testing.T) {
	env := newTestEnv(t)
	source := env.CreateSource(t)
	firstID := seedRepairRow(t, env.Store, source.ID, "gmail-a", "thread", "stored")
	seedRepairRow(t, env.Store, source.ID, "gmail-b", "thread", "coherent")
	require.NoError(t, env.Store.UpsertMessageRaw(firstID,
		testemail.NewMessage().Subject("mismatch").Body("different").Bytes()))
	base, cancel := context.WithCancel(t.Context())
	ctx := &cancelAfterArmedChecks{Context: base, cancel: cancel}
	var emitted []int64

	err := env.Syncer.AuditGmailMessages(ctx, source.ID, func(result RepairAuditResult) error {
		emitted = append(emitted, result.InternalID)
		ctx.arm()
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []int64{firstID}, emitted)
}

func TestRepairedMessageWithProviderAttachmentIsNotAuditedAgain(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	id := seedRepairRow(t, env.Store, source.ID, "gmail-a", "thread-a", "old")
	_, err := env.Store.DB().Exec(`INSERT INTO attachments (
		message_id, filename, mime_type, size, content_hash, storage_path,
		source_attachment_id, attachment_role, role_source
	) VALUES (?, 'provider.bin', 'application/octet-stream', 8, 'providerhash', 'pr/provider',
		'provider:1', 'standalone', 'provider_explicit')`, id)
	require.NoError(err)
	env.Mock.Messages["gmail-a"] = repairRaw("gmail-a", "thread-a", "new", "new body", nil)

	_, err = env.Syncer.RepairMessage(t.Context(), RepairRequest{Reference: "gmail-a", SourceID: source.ID})
	require.NoError(err)
	assert.Empty(collectAudit(t, env.Syncer, source.ID))
	var providerRows int
	require.NoError(env.Store.DB().QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id = ? AND source_attachment_id IS NOT NULL`, id).Scan(&providerRows))
	assert.Equal(1, providerRows)
}

func TestAuditGmailMessagesScopesPositiveSourceAndRejectsNonGmail(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	gmailSource := env.CreateSource(t)
	imapSource, err := env.Store.GetOrCreateSource("imap", "imap@example.com")
	require.NoError(err)
	gmailID := seedRepairRow(t, env.Store, gmailSource.ID, "gmail-a", "thread", "stored")
	seedRepairRow(t, env.Store, imapSource.ID, "imap-a", "thread", "stored")
	require.NoError(env.Store.UpsertMessageRaw(gmailID, testemail.NewMessage().Subject("different").Body("different").Bytes()))

	results := collectAudit(t, env.Syncer, gmailSource.ID)
	require.Len(results, 1)
	assert.Equal(t, gmailSource.ID, results[0].SourceID)
	err = env.Syncer.AuditGmailMessages(t.Context(), -1, func(RepairAuditResult) error { return nil })
	require.ErrorContains(err, "source ID must be positive")
	err = env.Syncer.AuditGmailMessages(t.Context(), imapSource.ID, func(RepairAuditResult) error { return nil })
	require.ErrorContains(err, "not gmail")
}

// TestAuditGmailMessagesMarksOversizedRawInconclusive proves the audit bounds
// raw-MIME decompression. The stored payload is a valid MIME message whose
// decompressed size lands just over the audit's 64 MiB cap; such a record
// must be reported inconclusive instead of being fully materialized, because
// raw storage has no size limit and unbounded decompression could exhaust
// memory on a corrupt or hostile archive.
func TestAuditGmailMessagesMarksOversizedRawInconclusive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// The cap under test is the named production constant (64 MiB).
	const gmailAuditRawMIMECapBytes = store.GmailAuditMaxRawMIMEBytes
	env := newTestEnv(t)
	source := env.CreateSource(t)
	id := seedGmailAuditRow(t, env.Store, source.ID, "gmail-oversized", "oversized subject")

	body := strings.Repeat("a", int(gmailAuditRawMIMECapBytes)+4096)
	raw := testemail.NewMessage().
		From("sender@example.com").To("recipient@example.com").
		Subject("oversized subject").
		Header("Message-ID", "<gmail-oversized@example.com>").
		Body(body).Bytes()
	require.Greater(len(raw), int(gmailAuditRawMIMECapBytes),
		"fixture must decompress just over the audit cap")
	require.NoError(env.Store.UpsertMessageRaw(id, raw))

	results := collectAudit(t, env.Syncer, source.ID)
	require.Len(results, 1,
		"oversized raw MIME must be reported instead of silently passing or failing on field comparisons")
	assert.Equal(AuditInconclusive, results[0].Status)
	assert.Equal(AuditInconclusive, results[0].Fields[AuditFieldRawMIME])
	assert.Contains(results[0].Error, "exceeds")
	assert.Contains(results[0].Error, "67108864")
}

func seedGmailAuditRow(
	t *testing.T, st *store.Store, sourceID int64, sourceMessageID, subject string,
) int64 {
	t.Helper()
	require := require.New(t)
	convID, err := st.EnsureConversation(sourceID, "thread-"+sourceMessageID, subject)
	require.NoError(err)
	participants, err := st.EnsureParticipantsBatch([]mime.Address{
		{Email: "sender@example.com", Domain: "example.com"},
		{Email: "recipient@example.com", Domain: "example.com"},
	})
	require.NoError(err)
	id, err := st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			ConversationID: convID, SourceID: sourceID, SourceMessageID: sourceMessageID,
			MessageType:     store.MessageTypeEmail,
			RFC822MessageID: sql.NullString{String: "<" + sourceMessageID + "@example.com>", Valid: true},
			Subject:         sql.NullString{String: subject, Valid: true},
			SenderID:        sql.NullInt64{Int64: participants["sender@example.com"], Valid: true},
		},
		Recipients: []store.RecipientSet{
			{Type: "from", ParticipantIDs: []int64{participants["sender@example.com"]}, EmailAddresses: []string{"sender@example.com"}},
			{Type: "to", ParticipantIDs: []int64{participants["recipient@example.com"]}, EmailAddresses: []string{"recipient@example.com"}},
		},
	})
	require.NoError(err)
	return id
}

func auditFieldsForMessage(t *testing.T, env *TestEnv, id int64) map[string]AuditState {
	t.Helper()
	var fields map[string]AuditState
	_, err := env.Store.StreamGmailAuditEvidencePageContext(
		t.Context(), 0, id-1, 1, func(evidence store.GmailAuditEvidence) error {
			result, _ := auditGmailEvidence(evidence)
			fields = result.Fields
			return nil
		})
	require.NoError(t, err)
	return fields
}

func collectAudit(t *testing.T, syncer *Syncer, sourceID int64) []RepairAuditResult {
	t.Helper()
	var results []RepairAuditResult
	require.NoError(t, syncer.AuditGmailMessages(t.Context(), sourceID, func(result RepairAuditResult) error {
		results = append(results, result)
		return nil
	}))
	return results
}

func auditByID(results []RepairAuditResult) map[int64]RepairAuditResult {
	byID := make(map[int64]RepairAuditResult, len(results))
	for _, result := range results {
		byID[result.InternalID] = result
	}
	return byID
}
