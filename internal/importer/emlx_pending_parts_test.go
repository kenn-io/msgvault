//go:build linux || darwin

package importer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/query"
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
				r.True(failed.HardErrors)
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
				a.Equal(fault == "sibling lookup", retried.HardErrors)
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
