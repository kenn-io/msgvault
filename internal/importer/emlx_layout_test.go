//go:build linux || darwin

package importer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
)

// A recoverable missing delimiter must not prevent first archival or strand
// legacy placeholders. Required work still uses the real strict MIME path.
func TestImportEmlxRecoverableClosingBoundary(t *testing.T) {
	for _, ending := range []string{"closed", "missing", "trailing whitespace"} {
		for _, legacy := range []bool{false, true} {
			name := "fresh"
			if legacy {
				name = "legacy"
			}
			t.Run(ending+"/"+name, func(t *testing.T) {
				r, a := require.New(t), assert.New(t)
				st, tmp := openTestStore(t)
				r.True(st.FTS5Available())
				root := filepath.Join(tmp, "Inbox.mbox")
				closed := partialRaw([]string{"Message-ID: <closing-layout@example.test>"}, "old.bin", "new.bin")
				withEnding := func(raw []byte) []byte {
					switch ending {
					case "missing":
						return bytes.TrimSuffix(raw, []byte("--=-b--\n"))
					case "trailing whitespace":
						return bytes.Replace(raw, []byte("--=-b--\n"), []byte("--=-b-- \t\n"), 1)
					default:
						return raw
					}
				}
				raw := withEnding(closed)
				parsed, err := mime.ParseWithRecovery(raw, "")
				r.NoError(err, "the real parser must recover this layout, unlike fatal MIME")
				r.Contains(parsed.BodyText, "see attached")
				mkMailboxDir(t, root, map[string][]byte{"1.partial.emlx": raw})
				file := filepath.Join(root, "Messages", "1.partial.emlx")
				old, fresh := []byte("retained synthetic attachment"), []byte("new synthetic attachment")
				cacheAttachment(t, root, "1", "2", "old.bin", old)
				opts := EmlxImportOptions{Identifier: "owner@example.test", AttachmentsDir: filepath.Join(tmp, "blobs")}
				if legacy {
					// Seed a genuine old archive with one downloaded part and one
					// placeholder, before the occurrence/target receipts existed.
					seedRaw, restored, err := emlx.RestoreAttachments(closed, file, 128<<20)
					r.NoError(err)
					r.Equal(1, restored)
					seedRaw = withEnding(seedRaw)
					source, err := st.GetOrCreateSource("apple-mail", opts.Identifier)
					r.NoError(err)
					label, err := st.EnsureLabel(source.ID, "legacy", "legacy", "user")
					r.NoError(err)
					sum := sha256.Sum256(raw)
					hash := hex.EncodeToString(sum[:])
					r.NoError(IngestRawMessage(t.Context(), st, source.ID, opts.Identifier,
						opts.AttachmentsDir, []int64{label}, "emlx-"+hash, hash, seedRaw, time.Now(), slog.Default()))
					r.NoError(os.Remove(filepath.Join(root, "Attachments", "1", "2", "old.bin")))
					r.Zero(countEmlxLedgerEntries(t, st, source.ID, "emlx-occurrence", "imported"))
				}
				cacheAttachment(t, root, "1", "3", "new.bin", fresh)
				msg, err := emlx.ParseFile(file, 128<<20)
				r.NoError(err)
				r.NoError(msg.RestorationError)
				r.Len(msg.RestorationParts, 2, "fixture must reach the part merge")
				_, err = mime.ParseWithRecovery(msg.Raw, "")
				r.NoError(err)

				first, err := ImportEmlxDir(t.Context(), st, root, opts)
				r.NoError(err)
				a.False(first.HardErrors)
				a.Zero(first.Errors)
				var mid int64
				r.NoError(st.DB().QueryRow("SELECT id FROM messages").Scan(&mid))
				archived, err := st.GetMessageRawContext(t.Context(), mid)
				r.NoError(err)
				parsed, err = mime.ParseWithRecovery(archived, "")
				r.NoError(err)
				r.Len(parsed.Attachments, 2)
				a.Equal(old, parsed.Attachments[0].Content)
				a.Equal(fresh, parsed.Attachments[1].Content)
				for _, content := range [][]byte{old, fresh} {
					sum := sha256.Sum256(content)
					hash := hex.EncodeToString(sum[:])
					var stored int
					r.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM attachments WHERE message_id = ? AND content_hash = ?"), mid, hash).Scan(&stored))
					a.Equal(1, stored)
					blobPath, err := export.StoragePath(opts.AttachmentsDir, hash)
					r.NoError(err)
					blob, err := os.ReadFile(blobPath)
					r.NoError(err)
					a.Equal(content, blob)
				}
				labels, err := st.MessageLabelIDsContext(t.Context(), mid)
				r.NoError(err)
				wantLabels := 1
				if legacy {
					wantLabels = 2
				}
				a.Len(labels, wantLabels)
				var indexed int
				r.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'attached'").Scan(&indexed))
				a.Equal(1, indexed)
				a.Equal(1, countEmlxLedgerEntries(t, st, first.SourceID, "emlx-occurrence", "imported"))
				warm, err := ImportEmlxDir(t.Context(), st, root, opts)
				r.NoError(err)
				a.False(warm.HardErrors)
				a.Equal(int64(1), warm.FilesUnchanged)
				opts.FullReconcile = true
				reconciled, err := ImportEmlxDir(t.Context(), st, root, opts)
				r.NoError(err)
				a.False(reconciled.HardErrors)
				retained, err := st.GetMessageRawContext(t.Context(), mid)
				r.NoError(err)
				a.Equal(archived, retained, "full reconciliation must retain vanished cached parts")
			})
		}
	}
}

func TestImportEmlxFatalUnclosedPlaceholderPreservesRaw(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := partialRaw([]string{"Message-ID: <fatal-layout@example.test>"}, "part.bin")
	raw = bytes.TrimSuffix(raw, []byte("--=-b--\n"))
	raw = bytes.Replace(raw, []byte("multipart/mixed"), []byte("multipart mixed"), 1)
	mkMailboxDir(t, root, map[string][]byte{"1.partial.emlx": raw})
	cacheAttachment(t, root, "1", "2", "part.bin", []byte("synthetic cached part"))
	msg, err := emlx.ParseFile(filepath.Join(root, "Messages", "1.partial.emlx"), 128<<20)
	r.NoError(err)
	r.Len(msg.RestorationParts, 1)
	_, err = mime.ParseWithRecovery(msg.Raw, "")
	r.Error(err, "invalid multipart media type is genuinely fatal, not a layout warning")
	for range 2 {
		result, err := ImportEmlxDir(t.Context(), st, root, EmlxImportOptions{Identifier: "owner@example.test"})
		r.NoError(err)
		a.False(result.HardErrors)
		a.Zero(result.Errors, "fatal MIME completes with salvaged headers")
		a.Equal(1, countEmlxLedgerEntries(t, st, result.SourceID, "emlx-occurrence", "imported"))
		a.Equal(1, countEmlxLedgerEntries(t, st, result.SourceID, "emlx-target", "imported"))
		var mid int64
		r.NoError(st.DB().QueryRow("SELECT id FROM messages").Scan(&mid))
		archived, err := st.GetMessageRawContext(t.Context(), mid)
		r.NoError(err)
		a.Equal(msg.Raw, archived, "salvage must preserve restore-attempted raw")
	}
}
