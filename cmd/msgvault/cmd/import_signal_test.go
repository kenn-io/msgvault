package cmd

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/signalfixture"
)

func TestResolveSignalInput(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "database.sqlite")
	require.NoError(os.WriteFile(dbPath, []byte("db"), 0o600))
	otherFiles := t.TempDir()

	gotDB, gotFiles, err := resolveSignalInput(dir, "")
	require.NoError(err)
	assert.Equal(dbPath, gotDB)
	assert.Equal(dir, gotFiles)

	gotDB, gotFiles, err = resolveSignalInput(dbPath, otherFiles)
	require.NoError(err)
	assert.Equal(dbPath, gotDB)
	assert.Equal(otherFiles, gotFiles)

	_, _, err = resolveSignalInput(t.TempDir(), "")
	require.Error(err)
	assert.Contains(err.Error(), "no database.sqlite")

	_, _, err = resolveSignalInput(dbPath, filepath.Join(dir, "missing"))
	require.Error(err)
	assert.Contains(err.Error(), "attachments directory not found")
}

func TestImportSignalConfirmsNormalizedPhone(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	markDaemonCLISubprocessForTest(t)
	home := t.TempDir()
	cfg := config.NewDefaultConfig()
	testCtx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	cfg.HomeDir = home
	cfg.Data.DataDir = home

	command := &cobra.Command{Use: "test"}
	command.SetContext(testCtx)
	command.AddCommand(newImportSignalCmd())
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	command.SetArgs([]string{"import-signal", dir, "--phone", "+1 555 555 0100"})
	require.NoError(command.Execute())

	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err)
	defer func() { require.NoError(st.Close()) }()
	source, err := st.GetOrCreateSource("signal", "+15555550100")
	require.NoError(err)
	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal("+15555550100", identities[0].Address)
}

func TestImportSignalRejectsRemoteLocalFiles(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t, func(daemonCLIRunTestRequest) {}, `{"type":"complete"}`)
	command := newImportSignalCmd()
	command.SetContext(configureRemoteDaemonForTest(t, server.URL))
	command.SetArgs([]string{"backup", "--phone", signalfixture.OwnPhone})
	require.ErrorContains(t, command.Execute(), "--local")
	assert.Zero(t, requests.Load())
}

func TestImportSignalRefreshesCacheAfterPartialFailure(t *testing.T) {
	cfg := lifecycleTestConfig(t.TempDir())
	command := &cobra.Command{Use: "import-signal"}
	command.SetContext(withStoreResolverConfig(t, cfg))
	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(invocationFromCommand(command))
	require.NoError(t, err)
	_, err = st.DB().Exec(`CREATE TRIGGER fail_stats BEFORE UPDATE OF message_count ON conversations BEGIN SELECT RAISE(ABORT, 'stats failure'); END`)
	cleanup()
	require.NoError(t, err)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	err = runSignalImport(command, dir, importSignalFlags{phone: signalfixture.OwnPhone})
	require.ErrorContains(t, err, "stats failure")
	db, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM read_parquet(?, hive_partitioning=true)`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&count))
	assert.Equal(t, 6, count)
}
