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

func TestImportEmlxEmptyReplacementUpdatesExportedAttachment(t *testing.T) {
	for _, tc := range []struct {
		name    string
		missing bool
		want    string
	}{
		{name: "empty sibling", want: ""},
		{name: "missing sibling", missing: true, want: "original attachment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			st, tmp := openTestStore(t)
			root := filepath.Join(tmp, "Inbox.mbox")
			mkMailboxDir(t, root, map[string][]byte{"1.partial.emlx": partialRaw(nil, "part.bin", "uncached.bin")})
			cacheAttachment(t, root, "1", "2", "part.bin", []byte("original attachment"))
			opts := EmlxImportOptions{Identifier: "owner@example.test", AttachmentsDir: filepath.Join(tmp, "blobs")}
			first, err := ImportEmlxDir(t.Context(), st, root, opts)
			r.NoError(err)
			r.False(first.HardErrors)
			var messageID int64
			r.NoError(st.DB().QueryRow("SELECT id FROM messages").Scan(&messageID))
			engine := query.NewSQLiteEngine(st.DB())
			before, err := engine.GetMessage(t.Context(), messageID)
			r.NoError(err)
			r.Len(before.Attachments, 1)

			if tc.missing {
				r.NoError(os.Remove(filepath.Join(root, "Attachments", "1", "2", "part.bin")))
			} else {
				cacheAttachment(t, root, "1", "2", "part.bin", []byte{})
			}
			changed, err := ImportEmlxDir(t.Context(), st, root, opts)
			r.NoError(err)
			r.False(changed.HardErrors)
			archived, err := st.GetMessageRawContext(t.Context(), messageID)
			r.NoError(err)
			parsed, err := mime.Parse(archived)
			r.NoError(err)
			r.Len(parsed.Attachments, 2)
			a.Equal(tc.want, string(parsed.Attachments[0].Content))

			after, err := engine.GetMessage(t.Context(), messageID)
			r.NoError(err)
			r.Len(after.Attachments, 1)
			a.Equal(before.Attachments[0].ID, after.Attachments[0].ID)
			a.Equal(int64(len(tc.want)), after.Attachments[0].Size)
			exported := export.AttachmentsToDir(t.TempDir(), opts.AttachmentsDir, after.Attachments)
			r.Empty(exported.Errors)
			r.Len(exported.Files, 1)
			content, err := os.ReadFile(exported.Files[0].Path)
			r.NoError(err)
			a.Equal(tc.want, string(content), "attachment exports must match the accepted MIME replacement")
			a.Equal(1, countEmlxLedgerEntries(t, st, first.SourceID, "emlx-occurrence", "imported"))
			warm, err := ImportEmlxDir(t.Context(), st, root, opts)
			r.NoError(err)
			a.False(warm.HardErrors)
			a.Equal(int64(1), warm.FilesUnchanged)
		})
	}
}
