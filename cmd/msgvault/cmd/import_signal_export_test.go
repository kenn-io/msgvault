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

func TestImportSignalExportReportsPartialImport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Data.DataDir = t.TempDir()
	ctx := withStoreResolverConfig(t, cfg)
	root := t.TempDir()
	// Omit files/ so the fixture attachment is reported as missing.
	data, err := os.ReadFile("../../../internal/signalexport/testdata/native-v1/main.jsonl")
	require.NoError(err)
	require.NoError(os.WriteFile(filepath.Join(root, "metadata.json"), []byte(`{"version":1}`), 0600))
	require.NoError(os.WriteFile(filepath.Join(root, "main.jsonl"), append(data, []byte("{broken")...), 0600))
	cmd := &cobra.Command{Use: "import-signal-export"}
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	err = runImportSignalExport(cmd, root, "fictional")
	require.ErrorContains(err, "line 12")
	assert.Contains(out.String(), "3 added")
	assert.Contains(out.String(), "Missing media file not found: 1")
	assert.Contains(out.String(), "Import failed. Re-run to continue. Committed messages are preserved.")
	db, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { _ = db.Close() })
	var n int
	require.NoError(db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?, hive_partitioning=true)`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&n))
	assert.Equal(3, n)
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

func TestImportSignalExportNewerFormat(t *testing.T) {
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Data.DataDir = t.TempDir()
	ctx := withStoreResolverConfig(t, cfg)
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(t, err)
	require.NoError(t, st.InitSchemaContext(t.Context()))
	source, err := st.GetOrCreateSourceContext(t.Context(), "signal_export", "fictional")
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncState(t.Context(), source.ID, `{"signal_export_format":2}`))
	require.NoError(t, st.Close())
	root := "../../../internal/signalexport/testdata/native-v1"
	cmd := &cobra.Command{Use: "import-signal-export"}
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	err = runImportSignalExport(cmd, root, "fictional")
	require.ErrorContains(t, err, "upgrade msgvault")
	assert.NotContains(t, out.String(), "Re-run")
}

func TestSignalExportReasonLabelIndexLimit(t *testing.T) {
	assert.Equal(t, "file scan limit reached", signalExportReasonLabel("index_limit"))
}
