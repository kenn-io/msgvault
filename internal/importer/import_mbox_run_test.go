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

func TestImportMboxRecordsNativeLogicalRuns(t *testing.T) {
	for _, sourceType := range []string{"mbox", "gmail"} {
		t.Run(sourceType, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st, tmp := testutil.NewTestStore(t), t.TempDir()
			const owner = "owner:synthetic-mbox-import"
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: owner, ImportNotifications: true})
			require.NoError(err)
			path := filepath.Join(tmp, "synthetic.mbox")
			raw := []byte("From sender@example.net Mon Jan 1 12:00:00 2024\nFrom: sender@example.net\nTo: owner@example.net\nMessage-ID: <native-mbox-run@example.net>\nSubject: Synthetic logical Mbox import\n\nSynthetic Mbox body\n")
			require.NoError(os.WriteFile(path, raw, 0o600))
			opts := MboxImportOptions{Identifier: "owner@example.net", SourceType: sourceType, NoResume: true}
			first, err := ImportMbox(t.Context(), st, path, opts)
			require.NoError(err)
			require.NotNil(first)
			assert.Equal(int64(1), first.MessagesAdded)
			second, err := ImportMbox(t.Context(), st, path, opts)
			require.NoError(err)
			require.NotNil(second)
			var count int
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM import_runs r WHERE r.source_id=? AND r.principal=? AND r.import_format='mbox' AND NOT r.notify_imports AND r.completed AND EXISTS (SELECT 1 FROM import_run_attempts a WHERE a.import_run_id=r.id AND EXISTS (SELECT 1 FROM sync_runs s WHERE s.id=a.sync_run_id AND s.status='completed'))`), first.SourceID, owner).Scan(&count))
			assert.Equal(2, count, "native Mbox calls need their own durable runs, independent of a custom source label")
			var messages int
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&messages))
			assert.Equal(1, messages, "logical runs preserve native Mbox deduplication")
		})
	}
}
