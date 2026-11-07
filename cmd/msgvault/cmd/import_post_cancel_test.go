package cmd

import (
	"context"
	"encoding/csv"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/imazingcsv"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// cancelWhenSyncCompletes cancels ctx inside the commit that records the
// import's sync run as completed. The import has succeeded at that point, so
// the command's post-import steps must still finish.
func cancelWhenSyncCompletes(ctx context.Context, t *testing.T, dsn string) (context.Context, func() bool) {
	t.Helper()
	probe, err := store.Open(dsn)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, probe.Close()) })
	queryCtx := context.WithoutCancel(t.Context())
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var count int
		require.NoError(t, probe.DB().QueryRowContext(queryCtx,
			`SELECT COUNT(*) FROM sync_runs WHERE status = 'completed'`).Scan(&count))
		return count > 0
	})
	return runCtx, canceled
}

// Not parallel: it replaces the process-wide slog default and SQL log options.
func TestImportWhatsAppCancelAtSyncCompletionRecordsDefaultIdentity(t *testing.T) {
	require := require.New(t)
	home := t.TempDir()
	fixture := createWhatsAppIdentityFixture(t, false, false)
	var canceled func() bool
	_, err := runWhatsAppIdentityCommandContext(t, func(ctx context.Context) context.Context {
		var runCtx context.Context
		runCtx, canceled = cancelWhenSyncCompletes(ctx, t, filepath.Join(home, "msgvault.db"))
		return runCtx
	}, home, fixture, false)
	require.True(canceled(), "the cancel must land as the sync completes")
	require.NoError(err)
	identities := whatsappIdentityTestIdentities(t, home)
	require.Len(identities, 1, "the owner identity must be recorded")
	assert.Equal(t, whatsappIdentityTestPhone, identities[0].Address)
}

// Not parallel: it replaces process-wide flags, the slog default, and SQL
// log options.
func TestImportMessengerCancelAtSyncCompletionFinishesMigrations(t *testing.T) {
	require := require.New(t)
	markDaemonCLISubprocessForTest(t)
	t.Cleanup(saveMessengerState(t))
	testCfg := lifecycleTestConfig(t.TempDir())
	testCtx := withStoreResolverConfig(t, testCfg)
	fixture, err := filepath.Abs("../../../internal/fbmessenger/testdata/json_simple")
	require.NoError(err)
	importMessengerMe = "test.user@facebook.messenger"
	importMessengerFormat = "auto"
	importMessengerLimit = 0
	importMessengerNoResume = false

	ctx, canceled := cancelWhenSyncCompletes(testCtx, t, testCfg.DatabaseDSN())
	cmd := &cobra.Command{Use: "import-messenger"}
	cmd.SetContext(ctx)
	runErr := runImportMessenger(cmd, fixture)
	require.True(canceled(), "the cancel must land as the sync completes")
	require.NoError(runErr)
}

// Not parallel: it replaces the process-wide slog default and SQL log options.
func TestImportIMazingCSVCancelAtSyncCompletionFinishesMigrations(t *testing.T) {
	require := require.New(t)
	testCfg := lifecycleTestConfig(t.TempDir())
	testCtx := withStoreResolverConfig(t, testCfg)
	exportDir := t.TempDir()
	csvDir := filepath.Join(exportDir, "csv")
	require.NoError(os.Mkdir(csvDir, 0o700))
	file, err := os.Create(filepath.Join(csvDir, "messages.csv"))
	require.NoError(err)
	writer := csv.NewWriter(file)
	require.NoError(writer.WriteAll([][]string{
		{"Chat Session", "Message Date", "Delivered Date", "Read Date", "Service", "Type",
			"Sender ID", "Sender Name", "Status", "Replying to", "Subject", "Text", "Attachment", "Attachment type"},
		{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "first message", "", ""},
	}))
	require.NoError(file.Close())

	ctx, canceled := cancelWhenSyncCompletes(testCtx, t, testCfg.DatabaseDSN())
	cmd := &cobra.Command{Use: "import-imazing-csv"}
	cmd.SetContext(ctx)
	runErr := runImportIMazingCSV(cmd, exportDir, imazingcsv.Options{Owner: "+15550000001", Timezone: "UTC"})
	require.True(canceled(), "the cancel must land as the sync completes")
	require.NoError(runErr)
}
