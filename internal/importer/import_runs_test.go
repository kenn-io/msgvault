package importer

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportEMLDirRecordsNativeLogicalRuns(t *testing.T) {
	for _, sourceType := range []string{"eml", "gmail"} {
		t.Run(sourceType, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st, tmp := testutil.NewTestStore(t), t.TempDir()
			const owner = "owner:synthetic-import"
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: owner, ImportNotifications: true})
			require.NoError(err)
			root := filepath.Join(tmp, "Synthetic.mailbox")
			require.NoError(os.MkdirAll(root, 0o700))
			raw := []byte("From: sender@example.net\r\nTo: owner@example.net\r\nMessage-ID: <logical-run@example.net>\r\nSubject: Synthetic import\r\n\r\nSynthetic body\r\n")
			require.NoError(os.WriteFile(filepath.Join(root, "message.eml"), raw, 0o600))
			opts := EMLImportOptions{Identifier: "owner@example.net", SourceType: sourceType, NoResume: true}
			first, err := ImportEMLDir(t.Context(), st, root, opts)
			require.NoError(err)
			assert.Equal(int64(1), first.MessagesAdded)
			second, err := ImportEMLDir(t.Context(), st, root, opts)
			require.NoError(err)
			assert.Equal(int64(1), second.MessagesSkipped)
			var count int
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM import_runs r WHERE r.source_id=? AND r.principal=? AND r.import_format='eml' AND NOT r.notify_imports AND EXISTS (SELECT 1 FROM import_run_attempts a WHERE a.import_run_id=r.id AND EXISTS (SELECT 1 FROM sync_runs s WHERE s.id=a.sync_run_id AND s.status='completed'))`), first.SourceID, owner).Scan(&count))
			assert.Equal(2, count, "completed native calls create fresh logical runs; parser format is independent of a custom source label")
			var messages int
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&messages))
			assert.Equal(1, messages, "logical run tracking preserves native deduplication")
		})
	}
}
