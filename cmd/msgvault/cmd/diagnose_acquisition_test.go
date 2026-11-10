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
	}{{"empty", false}, {"legacy", true}, {"local", true}, {"logging", false}} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			dir := t.TempDir()
			cfg := config.NewDefaultConfig()
			cfg.HomeDir, cfg.Data.DataDir = dir, dir
			var st *store.Store
			if dbURL := os.Getenv("MSGVAULT_TEST_DB"); store.IsPostgresURL(dbURL) && tc.pending {
				st = testutil.NewTestStore(t)
				var database, schema string
				require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT current_database(), current_schema()").Scan(&database, &schema))
				parsed, err := url.Parse(dbURL)
				require.NoError(err)
				parsed.Path = "/" + database
				query := parsed.Query()
				query.Set("search_path", schema)
				parsed.RawQuery = query.Encode()
				cfg.Data.DatabaseURL = parsed.String()
			} else {
				st = testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
			}
			sourceType := "gmail"
			if tc.name == "legacy" {
				sourceType = ""
			}
			source, err := st.GetOrCreateSource(sourceType, "source@example.com")
			require.NoError(err)
			if tc.pending {
				run, err := st.StartSync(source.ID, "full")
				require.NoError(err)
				for _, item := range []store.SyncRunItem{
					{SyncRunID: run, SourceMessageID: "pending", Phase: "fetch", Status: "error", ErrorKind: "fetch_error"},
					{SyncRunID: run, SourceMessageID: "(unknown)", Phase: "fetch", Status: "error", ErrorKind: "batch_fetch_error"},
				} {
					require.NoError(st.RecordSyncRunItem(item))
				}
				require.NoError(st.FailSync(run, "interrupted"))
			}
			require.NoError(st.Close())
			var archiveBefore []byte
			if tc.name == "logging" {
				archiveBefore, err = os.ReadFile(cfg.DatabaseDSN())
				require.NoError(err)
			}
			configText := fmt.Sprintf("[data]\ndata_dir = %q\ndatabase_url = %q\n", dir, cfg.Data.DatabaseURL)
			if tc.name == "local" {
				configText += "[remote]\nurl = 'https://example.com'\n"
			}
			require.NoError(os.WriteFile(filepath.Join(dir, "config.toml"), []byte(configText), 0o600))
			root := newRootCommand()
			root.AddCommand(newDiagnoseAcquisitionCmd())
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			args := []string{"--home", dir, "diagnose-acquisition", "--source-id", strconv.FormatInt(source.ID, 10)}
			if tc.name == "local" {
				args = append(args, "--local")
			}
			if tc.pending {
				args = append(args, "--json")
			}
			logPath := filepath.Join(dir, "diagnostic.log")
			if tc.name == "logging" {
				t.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}) })
				args = append(args, "--log-sql", "--log-file", logPath)
			}
			root.SetArgs(args)
			require.NoError(executeRootContext(t.Context(), root))
			if tc.name == "logging" {
				log, err := os.ReadFile(logPath)
				require.NoError(err)
				assert.Contains(string(log), `"msg":"sql"`)
				archiveAfter, err := os.ReadFile(cfg.DatabaseDSN())
				require.NoError(err)
				assert.Equal(archiveBefore, archiveAfter)
			}
			if tc.pending {
				assert.True(bytes.HasSuffix(output.Bytes(), []byte("\n")))
				var items []map[string]string
				require.NoError(json.Unmarshal(output.Bytes(), &items))
				require.Len(items, 1)
				assert.Equal(map[string]string{"source_message_id": "pending", "phase": "fetch", "error_kind": "fetch_error"}, items[0])
			} else {
				assert.Equal("No pending acquisition failures.\n", output.String())
			}
		})
	}
}

func TestDiagnoseAcquisitionErrors(t *testing.T) {
	for _, tc := range []struct{ name, config, want string }{
		{"invalid", "[data\n", "load config"},
		{"invalid-level", "", "invalid log level"},
		{"remote", "[remote]\nurl = 'https://example.com'\n", "use --local"},
		{"newer-schema", "", "run setup with the matching release"},
		{"merged-source", "", "merged into source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			dir := t.TempDir()
			cfg := config.NewDefaultConfig()
			cfg.HomeDir, cfg.Data.DataDir = dir, dir
			configText := tc.config
			want := tc.want
			sourceID := int64(1)
			if tc.name == "newer-schema" || tc.name == "merged-source" {
				st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
				source, err := st.GetOrCreateSource("gmail", "source@example.com")
				require.NoError(err)
				sourceID = source.ID
				if tc.name == "newer-schema" {
					_, err = st.DB().ExecContext(t.Context(), "UPDATE archive_metadata SET value = ? WHERE key = 'schema_version'", strconv.Itoa(store.SchemaVersion+1))
				} else {
					into, createErr := st.GetOrCreateSource("gmail", "into@example.com")
					require.NoError(createErr)
					_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: source.ID, IntoSourceID: into.ID})
					want = fmt.Sprintf("merged into source %d", into.ID)
				}
				require.NoError(err)
				require.NoError(st.Close())
				configText = fmt.Sprintf("[data]\ndata_dir = %q\n", dir)
			}
			require.NoError(os.WriteFile(filepath.Join(dir, "config.toml"), []byte(configText), 0o600))
			root := newRootCommand()
			root.AddCommand(newDiagnoseAcquisitionCmd())
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			args := []string{"--home", dir, "diagnose-acquisition", "--source-id", strconv.FormatInt(sourceID, 10)}
			if tc.name == "invalid-level" {
				args = append(args, "--log-level", "invalid-level")
			}
			root.SetArgs(args)
			require.ErrorContains(executeRootContext(t.Context(), root), want)
			if tc.name == "invalid" {
				assert.NotContains(output.String(), "Usage:")
			}
		})
	}
}
