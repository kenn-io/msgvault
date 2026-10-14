//go:build linux || darwin

package importer

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
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
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestImportEmlxPendingPartsPreserveNewerOccurrence(t *testing.T) {
	for _, priorReceipt := range []bool{false, true} {
		for _, fault := range []string{"sibling lookup", "attachment storage"} {
			name := "new occurrence/" + fault
			if priorReceipt {
				name = "updated occurrence/" + fault
			}
			t.Run(name, func(t *testing.T) {
				r, a := require.New(t), assert.New(t)
				st, tmp := openTestStore(t)
				rootA, rootB := filepath.Join(tmp, "A.mbox"), filepath.Join(tmp, "B.mbox")
				raw := partialRaw(nil, "one.bin", "two.bin")
				mkMailboxDir(t, rootA, map[string][]byte{"1.partial.emlx": raw})
				opts := EmlxImportOptions{Identifier: "owner@example.test", AttachmentsDir: filepath.Join(tmp, "blobs"), NoResume: true}
				if priorReceipt {
					cacheAttachment(t, rootA, "1", "2", "one.bin", []byte("initial A bytes"))
					first, err := ImportEmlxDir(t.Context(), st, rootA, opts)
					r.NoError(err)
					r.False(first.HardErrors)
				}
				oldPart, newPart := []byte("A committed before failure"), []byte("B committed newer bytes")
				cacheAttachment(t, rootA, "1", "2", "one.bin", oldPart)
				brokenDir := filepath.Join(rootA, "Attachments", "1", "3")
				if fault == "sibling lookup" {
					r.NoError(os.WriteFile(brokenDir, []byte("a file where the part directory belongs"), 0600))
				} else {
					cacheAttachment(t, rootA, "1", "3", "two.bin", []byte("second sibling"))
					_, err := st.DB().Exec(`CREATE TRIGGER fail_second_attachment BEFORE INSERT ON attachments
 WHEN NEW.filename = 'two.bin' BEGIN SELECT RAISE(ABORT, 'synthetic attachment failure'); END`)
					r.NoError(err)
				}
				failed, err := ImportEmlxDir(t.Context(), st, rootA, opts)
				r.NoError(err)
				r.False(failed.HardErrors)
				r.Positive(failed.Errors)
				var messageID int64
				r.NoError(st.DB().QueryRow("SELECT id FROM messages").Scan(&messageID))
				committed, err := st.GetMessageRawContext(t.Context(), messageID)
				r.NoError(err)
				parsed, err := mime.Parse(committed)
				r.NoError(err)
				r.Len(parsed.Attachments, 2)
				r.Equal(oldPart, parsed.Attachments[0].Content, "A must commit its contribution before failing")
				if fault == "attachment storage" {
					_, err := st.DB().Exec("DROP TRIGGER fail_second_attachment")
					r.NoError(err)
				}
				mkMailboxDir(t, rootB, map[string][]byte{"2.partial.emlx": raw})
				cacheAttachment(t, rootB, "2", "2", "one.bin", newPart)
				replaced, err := ImportEmlxDir(t.Context(), st, rootB, opts)
				r.NoError(err)
				r.False(replaced.HardErrors)
				committed, err = st.GetMessageRawContext(t.Context(), messageID)
				r.NoError(err)
				parsed, err = mime.Parse(committed)
				r.NoError(err)
				r.Equal(newPart, parsed.Attachments[0].Content)

				retried, err := ImportEmlxDir(t.Context(), st, rootA, opts)
				r.NoError(err)
				a.False(retried.HardErrors)
				a.Equal(fault == "sibling lookup", retried.Errors > 0, "a broken sibling stays retryable")
				warmB, err := ImportEmlxDir(t.Context(), st, rootB, opts)
				r.NoError(err)
				a.False(warmB.HardErrors)
				a.Equal(int64(1), warmB.FilesUnchanged, "B's completed occurrence does not replay its newer bytes")
				if fault == "sibling lookup" {
					r.NoError(os.Remove(brokenDir))
					cacheAttachment(t, rootA, "1", "3", "two.bin", []byte("second sibling"))
				}
				finished, err := ImportEmlxDir(t.Context(), st, rootA, opts)
				r.NoError(err)
				a.False(finished.HardErrors, "the unfinished sibling must remain retryable")
				retained, err := st.GetMessageRawContext(t.Context(), messageID)
				r.NoError(err)
				parsed, err = mime.Parse(retained)
				r.NoError(err)
				r.Len(parsed.Attachments, 2)
				a.Equal(newPart, parsed.Attachments[0].Content)
				a.Equal([]byte("second sibling"), parsed.Attachments[1].Content)
				engine := query.NewSQLiteEngine(st.DB())
				message, err := engine.GetMessage(t.Context(), messageID)
				r.NoError(err)
				exported := export.AttachmentsToDir(t.TempDir(), opts.AttachmentsDir, message.Attachments)
				r.Empty(exported.Errors)
				r.Len(exported.Files, 2)
				contents := make(map[string]string)
				for _, file := range exported.Files {
					content, err := os.ReadFile(file.Path)
					r.NoError(err)
					contents[filepath.Base(file.Path)] = string(content)
				}
				a.Equal(map[string]string{"one.bin": string(newPart), "two.bin": "second sibling"}, contents)
			})
		}
	}
}

