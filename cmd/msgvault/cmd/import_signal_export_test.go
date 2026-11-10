package cmd

import (
	"bytes"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestImportSignalExportRefreshesCacheAndReportsPartialFailure(t *testing.T) {
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Data.DataDir = t.TempDir()
	ctx := withStoreResolverConfig(t, cfg)
	root := t.TempDir()
	data, err := os.ReadFile("../../../internal/signalexport/testdata/native-v1/main.jsonl")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "metadata.json"), []byte(`{"version":1}`), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.jsonl"), append(data, []byte("{broken")...), 0600))
	cmd := &cobra.Command{Use: "import-signal-export"}
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	err = runImportSignalExport(cmd, root, "fictional")
	require.ErrorContains(t, err, "line 12")
	assert.Contains(t, out.String(), "3 added")
	assert.Contains(t, out.String(), "Records: 0 empty, 0 unrecognized")
	assert.Contains(t, out.String(), "Missing media file not found: 1")
	assert.Contains(t, out.String(), "Import failed. Re-run to continue; committed messages are preserved.")
	db, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	var n int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?, hive_partitioning=true)`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&n))
	assert.Equal(t, 3, n)
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?)`, filepath.Join(cfg.AnalyticsDir(), "owner_participants", "*.parquet")).Scan(&n))
	assert.Equal(t, 1, n)
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	var address string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT address FROM account_identities").Scan(&address))
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM account_identities").Scan(&n))
	assert.Equal(t, 1, n)
	const scopedSelf = "v1:a524644266885e16a3bb16166ae38bdbfb36af08fc51036f4dbb91423c7cbcc1:self"
	assert.Equal(t, scopedSelf, address)
}
func TestImportSignalExportRefusesRemoteClientFiles(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	command := newImportSignalExportCmd()
	command.SetContext(ctx)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--account", "fictional", "export"})
	require.ErrorContains(t, command.Execute(), "run it on the daemon host with --local")
	assert.Zero(t, requests.Load())
}
func TestImportSignalExportRequiresAccount(t *testing.T) {
	command := newImportSignalExportCmd()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"export"})
	require.ErrorContains(t, command.Execute(), "--account")
}
