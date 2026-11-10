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
	for _, tc := range []struct {
		name    string
		pending bool
	}{{"json", true}, {"empty", false}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := config.NewDefaultConfig()
			cfg.HomeDir, cfg.Data.DataDir = dir, dir
			var st *store.Store
			if dbURL := os.Getenv("MSGVAULT_TEST_DB"); store.IsPostgresURL(dbURL) && tc.pending {
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
			if tc.pending {
				run, err := st.StartSync(source.ID, "full")
				require.NoError(t, err)
				for _, item := range []store.SyncRunItem{
					{SyncRunID: run, SourceMessageID: "pending", Phase: "fetch", Status: "error", ErrorKind: "fetch_error"},
					{SyncRunID: run, SourceMessageID: "(unknown)", Phase: "fetch", Status: "error", ErrorKind: "batch_fetch_error"},
				} {
					require.NoError(t, st.RecordSyncRunItem(item))
				}
				require.NoError(t, st.FailSync(run, "interrupted"))
			}
			require.NoError(t, st.Close())
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf("[data]\ndata_dir = %q\ndatabase_url = %q\n", dir, cfg.Data.DatabaseURL)), 0o600))
			root := newRootCommand()
			root.AddCommand(newDiagnoseAcquisitionCmd())
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			args := []string{"--home", dir, "diagnose-acquisition", "--source-id", strconv.FormatInt(source.ID, 10)}
			if tc.pending {
				args = append(args, "--json")
			}
			root.SetArgs(args)
			require.NoError(t, executeRootContext(t.Context(), root))
			if tc.pending {
				var items []acquisitionFailure
				require.NoError(t, json.Unmarshal(output.Bytes(), &items))
				require.Len(t, items, 1)
				assert.Equal(t, "pending", items[0].SourceMessageID)
			} else {
				assert.Equal(t, "No pending acquisition failures.\n", output.String())
			}
		})
	}
}

func TestDiagnoseAcquisitionErrors(t *testing.T) {
	for _, tc := range []struct{ name, config, want string }{
		{"invalid", "[data\n", "load config"},
		{"remote", "[remote]\nurl = 'https://example.com'\n", "remote.url is configured"},
		{"newer-schema", "", "archive schema version"},
		{"merged-source", "", "merged into source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := config.NewDefaultConfig()
			cfg.HomeDir, cfg.Data.DataDir = dir, dir
			configText := tc.config
			if tc.name == "newer-schema" || tc.name == "merged-source" {
				st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
				source, err := st.GetOrCreateSource("gmail", "source@example.com")
				require.NoError(t, err)
				if tc.name == "newer-schema" {
					_, err = st.DB().ExecContext(t.Context(), "UPDATE archive_metadata SET value = ? WHERE key = 'schema_version'", strconv.Itoa(store.SchemaVersion+1))
				} else {
					into, createErr := st.GetOrCreateSource("gmail", "into@example.com")
					require.NoError(t, createErr)
					run, runErr := st.StartSync(source.ID, "full")
					require.NoError(t, runErr)
					require.NoError(t, st.RecordSyncRunItem(store.SyncRunItem{SyncRunID: run, SourceMessageID: "pending", Phase: "fetch", Status: "error", ErrorKind: "fetch_error"}))
					require.NoError(t, st.FailSync(run, "interrupted"))
					_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: source.ID, IntoSourceID: into.ID})
					tc.want = fmt.Sprintf("merged into source %d", into.ID)
				}
				require.NoError(t, err)
				require.NoError(t, st.Close())
				configText = fmt.Sprintf("[data]\ndata_dir = %q\n", dir)
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(configText), 0o600))
			root := newRootCommand()
			root.AddCommand(newDiagnoseAcquisitionCmd())
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			root.SetArgs([]string{"--home", dir, "diagnose-acquisition", "--source-id", "1"})
			require.ErrorContains(t, executeRootContext(t.Context(), root), tc.want)
			if tc.name == "invalid" {
				assert.NotContains(t, output.String(), "Usage:")
			}
			assert.NotContains(t, output.String(), "pending\tfetch")
		})
	}
}
