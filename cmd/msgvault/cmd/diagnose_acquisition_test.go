package cmd

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDiagnoseAcquisitionCommand(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	source, err := st.GetOrCreateSource("gmail", "source@example.com")
	require.NoError(t, err)
	run, err := st.StartSync(source.ID, "full")
	require.NoError(t, err)
	for _, item := range []store.SyncRunItem{
		{SyncRunID: run, SourceMessageID: "pending", Phase: "fetch", Status: "error", ErrorKind: "fetch_error"},
		{SyncRunID: run, SourceMessageID: "deleted", Phase: "delete", Status: "error", ErrorKind: "delete_error"},
		{SyncRunID: run, SourceMessageID: "unknown", Phase: "fetch", Status: "error", ErrorKind: "unknown"},
	} {
		require.NoError(t, st.RecordSyncRunItem(item))
	}
	require.NoError(t, st.FailSync(run, "interrupted"))
	require.NoError(t, st.Close())
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf("[data]\ndata_dir = %q\n", dir)), 0o600))
	root := newRootCommand()
	root.AddCommand(newDiagnoseAcquisitionCmd())
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"--home", dir, "diagnose-acquisition", "--source-id", strconv.FormatInt(source.ID, 10), "--json"})
	require.NoError(t, executeRootContext(t.Context(), root))
	var items []acquisitionFailure
	require.NoError(t, json.Unmarshal(output.Bytes(), &items))
	require.Len(t, items, 1)
	assert.Equal(t, "pending", items[0].SourceMessageID)
}

func TestDiagnoseAcquisitionRejectsNewerSchema(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	_, err := st.GetOrCreateSource("gmail", "source@example.com")
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), "UPDATE archive_metadata SET value = ? WHERE key = 'schema_version'", strconv.Itoa(store.SchemaVersion+1))
	require.NoError(t, err)
	require.NoError(t, st.Close())
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf("[data]\ndata_dir = %q\n", dir)), 0o600))
	root := newRootCommand()
	root.AddCommand(newDiagnoseAcquisitionCmd())
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"--home", dir, "diagnose-acquisition", "--source-id", "1", "--json"})
	require.ErrorContains(t, executeRootContext(t.Context(), root), "archive schema version")
	assert.NotContains(t, output.String(), "[]")
}
