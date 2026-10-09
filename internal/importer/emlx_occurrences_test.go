//go:build linux || darwin

package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emlx"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/email"
)

// Successful ingestion must acknowledge both the physical occurrence and
// completion of its shared archived target across a database reopen.
func TestImportEmlxOccurrenceReceiptsPersist(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := email.NewMessage().From("sender@example.test").To("owner@example.test").
		Header("Message-ID", "<receipt@example.test>").Body("Synthetic receipt message.").Bytes()
	mkMailboxDir(t, root, map[string][]byte{"1.emlx": raw})
	opts := EmlxImportOptions{Identifier: "owner@example.test"}

	summary, err := ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	requirements.False(summary.HardErrors)
	assertions.Equal(int64(1), summary.MessagesAdded)
	assertions.Equal(1, countEmlxLedgerEntries(t, st, summary.SourceID, "emlx-occurrence", "imported"))
	assertions.Equal(1, countEmlxLedgerEntries(t, st, summary.SourceID, "emlx-target", "imported"))

	requirements.NoError(st.Close())
	reopened, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(reopened.Close()) })
	assertions.Equal(1, countEmlxLedgerEntries(t, reopened, summary.SourceID, "emlx-occurrence", "imported"))
	assertions.Equal(1, countEmlxLedgerEntries(t, reopened, summary.SourceID, "emlx-target", "imported"))

	summary, err = ImportEmlxDir(t.Context(), reopened, root, opts)
	requirements.NoError(err)
	assertions.False(summary.HardErrors)
	assertions.Zero(summary.MessagesAdded)
	assertions.Equal(int64(1), summary.MessagesProcessed)
}

func TestImportEmlxNewOldDateMessage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	recent := email.NewMessage().From("sender@example.test").To("owner@example.test").
		Header("Message-ID", "<recent-download@example.test>").Date("Mon, 01 Jan 2024 12:00:00 +0000").
		Body("Synthetic recent message.").Bytes()
	mkMailboxDir(t, root, map[string][]byte{"20.emlx": recent})
	opts := EmlxImportOptions{Identifier: "owner@example.test"}
	first, err := ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	requirements.False(first.HardErrors)
	requirements.Equal(int64(1), first.MessagesAdded)

	old := email.NewMessage().From("sender@example.test").To("owner@example.test").
		Header("Message-ID", "<old-download@example.test>").Date("Sat, 01 Jan 2000 12:00:00 +0000").
		Body("Synthetic old message downloaded later.").Bytes()
	mkEmlx(t, filepath.Join(root, "Messages"), "10.emlx", old)
	second, err := ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	assertions.False(second.HardErrors)
	assertions.Equal(int64(1), second.MessagesAdded, "sent date and earlier path cannot hide a new occurrence")
	assertions.Equal(int64(2), second.MessagesProcessed)
	assertions.Equal(2, countEmlxLedgerEntries(t, st, first.SourceID, "emlx-occurrence", "imported"))
	var messageID int64
	requirements.NoError(st.DB().QueryRow(`SELECT id FROM messages
 WHERE rfc822_message_id = 'old-download@example.test'`).Scan(&messageID))
	archived, err := st.GetMessageRawContext(t.Context(), messageID)
	requirements.NoError(err)
	assertions.Equal(old, archived)
}

func TestImportEmlxMovedOccurrenceRetainsArchive(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Mail")
	inbox := filepath.Join(root, "Inbox.mbox")
	archive := filepath.Join(root, "Archive.mbox")
	raw := email.NewMessage().From("sender@example.test").To("owner@example.test").
		Header("Message-ID", "<moved-occurrence@example.test>").Body("Synthetic moved occurrence.").Bytes()
	mkMailboxDir(t, inbox, map[string][]byte{"1.emlx": raw})
	opts := EmlxImportOptions{Identifier: "owner@example.test"}
	first, err := ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	requirements.False(first.HardErrors)
	var messageID int64
	requirements.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&messageID))

	mkMailboxDir(t, archive, map[string][]byte{"3.emlx": raw})
	second, err := ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	assertions.False(second.HardErrors)
	assertions.Zero(second.MessagesAdded, "new occurrence retains content deduplication")
	labels, err := st.MessageLabelIDsContext(t.Context(), messageID)
	requirements.NoError(err)
	assertions.Len(labels, 2, "an additional mailbox occurrence accumulates provenance without erasing the old label")
	assertions.Equal(2, countEmlxLedgerEntries(t, st, first.SourceID, "emlx-occurrence", "imported"),
		"simultaneously present paths have independent durable occurrence records")
	assertions.Equal(1, countEmlxLedgerEntries(t, st, first.SourceID, "emlx-target", "imported"))

	movedPath := filepath.Join(archive, "Messages", "2.emlx")
	requirements.NoError(os.Rename(filepath.Join(inbox, "Messages", "1.emlx"), movedPath))
	moved, err := ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	assertions.False(moved.HardErrors)
	assertions.Zero(moved.MessagesAdded)
	var movedReceipts int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM source_import_items
 WHERE source_id = ? AND provider = 'emlx-occurrence' AND name = ? AND status = 'imported'`),
		first.SourceID, "Archive.mbox/Messages/2.emlx").Scan(&movedReceipts))
	assertions.Equal(1, movedReceipts, "a moved path gets its own receipt even when the content already exists")

	requirements.NoError(os.Remove(movedPath))
	requirements.NoError(os.Remove(filepath.Join(archive, "Messages", "3.emlx")))
	_, err = ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	archived, err := st.GetMessageRawContext(t.Context(), messageID)
	requirements.NoError(err)
	assertions.Equal(raw, archived, "source cache disappearance preserves the archived message")
	labels, err = st.MessageLabelIDsContext(t.Context(), messageID)
	requirements.NoError(err)
	assertions.Len(labels, 2)
	var live int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE id = ? AND deleted_at IS NULL`),
		messageID).Scan(&live))
	assertions.Equal(1, live)
}

