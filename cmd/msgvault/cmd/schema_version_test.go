package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestSchemaVersionConfigLoading(t *testing.T) {
	for _, database := range []bool{false, true} {
		t.Run(fmt.Sprintf("database=%t", database), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			root := newRootCommand()
			root.SetErr(io.Discard)
			root.AddCommand(newSchemaVersionCommand())
			badConfig := filepath.Join(t.TempDir(), "invalid.toml")
			require.NoError(os.WriteFile(badConfig, []byte("not = [valid"), 0o600))
			args := []string{"--home", t.TempDir(), "--config", badConfig, "schema-version"}
			if database {
				args = append(args, "--database")
			}
			root.SetArgs(args)
			var out bytes.Buffer
			root.SetOut(&out)
			err := executeRootContext(t.Context(), root)
			if database {
				require.ErrorContains(err, "load config: decode config:")
				assert.Empty(out.String())
			} else {
				require.NoError(err, "expected-version probe must bypass config loading")
				assert.Equal(fmt.Sprintf("%d\n", store.SchemaVersion), out.String())
			}
		})
	}
}

func TestSchemaVersionDatabaseDoesNotMigrate(t *testing.T) {
	future := strconv.Itoa(store.SchemaVersion + 1)
	for _, tc := range []struct {
		name    string
		marker  string
		wantOut string
		wantErr string
	}{
		{name: "missing", wantErr: "run 'msgvault migrate' first"},
		{name: "legacy", wantOut: "0\n"},
		{name: "future", marker: future, wantOut: future + "\n"},
		{name: "malformed", marker: "bad-marker", wantErr: `read archive schema version: invalid archive schema version "bad-marker"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "archive.db")
			configPath := filepath.Join(dir, "config.toml")
			require.NoError(os.WriteFile(configPath, fmt.Appendf(nil, "[data]\ndatabase_url = %q\n", dbPath), 0o600))
			if tc.name != "missing" {
				s, err := store.OpenForTest(dbPath)
				require.NoError(err)
				_, err = s.DB().Exec("CREATE TABLE sentinel (value TEXT); INSERT INTO sentinel VALUES ('keep')")
				require.NoError(err)
				if tc.marker != "" {
					_, err = s.DB().Exec("CREATE TABLE archive_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)")
					require.NoError(err)
					_, err = s.DB().Exec("INSERT INTO archive_metadata (key, value) VALUES ('schema_version', ?)", tc.marker)
					require.NoError(err)
				}
				require.NoError(s.Close())
			}
			root := newRootCommand()
			root.AddCommand(newSchemaVersionCommand())
			root.SetArgs([]string{"--home", t.TempDir(), "--config", configPath, "schema-version", "--database"})
			root.SetErr(io.Discard)
			var out bytes.Buffer
			root.SetOut(&out)
			err := executeRootContext(t.Context(), root)
			if tc.wantErr != "" {
				require.ErrorContains(err, tc.wantErr)
			} else {
				require.NoError(err)
			}
			assert.Equal(tc.wantOut, out.String())
			if tc.name == "missing" {
				_, err := os.Stat(dbPath)
				assert.ErrorIs(err, os.ErrNotExist, "probe must not create missing archive")
				return
			}
			s, err := store.OpenReadOnly(dbPath)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(s.Close()) })
			var count int
			require.NoError(s.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE name='messages'").Scan(&count))
			assert.Zero(count, "probe must not migrate the archive")
		})
	}
}

func TestSchemaVersionDatabaseCancellationBeforeOpen(t *testing.T) {
	require := require.New(t)
	conf := lifecycleTestConfig(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := newSchemaVersionCommand()
	cmd.SetContext(testInvocationContext(ctx, conf, invocationOptions{}))
	cmd.SetArgs([]string{"--database"})

	require.ErrorIs(cmd.Execute(), context.Canceled)
	_, err := os.Stat(conf.DatabaseDSN())
	require.ErrorIs(err, os.ErrNotExist, "cancellation before open must not create an archive")
}

func TestArchiveMaintenanceRemoteRefusal(t *testing.T) {
	for _, name := range []string{"schema-version", "migrate"} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			conf := lifecycleTestConfig(t.TempDir())
			conf.Remote.URL = "http://127.0.0.1:1"
			newCommand := newMigrateCommand
			if name == "schema-version" {
				newCommand = newSchemaVersionCommand
				s, err := store.OpenForTest(conf.DatabaseDSN())
				require.NoError(err)
				require.NoError(s.Close())
			}
			for _, useLocal := range []bool{false, true} {
				cmd := commandWithInvocation(t, newCommand(), conf, invocationOptions{useLocal: useLocal})
				if name == "schema-version" {
					cmd.SetArgs([]string{"--database"})
				}
				cmd.SetOut(&bytes.Buffer{})
				if useLocal {
					require.NoError(cmd.Execute())
				} else {
					require.ErrorContains(cmd.Execute(), "local-only")
				}
			}
			if name == "schema-version" {
				cmd := commandWithInvocation(t, newSchemaVersionCommand(), conf, invocationOptions{})
				var out bytes.Buffer
				cmd.SetOut(&out)
				require.NoError(cmd.Execute(), "binary probe is independent of remote")
				assert.Equal(fmt.Sprintf("%d\n", store.SchemaVersion), out.String())
			}
		})
	}
}