// A process can stop after ingestion commits occurrence A's attachment but
// before A's receipt records it. The target's intent must still credit A, so
// a later replacement by B is not overwritten when A retries.
func TestImportEmlxInterruptedIngestCreditsIntent(t *testing.T) {
	for _, committed := range []bool{true, false} {
		t.Run(fmt.Sprintf("raw committed=%t", committed), func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			st, tmp := openTestStore(t)
			rootA, rootB := filepath.Join(tmp, "A.mbox"), filepath.Join(tmp, "B.mbox")
			raw := partialRaw(nil, "one.bin")
			mkMailboxDir(t, rootA, map[string][]byte{"1.partial.emlx": raw})
			cacheAttachment(t, rootA, "1", "2", "one.bin", []byte("initial A bytes"))
			opts := EmlxImportOptions{Identifier: "owner@example.test", AttachmentsDir: filepath.Join(tmp, "blobs")}
			_, err := ImportEmlxDir(t.Context(), st, rootA, opts)
			r.NoError(err)

			aBytes, bBytes := []byte("A bytes before the stop"), []byte("B newer bytes")
			cacheAttachment(t, rootA, "1", "2", "one.bin", aBytes)
			stopped := defaultEmlxImportIO(opts)
			ingest := stopped.ingest
			stopped.ingest = func(
				ctx context.Context, s *store.Store, sid int64, identifier, dest string, labels []int64,
				target, hash string, raw []byte, date time.Time, log *slog.Logger,
			) error {
				if committed {
					r.NoError(ingest(ctx, s, sid, identifier, dest, labels, target, hash, raw, date, log))
				}
				return errors.New("simulated process stop")
			}
			_, err = importEmlxDir(t.Context(), st, rootA, opts, stopped)
			r.NoError(err)
			var messageID int64
			r.NoError(st.DB().QueryRow("SELECT id FROM messages").Scan(&messageID))
			attachment := func() []byte {
				t.Helper()
				current, err := st.GetMessageRawContext(t.Context(), messageID)
				r.NoError(err)
				parsed, err := mime.Parse(current)
				r.NoError(err)
				r.Len(parsed.Attachments, 1)
				return parsed.Attachments[0].Content
			}
			if !committed {
				a.Equal([]byte("initial A bytes"), attachment())
				_, err = ImportEmlxDir(t.Context(), st, rootA, opts)
				r.NoError(err)
				a.Equal(aBytes, attachment(), "an uncommitted write is retried, not credited")
				return
			}
			a.Equal(aBytes, attachment())

			mkMailboxDir(t, rootB, map[string][]byte{"1.partial.emlx": raw})
			cacheAttachment(t, rootB, "1", "2", "one.bin", bBytes)
			_, err = ImportEmlxDir(t.Context(), st, rootB, opts)
			r.NoError(err)
			a.Equal(bBytes, attachment())

			retried, err := ImportEmlxDir(t.Context(), st, rootA, opts)
			r.NoError(err)
			a.False(retried.HardErrors)
			a.Equal(bBytes, attachment(), "A's committed bytes were credited, so A does not replay them")
			warmB, err := ImportEmlxDir(t.Context(), st, rootB, opts)
			r.NoError(err)
			a.Equal(int64(1), warmB.FilesUnchanged)
			a.Equal(bBytes, attachment())
		})
	}
}