// Fail the actual FTS insertion after raw persistence. A raw-present duplicate
// must retry the unfinished index work when the database failure is removed.
func TestImportEmlxFTSFailureRemainsRetryable(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, tmp := openTestStore(t)
	requirements.True(st.FTS5Available(), "required tagged SQLite FTS capability")
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := email.NewMessage().From("sender@example.test").To("owner@example.test").
		Header("Message-ID", "<retry-index@example.test>").Body("syntheticretryneedle").Bytes()
	mkMailboxDir(t, root, map[string][]byte{"1.emlx": raw})
	opts := EmlxImportOptions{Identifier: "owner@example.test"}

	// The FTS content shadow table is written by SQLite's real virtual-table
	// insertion. Its insert trigger leaves message persistence and FTS deletion
	// intact, so the failure happens at the production post-commit FTS boundary.
	_, err := st.DB().Exec(`CREATE TRIGGER fail_emlx_fts_content
 BEFORE INSERT ON messages_fts_content BEGIN
 SELECT RAISE(ABORT, 'synthetic EMLX FTS insertion failure'); END`)
	requirements.NoError(err)
	summary, err := ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	assertions.True(summary.HardErrors, "committed raw does not complete failed FTS work")
	assertions.Positive(summary.Errors)
	assertions.Zero(countEmlxLedgerEntries(t, st, summary.SourceID, "emlx-occurrence", "imported"))
	assertions.Zero(countEmlxLedgerEntries(t, st, summary.SourceID, "emlx-target", "imported"))

	var messageID int64
	requirements.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&messageID))
	archived, err := st.GetMessageRawContext(t.Context(), messageID)
	requirements.NoError(err)
	assertions.Equal(raw, archived, "raw committed before the injected real FTS failure")
	var indexed int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts
 WHERE messages_fts MATCH 'syntheticretryneedle'`).Scan(&indexed))
	requirements.Zero(indexed, "the real FTS insertion fault must leave committed raw unindexed")

	_, err = st.DB().Exec(`DROP TRIGGER fail_emlx_fts_content`)
	requirements.NoError(err)
	summary, err = ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	assertions.False(summary.HardErrors)
	assertions.Zero(summary.Errors)
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts
 WHERE messages_fts MATCH 'syntheticretryneedle'`).Scan(&indexed))
	assertions.Equal(1, indexed, "an unchanged raw-present source must repair unfinished FTS")
	assertions.Equal(1, countEmlxLedgerEntries(t, st, summary.SourceID, "emlx-occurrence", "imported"))
	assertions.Equal(1, countEmlxLedgerEntries(t, st, summary.SourceID, "emlx-target", "imported"))
}

