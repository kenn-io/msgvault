package cmd

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/url"
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
	var st *store.Store
	if dbURL := os.Getenv("MSGVAULT_TEST_DB"); store.IsPostgresURL(dbURL) {
		st = testutil.NewTestStore(t)
		var database, schema string
		require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT current_database(), current_schema()").Scan(&database, &schema))
		parsed, err := url.Parse(dbURL)
		require.NoError(t, err)
		parsed.Path = "/" + database
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		cfg.Data.DatabaseURL = parsed.String()
	} else {
		st = testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	}
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
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf("[data]\ndata_dir = %q\ndatabase_url = %q\n", dir, cfg.Data.DatabaseURL)), 0o600))
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

func TestDiagnoseAcquisitionEmpty(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	source, err := st.GetOrCreateSource("gmail", "source@example.com")
	require.NoError(t, err)
	require.NoError(t, st.Close())
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf("[data]\ndata_dir = %q\n", dir)), 0o600))
	root := newRootCommand()
	root.AddCommand(newDiagnoseAcquisitionCmd())
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"--home", dir, "diagnose-acquisition", "--source-id", strconv.FormatInt(source.ID, 10)})
	require.NoError(t, executeRootContext(t.Context(), root))
	assert.Equal(t, "No pending acquisition failures.\n", output.String())
}

func TestDiagnoseAcquisitionConfigErrors(t *testing.T) {
	for _, tc := range []struct{ name, config, want string }{
		{"invalid", "[data\n", "load config"},
		{"remote", "[remote]\nurl = 'https://example.com'\n", "remote.url is configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(tc.config), 0o600))
			root := newRootCommand()
			root.AddCommand(newDiagnoseAcquisitionCmd())
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			root.SetArgs([]string{"--home", dir, "diagnose-acquisition", "--source-id", "1"})
			require.ErrorContains(t, executeRootContext(t.Context(), root), tc.want)
			assert.NotContains(t, output.String(), "Usage:")
		})
	}
}

func TestDiagnoseAcquisitionMergedSource(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	from, err := st.GetOrCreateSource("gmail", "from@example.com")
	require.NoError(t, err)
	into, err := st.GetOrCreateSource("gmail", "into@example.com")
	require.NoError(t, err)
	run, err := st.StartSync(from.ID, "full")
	require.NoError(t, err)
	require.NoError(t, st.RecordSyncRunItem(store.SyncRunItem{SyncRunID: run, SourceMessageID: "pending", Phase: "fetch", Status: "error", ErrorKind: "fetch_error"}))
	require.NoError(t, st.FailSync(run, "interrupted"))
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	require.NoError(t, err)
	require.NoError(t, st.Close())
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf("[data]\ndata_dir = %q\n", dir)), 0o600))
	root := newRootCommand()
	root.AddCommand(newDiagnoseAcquisitionCmd())
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"--home", dir, "diagnose-acquisition", "--source-id", strconv.FormatInt(from.ID, 10)})
	require.ErrorContains(t, executeRootContext(t.Context(), root), fmt.Sprintf("merged into source %d", into.ID))
	assert.NotContains(t, output.String(), "pending")
}