// When an earlier file in the same chunk credits the interrupted occurrence,
// that occurrence must use its credited receipt, not the one read before.
func TestImportEmlxCreditReachesLaterFileInChunk(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := partialRaw(nil, "one.bin")
	mkMailboxDir(t, root, map[string][]byte{"2.partial.emlx": raw})
	cacheAttachment(t, root, "2", "2", "one.bin", []byte("initial A bytes"))
	opts := EmlxImportOptions{Identifier: "owner@example.test", AttachmentsDir: filepath.Join(tmp, "blobs")}
	_, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)

	cacheAttachment(t, root, "2", "2", "one.bin", []byte("A bytes before the stop"))
	stopped := defaultEmlxImportIO(opts)
	ingest := stopped.ingest
	stopped.ingest = func(
		ctx context.Context, s *store.Store, sid int64, identifier, dest string, labels []int64,
		target, hash string, raw []byte, date time.Time, log *slog.Logger,
	) error {
		r.NoError(ingest(ctx, s, sid, identifier, dest, labels, target, hash, raw, date, log))
		return errors.New("simulated process stop")
	}
	_, err = importEmlxDir(t.Context(), st, root, opts, stopped)
	r.NoError(err)

	// 1.partial.emlx sorts before A, so it finishes the interrupted message
	// first and writes newer bytes in the same chunk that later visits A.
	bBytes := []byte("B newer bytes")
	mkMailboxDir(t, root, map[string][]byte{"1.partial.emlx": raw})
	cacheAttachment(t, root, "1", "2", "one.bin", bBytes)
	summary, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	a.Zero(summary.Errors)
	var messageID int64
	r.NoError(st.DB().QueryRow("SELECT id FROM messages").Scan(&messageID))
	current, err := st.GetMessageRawContext(t.Context(), messageID)
	r.NoError(err)
	parsed, err := mime.Parse(current)
	r.NoError(err)
	r.Len(parsed.Attachments, 1)
	a.Equal(bBytes, parsed.Attachments[0].Content, "A does not replay bytes already credited to it")
}

// A file whose interrupted write is credited later may already describe a
// different message. Its newer receipt must survive the historical credit.
func TestImportEmlxCreditKeepsNewerReceiptForOtherMessage(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	rootA, rootB := filepath.Join(tmp, "A.mbox"), filepath.Join(tmp, "B.mbox")
	oldRaw := partialRaw(nil, "one.bin")
	mkMailboxDir(t, rootA, map[string][]byte{"1.partial.emlx": oldRaw})
	cacheAttachment(t, rootA, "1", "2", "one.bin", []byte("A bytes for the old message"))
	opts := EmlxImportOptions{Identifier: "owner@example.test", AttachmentsDir: filepath.Join(tmp, "blobs")}
	stopped := defaultEmlxImportIO(opts)
	ingest := stopped.ingest
	stopped.ingest = func(
		ctx context.Context, s *store.Store, sid int64, identifier, dest string, labels []int64,
		target, hash string, raw []byte, date time.Time, log *slog.Logger,
	) error {
		r.NoError(ingest(ctx, s, sid, identifier, dest, labels, target, hash, raw, date, log))
		return errors.New("simulated process stop")
	}
	_, err := importEmlxDir(t.Context(), st, rootA, opts, stopped)
	r.NoError(err)

	newRaw := partialRaw([]string{"Message-ID: <newer@example.test>"}, "one.bin")
	mkMailboxDir(t, rootA, map[string][]byte{"1.partial.emlx": newRaw})
	moved, err := ImportEmlxDir(t.Context(), st, rootA, opts)
	r.NoError(err)
	a.Zero(moved.Errors)
	receiptFor := func() (emlxReceipt, string) {
		t.Helper()
		var checksum, status string
		r.NoError(st.DB().QueryRow(`SELECT checksum, status FROM source_import_items
 WHERE provider = 'emlx-occurrence'`).Scan(&checksum, &status))
		var receipt emlxReceipt
		r.NoError(json.Unmarshal([]byte(checksum), &receipt))
		return receipt, status
	}
	before, status := receiptFor()
	r.Equal("imported", status)

	mkMailboxDir(t, rootB, map[string][]byte{"1.partial.emlx": oldRaw})
	cacheAttachment(t, rootB, "1", "2", "one.bin", []byte("B bytes for the old message"))
	_, err = ImportEmlxDir(t.Context(), st, rootB, opts)
	r.NoError(err)
	var receipt emlxReceipt
	var checksum string
	r.NoError(st.DB().QueryRow(st.Rebind(`SELECT checksum, status FROM source_import_items
 WHERE provider = 'emlx-occurrence' AND provider_id = ?`), before.ID).Scan(&checksum, &status))
	r.NoError(json.Unmarshal([]byte(checksum), &receipt))
	a.Equal("imported", status, "the historical credit must not reopen the newer receipt")
	a.Equal(before, receipt)
}