// EMLX framing can be valid while MIME parsing is fatally incomplete. Preserve
// salvage/raw without claiming the missing body and attachment work completed.
func TestImportEmlxFatalMIMEDoesNotComplete(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := []byte("From: sender@example.test\r\n" +
		"To: owner@example.test\r\n" +
		"Subject: Synthetic malformed import\r\n" +
		"Message-ID: <malformed-import@example.test>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--outer--\r\n")
	salvaged, parseErr := mime.ParseWithRecovery(raw, "")
	requirements.Error(parseErr, "fixture must reach fatal MIME recovery rather than a nonfatal warning")
	requirements.NotNil(salvaged)
	mkMailboxDir(t, root, map[string][]byte{"1.emlx": raw})
	summary, err := ImportEmlxDir(t.Context(), st, root, EmlxImportOptions{Identifier: "owner@example.test"})
	requirements.NoError(err)
	assertions.True(summary.HardErrors)
	assertions.Positive(summary.Errors)
	assertions.Zero(countEmlxLedgerEntries(t, st, summary.SourceID, "emlx-occurrence", "imported"))
	assertions.Zero(countEmlxLedgerEntries(t, st, summary.SourceID, "emlx-target", "imported"))

	var messageID int64
	var subject, rfcID string
	requirements.NoError(st.DB().QueryRow(`SELECT id, subject, rfc822_message_id FROM messages`).
		Scan(&messageID, &subject, &rfcID))
	assertions.Equal("Synthetic malformed import", subject)
	assertions.Equal("malformed-import@example.test", rfcID)
	archived, err := st.GetMessageRawContext(t.Context(), messageID)
	requirements.NoError(err)
	assertions.Equal(raw, archived)
}

func TestImportEmlxPolicyChangeRetriesAttachmentStorage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	content := []byte("Synthetic attachment to store after enabling disk storage.")
	raw := email.NewMessage().From("sender@example.test").To("owner@example.test").
		Header("Message-ID", "<attachment-policy@example.test>").Body("Synthetic policy message.").
		WithAttachment("fixture.txt", "text/plain", content).Bytes()
	mkMailboxDir(t, root, map[string][]byte{"1.emlx": raw})
	opts := EmlxImportOptions{Identifier: "owner@example.test"}
	first, err := ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	requirements.False(first.HardErrors)
	var messageID, attachments int64
	requirements.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&messageID))
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attachments))
	requirements.Zero(attachments, "disk storage was intentionally disabled")

	opts.AttachmentsDir = filepath.Join(tmp, "enabled-attachments")
	second, err := ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	assertions.False(second.HardErrors)
	assertions.Zero(second.MessagesAdded)
	var contentHash string
	err = st.DB().QueryRow(st.Rebind(`SELECT content_hash FROM attachments WHERE message_id = ?`), messageID).
		Scan(&contentHash)
	requirements.NoError(err, "unchanged raw must complete newly enabled attachment storage")
	wantHash := sha256.Sum256(content)
	assertions.Equal(hex.EncodeToString(wantHash[:]), contentHash)
	blobPath, err := export.StoragePath(opts.AttachmentsDir, contentHash)
	requirements.NoError(err)
	blob, err := os.ReadFile(blobPath)
	requirements.NoError(err)
	assertions.Equal(content, blob)
	archived, err := st.GetMessageRawContext(t.Context(), messageID)
	requirements.NoError(err)
	assertions.Equal(raw, archived)
}

func TestImportEmlxTargetTransitionFailures(t *testing.T) {
	for _, transition := range []string{"dirty", "complete"} {
		t.Run(transition, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st, tmp := openTestStore(t)
			root := filepath.Join(tmp, "Inbox.mbox")
			raw := email.NewMessage().From("sender@example.test").To("owner@example.test").
				Header("Message-ID", "<transition@example.test>").Body("Synthetic target transition.").Bytes()
			mkMailboxDir(t, root, map[string][]byte{"1.emlx": raw})
			opts := EmlxImportOptions{Identifier: "owner@example.test"}
			failedStatus := "pending"
			if transition == "complete" {
				failedStatus = "imported"
			}
			// Both INSERT and UPDATE are covered so the observable contract does
			// not depend on the ledger's choice of upsert statement.
			for _, operation := range []string{"INSERT", "UPDATE"} {
				_, err := st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER fail_target_%s BEFORE %s ON source_import_items
 WHEN NEW.provider = 'emlx-target' AND NEW.status = '%s' BEGIN
 SELECT RAISE(ABORT, 'synthetic target transition failure'); END`, operation, operation, failedStatus))
				requirements.NoError(err)
			}
			failed, err := ImportEmlxDir(t.Context(), st, root, opts)
			requirements.NoError(err)
			assertions.True(failed.HardErrors)
			assertions.Positive(failed.Errors)
			assertions.Zero(countEmlxLedgerEntries(t, st, failed.SourceID, "emlx-occurrence", "imported"))
			assertions.Zero(countEmlxLedgerEntries(t, st, failed.SourceID, "emlx-target", "imported"))
			var messages, rawRows int
			requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
			requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM message_raw`).Scan(&rawRows))
			if transition == "dirty" {
				assertions.Zero(messages, "failed durable dirty transition must prevent message mutation")
				assertions.Zero(rawRows)
			} else {
				requirements.Equal(1, messages, "completion publication failed after real ingestion")
				requirements.Equal(1, rawRows)
				assertions.Equal(1, countEmlxLedgerEntries(t, st, failed.SourceID, "emlx-target", "pending"))
				var messageID int64
				requirements.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&messageID))
				archived, err := st.GetMessageRawContext(t.Context(), messageID)
				requirements.NoError(err)
				assertions.Equal(raw, archived)
			}
			for _, operation := range []string{"INSERT", "UPDATE"} {
				_, err := st.DB().Exec("DROP TRIGGER fail_target_" + operation)
				requirements.NoError(err)
			}
			requirements.NoError(st.Close())
			reopened, err := store.Open(filepath.Join(tmp, "msgvault.db"))
			requirements.NoError(err)
			t.Cleanup(func() { requirements.NoError(reopened.Close()) })
			retried, err := ImportEmlxDir(t.Context(), reopened, root, opts)
			requirements.NoError(err)
			assertions.False(retried.HardErrors)
			assertions.Zero(retried.Errors)
			assertions.Equal(1, countEmlxLedgerEntries(t, reopened, retried.SourceID, "emlx-target", "imported"))
			assertions.Equal(1, countEmlxLedgerEntries(t, reopened, retried.SourceID, "emlx-occurrence", "imported"))
		})
	}
}

