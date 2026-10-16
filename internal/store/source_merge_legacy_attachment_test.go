package store_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMergeSourcesKeepsConflictingLegacyAttachmentDetailsVisible(t *testing.T) {
	for _, test := range []struct {
		name, filename, mimeType string
		hidden                   bool
	}{
		{name: "identical", filename: "current.txt", mimeType: "text/plain", hidden: true},
		{name: "different filename", filename: "historical.txt", mimeType: "text/plain"},
		{name: "different MIME type", filename: "current.txt", mimeType: "application/octet-stream"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			from, err := st.GetOrCreateSource("gmail", "history@example.test")
			require.NoError(err)
			into, err := st.GetOrCreateSource("gmail", "live@example.test")
			require.NoError(err)
			live := emailMergeFixtureMessage(t, st, into.ID, "live", []string{"recipient@example.test"}, true)
			history := emailMergeFixtureMessage(t, st, from.ID, "history", []string{"recipient@example.test"}, true)
			hash := strings.Repeat("a", 64)
			require.NoError(st.UpsertAttachment(live, "current.txt", "text/plain", "synthetic/content", hash, 12))
			require.NoError(st.UpsertAttachment(history, test.filename, test.mimeType, "synthetic/content", hash, 12))
			req := store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID, DryRun: true}
			preview, err := st.MergeSourcesContext(t.Context(), req)
			require.NoError(err)
			req.DryRun = false
			result, err := st.MergeSourcesContext(t.Context(), req)
			require.NoError(err)
			preview.DryRun = false
			assert.Equal(preview, result)
			wantHidden := int64(0)
			if test.hidden {
				wantHidden = 1
			}
			assert.Equal(wantHidden, result.DuplicatesHidden)
			assert.Zero(result.AttachmentsCopied)
			var visible int
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM attachments a
 WHERE a.filename = ? AND a.mime_type = ?
 AND EXISTS (SELECT 1 FROM messages m WHERE m.id = a.message_id AND m.deleted_at IS NULL)`),
				test.filename, test.mimeType).Scan(&visible))
			assert.Equal(1, visible, "historical attachment details remain on a visible message")
		})
	}
}
