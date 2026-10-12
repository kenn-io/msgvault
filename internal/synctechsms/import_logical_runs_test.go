package synctechsms

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func syntheticSynctechSMSFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "messages.xml")
	Require.NoError(t, os.WriteFile(path, []byte(`<smses count="1"><sms address="+15550000002" date="1717214400000" type="1" body="Synthetic logical SMS" /></smses>`), 0o600))
	return path
}

func TestImportSynctechRecordsFreshSilentLogicalRuns(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	path := syntheticSynctechSMSFile(t)
	imp := NewImporter(st, ImportOptions{OwnerPhone: "+15550000001", IncludeSMS: true})
	for range 2 {
		_, err := imp.ImportPath(path)
		require.NoError(err)
	}
	rows, err := st.DB().Query(`SELECT r.id,r.import_format,r.notify_imports,r.completed,s.status FROM import_runs r JOIN import_run_attempts a ON a.import_run_id=r.id JOIN sync_runs s ON s.id=a.sync_run_id ORDER BY s.id`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var ids []string
	for rows.Next() {
		var id, format, status string
		var notify, completed bool
		require.NoError(rows.Scan(&id, &format, &notify, &completed, &status))
		assert.NotEmpty(id)
		assert.Equal(SourceType, format)
		assert.False(notify)
		assert.True(completed)
		assert.Equal("completed", status)
		ids = append(ids, id)
	}
	require.NoError(rows.Err())
	require.Len(ids, 2, "non-resumable explicit imports create fresh frozen logical runs")
	assert.NotEqual(ids[0], ids[1])
	var messages, decisions, events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_message_decisions`).Scan(&decisions))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Equal(1, messages)
	assert.Equal(1, decisions)
	assert.Zero(events)
}

func TestImportSynctechPollingCannotInheritImportAuthority(t *testing.T) {
	for _, entry := range []string{"path", "files"} {
		t.Run(entry, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-synctech", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: SourceType, Kinds: []string{"message"}}}})
			require.NoError(err)
			source, err := st.GetOrCreateSource(SourceType, "+15550000001")
			require.NoError(err)
			syncID, err := st.StartSync(source.ID, AdapterName)
			require.NoError(err)
			scoped := st.ScopedToSync(source.ID, syncID)
			run, err := scoped.BeginNativeImportAttempt(t.Context(), source.ID, SourceType, true, syncID, 0)
			require.NoError(err)
			scoped, err = scoped.WithImportRun(t.Context(), run.ID)
			require.NoError(err)
			imp := NewImporter(scoped, ImportOptions{OwnerPhone: "+15550000001", IncludeSMS: true})
			path := syntheticSynctechSMSFile(t)
			var summary ImportSummary
			if entry == "path" {
				summary, err = imp.ImportPathIntoSource(source.ID, path)
			} else {
				files, discoverErr := DiscoverBackupFiles(path)
				require.NoError(discoverErr)
				summary, err = imp.ImportFilesIntoSource(source.ID, files)
			}
			require.NoError(err)
			assert.Equal(1, summary.SMSImported)
			var decisions, events, runs int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_message_decisions`).Scan(&decisions))
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_runs`).Scan(&runs))
			assert.Zero(events, "polling is not an explicitly opted-in native import")
			assert.Zero(decisions, "polling cannot consume another invocation's import decision")
			assert.Equal(1, runs, "polling owns no nested native import or sync")
		})
	}
}