// A clean occurrence cannot hide unfinished work from a same-hash occurrence
// that later committed new attachment bytes and disappeared from the source.
func TestImportEmlxSharedTargetRecoversCurrentRaw(t *testing.T) {
	for _, layout := range []string{"same batch", "earlier path", "later batch", "later mailbox", "different roots"} {
		for _, fault := range []string{"attachment record", "FTS"} {
			t.Run(layout+"/"+fault, func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)
				st, tmp := openTestStore(t)
				requirements.True(st.FTS5Available())
				rootA := filepath.Join(tmp, "Mail")
				rootB := rootA
				mboxA := filepath.Join(rootA, "A.mbox")
				mboxB := mboxA
				nameB, numberB := "2.partial.emlx", "2"
				nameA, numberA := "1.partial.emlx", "1"
				if layout == "earlier path" {
					nameA, numberA = "9999.partial.emlx", "9999"
					nameB, numberB = "1.partial.emlx", "1"
				}
				switch layout {
				case "later mailbox":
					mboxB = filepath.Join(rootA, "B.mbox")
				case "different roots":
					rootB = filepath.Join(tmp, "OtherMail")
					mboxB = filepath.Join(rootB, "B.mbox")
				case "later batch":
					nameB, numberB = "9999.partial.emlx", "9999"
				}
				raw := []byte("From: sender@example.test\nTo: owner@example.test\n" +
					"Message-ID: <shared-parts@example.test>\n" +
					"Subject: Synthetic shared target\n" +
					"Content-Type: multipart/mixed; boundary=fixture\n\n" +
					"--fixture\nContent-Type: text/plain\n\nsharedtargetneedle\n" +
					"--fixture\nContent-Type: application/octet-stream\n" +
					"Content-Disposition: attachment; filename=part.bin\n" +
					"X-Apple-Content-Length: 20\n\n\n--fixture--\n")
				oldPart, newPart := []byte("older cached part"), []byte("latest committed part")
				mkMailboxDir(t, mboxA, map[string][]byte{nameA: raw})
				cacheAttachment(t, mboxA, numberA, "2", "part.bin", oldPart)
				opts := EmlxImportOptions{
					Identifier: "owner@example.test", AttachmentsDir: filepath.Join(tmp, "archive-attachments"),
				}
				first, err := ImportEmlxDir(t.Context(), st, rootA, opts)
				requirements.NoError(err)
				requirements.False(first.HardErrors)
				requirements.Equal(1, countEmlxLedgerEntries(t, st, first.SourceID, "emlx-occurrence", "imported"),
					"A must be a completed occurrence before B mutates their shared target")
				requirements.Equal(1, countEmlxLedgerEntries(t, st, first.SourceID, "emlx-target", "imported"))

				mkMailboxDir(t, mboxB, map[string][]byte{nameB: raw})
				cacheAttachment(t, mboxB, numberB, "2", "part.bin", newPart)
				if layout == "later batch" {
					// Distinct source hashes fill the real 200-record batch before B.
					for i := range 201 {
						filler := email.NewMessage().From("sender@example.test").
							Header("Message-ID", fmt.Sprintf("<filler-%d@example.test>", i)).
							Body(fmt.Sprintf("Synthetic batch filler %d.", i)).Bytes()
						mkEmlx(t, filepath.Join(mboxA, "Messages"), fmt.Sprintf("5%03d.emlx", i), filler)
					}
				}

				trigger := "fail_shared_attachment"
				statement := `CREATE TRIGGER fail_shared_attachment BEFORE UPDATE ON attachments
 WHEN NEW.content_hash <> OLD.content_hash BEGIN
 SELECT RAISE(ABORT, 'synthetic shared attachment failure'); END`
				if fault == "FTS" {
					trigger = "fail_shared_fts"
					statement = `CREATE TRIGGER fail_shared_fts BEFORE INSERT ON messages_fts_content
 BEGIN SELECT RAISE(ABORT, 'synthetic shared FTS failure'); END`
				}
				_, err = st.DB().Exec(statement)
				requirements.NoError(err)
				failed, err := ImportEmlxDir(t.Context(), st, rootB, opts)
				requirements.NoError(err)
				assertions.True(failed.HardErrors)
				originalHash := sha256.Sum256(raw)
				sourceMessageID := "emlx-" + hex.EncodeToString(originalHash[:])
				var messageID int64
				requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages
 WHERE source_id = ? AND source_message_id = ?`), first.SourceID, sourceMessageID).Scan(&messageID))
				committed, err := st.GetMessageRawContext(t.Context(), messageID)
				requirements.NoError(err)
				parsed, err := mime.Parse(committed)
				requirements.NoError(err)
				requirements.Len(parsed.Attachments, 1)
				requirements.Equal(newPart, parsed.Attachments[0].Content,
					"the fixture must reach changed raw persistence before completion fails")
				var completedTarget int
				requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM source_import_items
 WHERE source_id = ? AND provider = 'emlx-target' AND provider_id = ? AND status = 'imported'`),
					first.SourceID, sourceMessageID).Scan(&completedTarget))
				assertions.Zero(completedTarget)

				_, err = st.DB().Exec("DROP TRIGGER " + trigger)
				requirements.NoError(err)
				requirements.NoError(os.Remove(filepath.Join(mboxB, "Messages", nameB)))
				requirements.NoError(st.Close())
				reopened, err := store.Open(filepath.Join(tmp, "msgvault.db"))
				requirements.NoError(err)
				t.Cleanup(func() { requirements.NoError(reopened.Close()) })
				if layout == "different roots" {
					opts.NoResume = true
				}
				recovered, err := ImportEmlxDir(t.Context(), reopened, rootA, opts)
				requirements.NoError(err)
				assertions.False(recovered.HardErrors)
				archived, err := reopened.GetMessageRawContext(t.Context(), messageID)
				requirements.NoError(err)
				assertions.Equal(committed, archived, "unchanged older A must finish CURRENT B raw")
				var contentHash, storagePath string
				requirements.NoError(reopened.DB().QueryRow(reopened.Rebind(`SELECT content_hash, storage_path
 FROM attachments WHERE message_id = ?`), messageID).Scan(&contentHash, &storagePath))
				partHash := sha256.Sum256(newPart)
				assertions.Equal(hex.EncodeToString(partHash[:]), contentHash)
				blobPath, err := export.StoragePath(opts.AttachmentsDir, contentHash)
				requirements.NoError(err)
				blob, err := os.ReadFile(blobPath)
				requirements.NoError(err)
				assertions.Equal(newPart, blob)
				assertions.NotEmpty(storagePath)
				var indexed int
				requirements.NoError(reopened.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts
 WHERE messages_fts MATCH 'sharedtargetneedle'`).Scan(&indexed))
				assertions.Equal(1, indexed)
				labels, err := reopened.MessageLabelIDsContext(t.Context(), messageID)
				requirements.NoError(err)
				wantLabels := 1
				if layout == "later mailbox" || layout == "different roots" {
					wantLabels = 2
				}
				assertions.Len(labels, wantLabels, "recovery preserves accumulated mailbox provenance")
				requirements.NoError(reopened.DB().QueryRow(reopened.Rebind(`SELECT COUNT(*) FROM source_import_items
 WHERE source_id = ? AND provider = 'emlx-target' AND provider_id = ? AND status = 'imported'`),
					first.SourceID, sourceMessageID).Scan(&completedTarget))
				assertions.Equal(1, completedTarget)
				warm, err := ImportEmlxDir(t.Context(), reopened, rootA, opts)
				requirements.NoError(err)
				assertions.False(warm.HardErrors)
				assertions.Zero(warm.MessagesUpdated, "conflicting old parts must settle into a stable warm run")
				again, err := reopened.GetMessageRawContext(t.Context(), messageID)
				requirements.NoError(err)
				assertions.Equal(committed, again)
			})
		}
	}
}

func TestImportEmlxWarmAvoidsContentReads(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := email.NewMessage().From("sender@example.test").Header("Message-ID", "<warm@example.test>").Body("synthetic warm").Bytes()
	mkMailboxDir(t, root, map[string][]byte{"1.emlx": raw})
	opts := EmlxImportOptions{Identifier: "owner@example.test"}
	first, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	r.False(first.HardErrors)
	r.NoError(st.Close())
	st, err = store.Open(filepath.Join(tmp, "msgvault.db"))
	r.NoError(err)
	t.Cleanup(func() { r.NoError(st.Close()) })
	io := defaultEmlxImportIO(opts)
	parses, reads, ingests := 0, 0, 0
	parse, read, ingest := io.parse, io.raw, io.ingest
	io.parse = func(p string, byteLimit int64) (*emlx.Message, error) { parses++; return parse(p, byteLimit) }
	io.raw = func(ctx context.Context, s *store.Store, id int64) ([]byte, error) { reads++; return read(ctx, s, id) }
	io.ingest = func(ctx context.Context, s *store.Store, sid int64, identifier, dest string, labels []int64, target, hash string, raw []byte, date time.Time, log *slog.Logger) error {
		ingests++
		return ingest(ctx, s, sid, identifier, dest, labels, target, hash, raw, date, log)
	}
	warm, err := importEmlxDir(t.Context(), st, root, opts, io)
	r.NoError(err)
	r.False(warm.HardErrors)
	a.Equal(int64(1), warm.MessagesProcessed)
	a.Equal(int64(1), warm.FilesUnchanged)
	a.Zero(parses)
	a.Zero(reads)
	a.Zero(ingests)
}

func TestImportEmlxReconcileEmptyTree(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := email.NewMessage().From("sender@example.test").Body("synthetic empty reconciliation").Bytes()
	mkMailboxDir(t, root, map[string][]byte{"1.emlx": raw})
	opts := EmlxImportOptions{Identifier: "owner@example.test"}
	first, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	r.False(first.HardErrors)
	r.NoError(os.Remove(filepath.Join(root, "Messages", "1.emlx")))
	opts.FullReconcile = true
	empty, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	a.Zero(empty.Errors)
	a.Zero(countEmlxLedgerEntries(t, st, first.SourceID, "emlx-occurrence", "imported"))
	a.Equal(1, countEmlxLedgerEntries(t, st, first.SourceID, "emlx-occurrence", "pending"))
	var n int
	r.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&n))
	a.Equal(1, n)
}

func TestImportEmlxReconcileRepairsEqualRaw(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := email.NewMessage().From("sender@example.test").Body("reconciliationneedle").Bytes()
	mkMailboxDir(t, root, map[string][]byte{"1.emlx": raw})
	opts := EmlxImportOptions{Identifier: "owner@example.test"}
	first, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	r.False(first.HardErrors)
	_, err = st.DB().Exec("DELETE FROM messages_fts")
	r.NoError(err)
	opts.FullReconcile = true
	fixed, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	a.False(fixed.HardErrors)
	var n int
	r.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'reconciliationneedle'").Scan(&n))
	a.Equal(1, n)
}

func TestImportEmlxReconcileInvalidatesUnvisitedOccurrences(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	mkMailboxDir(t, root, map[string][]byte{
		"1.emlx": email.NewMessage().From("sender@example.test").Body("synthetic first").Bytes(),
		"2.emlx": email.NewMessage().From("sender@example.test").Body("synthetic second").Bytes(),
	})
	opts := EmlxImportOptions{Identifier: "owner@example.test"}
	first, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	r.False(first.HardErrors)
	opts.FullReconcile = true
	io := defaultEmlxImportIO(opts)
	ingest := io.ingest
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	io.ingest = func(ctx context.Context, s *store.Store, sid int64, identifier, dest string, labels []int64, target, hash string, raw []byte, date time.Time, log *slog.Logger) error {
		err := ingest(ctx, s, sid, identifier, dest, labels, target, hash, raw, date, log)
		cancel()
		return err
	}
	_, err = importEmlxDir(ctx, st, root, opts, io)
	r.ErrorIs(err, context.Canceled)
	a.Zero(countEmlxLedgerEntries(t, st, first.SourceID, "emlx-occurrence", "imported"))
	var checksums int
	r.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM source_import_items WHERE provider='emlx-occurrence' AND checksum IS NOT NULL").Scan(&checksums))
	a.Zero(checksums)
	opts.FullReconcile = false
	parses := 0
	io = defaultEmlxImportIO(opts)
	parse := io.parse
	io.parse = func(path string, byteLimit int64) (*emlx.Message, error) { parses++; return parse(path, byteLimit) }
	retried, err := importEmlxDir(t.Context(), st, root, opts, io)
	r.NoError(err)
	a.False(retried.HardErrors)
	a.Equal(2, parses)
}

func TestImportEmlxPostReadMutationRemainsRetryable(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := email.NewMessage().From("sender@example.test").Body("synthetic initial").Bytes()
	mkMailboxDir(t, root, map[string][]byte{"1.emlx": raw})
	opts := EmlxImportOptions{Identifier: "owner@example.test"}
	io := defaultEmlxImportIO(opts)
	parse := io.parse
	io.parse = func(path string, byteLimit int64) (*emlx.Message, error) {
		msg, err := parse(path, byteLimit)
		if err == nil {
			err = os.Chtimes(path, time.Unix(123, 0), time.Unix(123, 0))
		}
		return msg, err
	}
	first, err := importEmlxDir(t.Context(), st, root, opts, io)
	r.NoError(err)
	a.True(first.HardErrors)
	a.Zero(countEmlxLedgerEntries(t, st, first.SourceID, "emlx-occurrence", "imported"))
	retried, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	a.False(retried.HardErrors)
	a.Equal(1, countEmlxLedgerEntries(t, st, first.SourceID, "emlx-occurrence", "imported"))
}

func TestImportEmlxReconcileBypassesDifferentRoot(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	mkMailboxDir(t, root, map[string][]byte{"1.emlx": email.NewMessage().From("sender@example.test").Body("synthetic root").Bytes()})
	opts := EmlxImportOptions{Identifier: "owner@example.test"}
	src, err := st.GetOrCreateSource("apple-mail", opts.Identifier)
	r.NoError(err)
	run, err := st.StartSync(src.ID, "import-emlx")
	r.NoError(err)
	r.NoError(st.UpdateSyncCheckpoint(run, &store.Checkpoint{PageToken: `{"root_dir":"/obsolete","phase":"unknown","mailbox_index":-99,"reply_after_id":-99}`}))
	r.NoError(st.FailSync(run, "synthetic interruption"))
	_, err = ImportEmlxDir(t.Context(), st, root, opts)
	r.Error(err)
	opts.FullReconcile = true
	fresh, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	a.False(fresh.HardErrors)
	a.Equal(int64(1), fresh.MessagesAdded)
}

func TestImportEmlxMergedBudgetRemainsRetryable(t *testing.T) {
	for _, retry := range []string{"larger budget", "shrinking retained part"} {
		t.Run(retry, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			st, tmp := openTestStore(t)
			root := filepath.Join(tmp, "Inbox.mbox")
			raw := partialRaw(nil, "one.bin", "two.bin")
			mkMailboxDir(t, root, map[string][]byte{"1.partial.emlx": raw})
			old, newPart := bytes.Repeat([]byte("a"), 200), bytes.Repeat([]byte("b"), 200)
			cacheAttachment(t, root, "1", "2", "one.bin", old)
			opts := EmlxImportOptions{Identifier: "owner@example.test", AttachmentsDir: filepath.Join(tmp, "blobs"), MaxMessageBytes: int64(len(raw) + 350)}
			first, err := ImportEmlxDir(t.Context(), st, root, opts)
			r.NoError(err)
			r.False(first.HardErrors)
			mkEmlx(t, filepath.Join(root, "Messages"), "2.partial.emlx", raw)
			cacheAttachment(t, root, "2", "3", "two.bin", newPart)
			limited, err := ImportEmlxDir(t.Context(), st, root, opts)
			r.NoError(err)
			a.True(limited.HardErrors)
			a.Equal(1, countEmlxLedgerEntries(t, st, first.SourceID, "emlx-occurrence", "imported"))
			var mid int64
			r.NoError(st.DB().QueryRow("SELECT id FROM messages").Scan(&mid))
			current, err := st.GetMessageRawContext(t.Context(), mid)
			r.NoError(err)
			parsed, err := mime.Parse(current)
			r.NoError(err)
			r.Len(parsed.Attachments, 2)
			a.Equal(old, parsed.Attachments[0].Content)
			a.Empty(parsed.Attachments[1].Content)
			if retry == "larger budget" {
				opts.MaxMessageBytes = int64(len(raw) + 700)
			} else {
				old = []byte("smaller")
				r.NoError(os.WriteFile(filepath.Join(root, "Attachments", "1", "2", "one.bin"), old, 0600))
			}
			fixed, err := ImportEmlxDir(t.Context(), st, root, opts)
			r.NoError(err)
			a.False(fixed.HardErrors)
			current, err = st.GetMessageRawContext(t.Context(), mid)
			r.NoError(err)
			parsed, err = mime.Parse(current)
			r.NoError(err)
			r.Len(parsed.Attachments, 2)
			a.Equal(old, parsed.Attachments[0].Content)
			a.Equal(newPart, parsed.Attachments[1].Content)
			a.Equal(2, countEmlxLedgerEntries(t, st, first.SourceID, "emlx-occurrence", "imported"))
			warm, err := ImportEmlxDir(t.Context(), st, root, opts)
			r.NoError(err)
			a.False(warm.HardErrors)
			a.Equal(int64(2), warm.FilesUnchanged)
		})
	}
}

func TestImportEmlxSourceBudgetCanComplete(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := partialRaw(nil, "one.bin", "two.bin")
	mkMailboxDir(t, root, map[string][]byte{"1.partial.emlx": raw})
	cacheAttachment(t, root, "1", "2", "one.bin", bytes.Repeat([]byte("a"), 200))
	cacheAttachment(t, root, "1", "3", "two.bin", bytes.Repeat([]byte("b"), 200))
	opts := EmlxImportOptions{Identifier: "owner@example.test", MaxMessageBytes: int64(len(raw) + 350)}
	first, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	a.False(first.HardErrors)
	a.Equal(1, countEmlxLedgerEntries(t, st, first.SourceID, "emlx-occurrence", "imported"))
	warm, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	a.Equal(int64(1), warm.FilesUnchanged)
}

func TestImportEmlxSingleOccurrenceShrinkingReplacement(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := partialRaw(nil, "one.bin", "two.bin", "three.bin")
	mkMailboxDir(t, root, map[string][]byte{"1.partial.emlx": raw})
	oldFirst, oldSecond := bytes.Repeat([]byte("a"), 20), bytes.Repeat([]byte("b"), 200)
	cacheAttachment(t, root, "1", "2", "one.bin", oldFirst)
	cacheAttachment(t, root, "1", "3", "two.bin", oldSecond)
	opts := EmlxImportOptions{Identifier: "owner@example.test", AttachmentsDir: filepath.Join(tmp, "blobs"), MaxMessageBytes: int64(len(raw) + 350)}
	first, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	r.False(first.HardErrors)
	newSecond, newThird := bytes.Repeat([]byte("c"), 100), bytes.Repeat([]byte("d"), 90)
	newFirst := bytes.Repeat([]byte("e"), 200)
	cacheAttachment(t, root, "1", "2", "one.bin", newFirst)
	cacheAttachment(t, root, "1", "3", "two.bin", newSecond)
	file := filepath.Join(root, "Messages", "1.partial.emlx")
	grown, _, err := emlx.RestoreAttachments(raw, file, 128<<20)
	r.NoError(err)
	r.Greater(int64(len(grown)), opts.MaxMessageBytes, "first replacement plus shrinking second cannot fit even without third")
	cacheAttachment(t, root, "1", "4", "three.bin", newThird)
	cacheAttachment(t, root, "1", "2", "one.bin", oldFirst)
	feasible, _, err := emlx.RestoreAttachments(raw, file, 128<<20)
	r.NoError(err)
	r.LessOrEqual(int64(len(feasible)), opts.MaxMessageBytes, "old first plus current second and third fit")
	cacheAttachment(t, root, "1", "2", "one.bin", newFirst)
	changed, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	a.True(changed.HardErrors, "first replacement still cannot fit; feasible later parts must nevertheless progress")
	var mid int64
	r.NoError(st.DB().QueryRow("SELECT id FROM messages").Scan(&mid))
	current, err := st.GetMessageRawContext(t.Context(), mid)
	r.NoError(err)
	parsed, err := mime.Parse(current)
	r.NoError(err)
	r.Len(parsed.Attachments, 3)
	a.Equal(oldFirst, parsed.Attachments[0].Content)
	a.Equal(newSecond, parsed.Attachments[1].Content)
	a.Equal(newThird, parsed.Attachments[2].Content)
	a.Zero(countEmlxLedgerEntries(t, st, first.SourceID, "emlx-occurrence", "imported"))
	retry, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	a.True(retry.HardErrors)
	retained, err := st.GetMessageRawContext(t.Context(), mid)
	r.NoError(err)
	a.Equal(current, retained, "feasible parts stay archived on a same-budget retry")
	opts.MaxMessageBytes = int64(len(raw) + 1000)
	completed, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	a.False(completed.HardErrors)
	a.Equal(1, countEmlxLedgerEntries(t, st, first.SourceID, "emlx-occurrence", "imported"))
	warm, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	a.Equal(int64(1), warm.FilesUnchanged)
}

func TestImportEmlxPlainPartialReconciles(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := email.NewMessage().From("sender@example.test").To("owner@example.test").Body("Synthetic text mentioning X-Apple-Content-Length: without a MIME placeholder.").Bytes()
	mkMailboxDir(t, root, map[string][]byte{"1.partial.emlx": raw})
	opts := EmlxImportOptions{Identifier: "owner@example.test"}
	first, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	r.False(first.HardErrors)
	opts.FullReconcile = true
	reconciled, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	a.False(reconciled.HardErrors, "a plain-text partial has no supported attachment work")
	a.Equal(1, countEmlxLedgerEntries(t, st, first.SourceID, "emlx-occurrence", "imported"))
}