// stoppedAfterCommit ingests normally, then reports a stop, leaving the state
// a process killed between ingestion and receipt publication would leave.
func stoppedAfterCommit(t *testing.T, opts EmlxImportOptions) emlxImportIO {
	t.Helper()
	io := defaultEmlxImportIO(opts)
	ingest := io.ingest
	io.ingest = func(
		ctx context.Context, s *store.Store, sid int64, identifier, dest string, labels []int64,
		target, hash string, raw []byte, date time.Time, log *slog.Logger,
	) error {
		require.NoError(t, ingest(ctx, s, sid, identifier, dest, labels, target, hash, raw, date, log))
		return errors.New("simulated process stop")
	}
	return io
}

func messageAttachment(t *testing.T, st *store.Store, rfcID string) []byte {
	t.Helper()
	var messageID int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE rfc822_message_id = ?`), rfcID).
		Scan(&messageID))
	raw, err := st.GetMessageRawContext(t.Context(), messageID)
	require.NoError(t, err)
	parsed, err := mime.Parse(raw)
	require.NoError(t, err)
	require.Len(t, parsed.Attachments, 1)
	return parsed.Attachments[0].Content
}

// A file that switches to another message and stops after that write landed
// must still be credited for it, even though its last receipt named the old
// message.
func TestImportEmlxCreditFollowsFileThatSwitchedMessages(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	rootA, rootB := filepath.Join(tmp, "A.mbox"), filepath.Join(tmp, "B.mbox")
	mkMailboxDir(t, rootA, map[string][]byte{"1.partial.emlx": partialRaw(nil, "one.bin")})
	cacheAttachment(t, rootA, "1", "2", "one.bin", []byte("A bytes for the old message"))
	opts := EmlxImportOptions{Identifier: "owner@example.test", AttachmentsDir: filepath.Join(tmp, "blobs")}
	_, err := ImportEmlxDir(t.Context(), st, rootA, opts)
	r.NoError(err)

	switched := partialRaw([]string{"Message-ID: <switched@example.test>"}, "one.bin")
	mkMailboxDir(t, rootA, map[string][]byte{"1.partial.emlx": switched})
	cacheAttachment(t, rootA, "1", "2", "one.bin", []byte("A bytes for the new message"))
	_, err = importEmlxDir(t.Context(), st, rootA, opts, stoppedAfterCommit(t, opts))
	r.NoError(err)

	bBytes := []byte("B newer bytes")
	mkMailboxDir(t, rootB, map[string][]byte{"1.partial.emlx": switched})
	cacheAttachment(t, rootB, "1", "2", "one.bin", bBytes)
	_, err = ImportEmlxDir(t.Context(), st, rootB, opts)
	r.NoError(err)
	a.Equal(bBytes, messageAttachment(t, st, "switched@example.test"))

	retried, err := ImportEmlxDir(t.Context(), st, rootA, opts)
	r.NoError(err)
	a.Zero(retried.Errors)
	a.Equal(bBytes, messageAttachment(t, st, "switched@example.test"),
		"A's landed write was credited, so A does not replay its older bytes")
}

// A file whose content already matches the archive writes no in-progress
// marker. If its completed receipt cannot be saved, the pending receipt must
// still record the attachments it accounts for.
func TestImportEmlxFailedReceiptSaveKeepsAttachmentRecord(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	rootA, rootB := filepath.Join(tmp, "A.mbox"), filepath.Join(tmp, "B.mbox")
	raw := partialRaw([]string{"Message-ID: <receipt-save@example.test>"}, "one.bin")
	shared := []byte("bytes both copies hold")
	mkMailboxDir(t, rootB, map[string][]byte{"1.partial.emlx": raw})
	cacheAttachment(t, rootB, "1", "2", "one.bin", shared)
	opts := EmlxImportOptions{Identifier: "owner@example.test", AttachmentsDir: filepath.Join(tmp, "blobs")}
	_, err := ImportEmlxDir(t.Context(), st, rootB, opts)
	r.NoError(err)

	// A hex digest and a fixed path: safe to embed, since triggers cannot bind.
	receiptA := emlxDigest(rootA) + "/Messages/1.partial.emlx"
	for _, event := range []string{"INSERT", "UPDATE"} {
		_, err = st.DB().Exec(`CREATE TRIGGER block_completed_receipt_` + event + ` BEFORE ` + event +
			` ON source_import_items WHEN NEW.provider_id = '` + receiptA + `' AND NEW.status = 'imported'
 BEGIN SELECT RAISE(ABORT, 'synthetic receipt failure'); END`)
		r.NoError(err)
	}
	mkMailboxDir(t, rootA, map[string][]byte{"1.partial.emlx": raw})
	cacheAttachment(t, rootA, "1", "2", "one.bin", shared)
	failed, err := ImportEmlxDir(t.Context(), st, rootA, opts)
	r.NoError(err)
	r.True(failed.HardErrors, "the blocked receipt save is a database failure")
	for _, event := range []string{"INSERT", "UPDATE"} {
		_, err = st.DB().Exec(`DROP TRIGGER block_completed_receipt_` + event)
		r.NoError(err)
	}

	bBytes := []byte("B newer bytes")
	cacheAttachment(t, rootB, "1", "2", "one.bin", bBytes)
	_, err = ImportEmlxDir(t.Context(), st, rootB, opts)
	r.NoError(err)
	a.Equal(bBytes, messageAttachment(t, st, "receipt-save@example.test"))
	_, err = ImportEmlxDir(t.Context(), st, rootA, opts)
	r.NoError(err)
	a.Equal(bBytes, messageAttachment(t, st, "receipt-save@example.test"),
		"A's unchanged copy does not replay over B's newer bytes")
}

// A message archived before receipts keeps the attachment an earlier import
// stored, even when the copy that supplied it is gone and a remaining copy
// caches different bytes. That copy may replace it only after its own cache
// changes.
func TestImportEmlxLegacyArchiveKeepsStoredAttachment(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := partialRaw([]string{"Message-ID: <legacy-merge@example.test>"}, "one.bin")
	mkMailboxDir(t, root, map[string][]byte{"1.partial.emlx": raw})
	opts := EmlxImportOptions{Identifier: "owner@example.test", AttachmentsDir: filepath.Join(tmp, "blobs")}

	// An older importer stored the copy that 1.partial.emlx supplied.
	kept := []byte("bytes the older import kept")
	cacheAttachment(t, root, "1", "2", "one.bin", kept)
	seedRaw, restored, err := emlx.RestoreAttachments(raw, filepath.Join(root, "Messages", "1.partial.emlx"), 128<<20)
	r.NoError(err)
	r.Equal(1, restored)
	source, err := st.GetOrCreateSource("apple-mail", opts.Identifier)
	r.NoError(err)
	label, err := st.EnsureLabel(source.ID, "Inbox", "Inbox", "user")
	r.NoError(err)
	hash := emlxRawDigest(raw)
	r.NoError(IngestRawMessage(t.Context(), st, source.ID, opts.Identifier, opts.AttachmentsDir,
		[]int64{label}, "emlx-"+hash, hash, seedRaw, time.Now(), slog.Default()))
	r.NoError(os.RemoveAll(filepath.Join(root, "Messages")))
	r.NoError(os.RemoveAll(filepath.Join(root, "Attachments")))

	mkMailboxDir(t, root, map[string][]byte{"2.partial.emlx": raw})
	cacheAttachment(t, root, "2", "2", "one.bin", []byte("a remaining copy's other bytes"))
	for range 2 {
		summary, err := ImportEmlxDir(t.Context(), st, root, opts)
		r.NoError(err)
		a.Zero(summary.Errors)
		a.Equal(kept, messageAttachment(t, st, "legacy-merge@example.test"))
	}

	// The legacy mark outlives the first completion: a copy found later
	// without its own record cannot replace the stored attachment either.
	mkMailboxDir(t, root, map[string][]byte{"3.partial.emlx": raw})
	cacheAttachment(t, root, "3", "2", "one.bin", []byte("a copy found later"))
	_, err = ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	a.Equal(kept, messageAttachment(t, st, "legacy-merge@example.test"))

	changed := []byte("the remaining copy changed")
	cacheAttachment(t, root, "2", "2", "one.bin", changed)
	_, err = ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	a.Equal(changed, messageAttachment(t, st, "legacy-merge@example.test"),
		"a change observed in the copy's own cache can replace the stored attachment")
}

// A preserved attachment too large to add to the archive under the combined
// limit is still read so the copy records its hash. Otherwise a later,
// smaller change to that cache would look like a first sighting and be
// recorded without replacing the stored attachment.
func TestImportEmlxLegacyOversizedCopyRecordsHash(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := partialRaw([]string{"Message-ID: <legacy-budget@example.test>"}, "one.bin", "two.bin")
	mkMailboxDir(t, root, map[string][]byte{"1.partial.emlx": raw})
	kept := bytes.Repeat([]byte("k"), 100)
	cacheAttachment(t, root, "1", "2", "one.bin", kept)
	cacheAttachment(t, root, "1", "3", "two.bin", bytes.Repeat([]byte("m"), 100))
	seedRaw, restored, err := emlx.RestoreAttachments(raw, filepath.Join(root, "Messages", "1.partial.emlx"), 128<<20)
	r.NoError(err)
	r.Equal(2, restored)
	opts := EmlxImportOptions{Identifier: "owner@example.test", AttachmentsDir: filepath.Join(tmp, "blobs")}
	source, err := st.GetOrCreateSource("apple-mail", opts.Identifier)
	r.NoError(err)
	label, err := st.EnsureLabel(source.ID, "Inbox", "Inbox", "user")
	r.NoError(err)
	hash := emlxRawDigest(raw)
	r.NoError(IngestRawMessage(t.Context(), st, source.ID, opts.Identifier, opts.AttachmentsDir,
		[]int64{label}, "emlx-"+hash, hash, seedRaw, time.Now(), slog.Default()))
	r.NoError(os.RemoveAll(filepath.Join(root, "Messages")))
	r.NoError(os.RemoveAll(filepath.Join(root, "Attachments")))

	// The remaining copy's attachment fits the limit by itself, but not on top
	// of the archive's current size.
	larger := bytes.Repeat([]byte("n"), 220)
	mkMailboxDir(t, root, map[string][]byte{"2.partial.emlx": raw})
	cacheAttachment(t, root, "2", "2", "one.bin", larger)
	file := filepath.Join(root, "Messages", "2.partial.emlx")
	opts.MaxMessageBytes = int64(len(seedRaw)) + 100
	alone, err := emlx.ParseFile(file, opts.MaxMessageBytes)
	r.NoError(err)
	r.Equal(1, alone.RestoredAttachments, "the copy's attachment must fit the limit by itself")
	r.Greater(int64(len(seedRaw)+len(alone.Raw)-len(raw)), opts.MaxMessageBytes,
		"adding it to the archive must exceed the limit")

	firstAttachment := func() []byte {
		t.Helper()
		var messageID int64
		r.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&messageID))
		archived, err := st.GetMessageRawContext(t.Context(), messageID)
		r.NoError(err)
		parsed, err := mime.Parse(archived)
		r.NoError(err)
		r.Len(parsed.Attachments, 2)
		return parsed.Attachments[0].Content
	}
	first, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	a.Zero(first.Errors)
	a.Equal(kept, firstAttachment())

	smaller := []byte("a smaller change")
	cacheAttachment(t, root, "2", "2", "one.bin", smaller)
	_, err = ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	a.Equal(smaller, firstAttachment(),
		"the copy's own cache changed, so its new attachment replaces the stored one")
}
