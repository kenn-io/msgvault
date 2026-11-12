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
)

func TestImportInstagramNameAdvice(t *testing.T) {
	for _, tc := range []struct {
		name, selfName, records string
		limit                   int
		badShard, wantWarning   bool
	}{
		{"complete without match", "Unmatched", `{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"one"}`, 0, false, true},
		{"limit before self", "Nova Example", `{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"one"},{"sender_name":"Nova Example","timestamp_ms":1730500000124,"content":"two"}`, 1, false, false},
		{"incomplete records", "Nova Example", `{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"one"},{"sender_name":"Nova Example","content":"missing timestamp"}`, 0, false, false},
		{"import error", "Nova Example", `{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"one"}`, 0, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			root := t.TempDir()
			dir := filepath.Join(root, "your_instagram_activity", "messages", "inbox", "circle")
			require.NoError(os.MkdirAll(dir, 0700))
			data := `{"thread_path":"inbox/circle","participants":[{"name":"Mira Example"},{"name":"Nova Example"}],"messages":[` + tc.records + `]}`
			require.NoError(os.WriteFile(filepath.Join(dir, "message_1.json"), []byte(data), 0600))
			if tc.badShard {
				require.NoError(os.WriteFile(filepath.Join(dir, "message_2.json"), []byte(`{"thread_path":"inbox/other","messages":[]}`), 0600))
			}
			command := &cobra.Command{Use: "import-instagram"}
			command.SetContext(withStoreResolverConfig(t, lifecycleTestConfig(t.TempDir())))
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(io.Discard)
			err := runImportInstagram(command, root, instagramCLIOptions{Account: "example", SelfName: tc.selfName, Limit: tc.limit})
			if tc.badShard {
				require.Error(err)
			} else {
				require.NoError(err)
			}
			if tc.wantWarning {
				assert.Contains(output.String(), "No sender exactly matched --self-name")
			} else {
				assert.NotContains(output.String(), "No sender exactly matched --self-name")
			}
			if tc.limit > 0 {
				assert.Contains(output.String(), "Import paused at --limit")
			}
		})
	}
}

func TestImportInstagramRejectsClientFilesOnRemote(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	t.Cleanup(saveMessengerState(t))
	command := importMessengerCmd
	previousContext := command.Context()
	t.Cleanup(func() { command.SetContext(previousContext) })
	command.SetContext(ctx)
	importMessengerSource, importMessengerAccount, importMessengerSelfName = "instagram", "example", "Nova Example"
	require.ErrorContains(command.RunE(command, []string{"/fictional/export"}), "run it on the daemon host with --local")
	assert.Zero(requests.Load())
}

func TestImportInstagramRefreshesCache(t *testing.T) {
	for _, scenario := range []string{"partial failure", "unchanged replay"} {
		t.Run(scenario, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			cfg := lifecycleTestConfig(t.TempDir())
			root := t.TempDir()
			dir := filepath.Join(root, "your_instagram_activity", "messages", "inbox", "a")
			require.NoError(os.MkdirAll(dir, 0700))
			require.NoError(os.WriteFile(filepath.Join(dir, "message_1.json"), []byte(`{"thread_path":"inbox/a","messages":[{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"retained"}]}`), 0600))
			command := &cobra.Command{Use: "import-instagram"}
			command.SetContext(withStoreResolverConfig(t, cfg))
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			opts := instagramCLIOptions{Account: "example", SelfName: "Nova Example"}
			if scenario == "partial failure" {
				bad := filepath.Join(root, "your_instagram_activity", "messages", "inbox", "fictional")
				require.NoError(os.MkdirAll(bad, 0700))
				require.NoError(os.WriteFile(filepath.Join(bad, "message_1.json"), []byte(`{"thread_path":"inbox/fictional","messages":[{"timestamp_ms":1730500000123,"content":"first conflicting shard"}]}`), 0600))
				require.NoError(os.WriteFile(filepath.Join(bad, "message_2.json"), []byte(`{"thread_path":"inbox/other","messages":[{"timestamp_ms":1730500001123,"content":"conflicting key"}]}`), 0600))
				require.ErrorContains(runImportInstagram(command, root, opts), "conversation paths conflict between message files")
			} else {
				require.NoError(os.MkdirAll(filepath.Dir(cfg.AnalyticsDir()), 0700))
				require.NoError(os.WriteFile(cfg.AnalyticsDir(), []byte("obstruction"), 0600))
				require.Error(runImportInstagram(command, root, opts))
				require.NoError(os.Remove(cfg.AnalyticsDir()))
				require.NoError(runImportInstagram(command, root, opts))
			}
			db, err := sql.Open("duckdb", "")
			require.NoError(err)
			t.Cleanup(func() { require.NoError(db.Close()) })
			var n int
			require.NoError(db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?,hive_partitioning=true)`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&n))
			assert.Equal(1, n)
		})
	}
}

func TestImportMessengerInstagramRejectsUnsupportedFormat(t *testing.T) {
	for _, format := range []string{"both", "typo"} {
		t.Run(format, func(t *testing.T) {
			markDaemonCLISubprocessForTest(t)
			t.Cleanup(saveMessengerState(t))
			cmd := importMessengerCmd
			previousContext := cmd.Context()
			t.Cleanup(func() { cmd.SetContext(previousContext) })
			cmd.SetContext(withStoreResolverConfig(t, lifecycleTestConfig(t.TempDir())))
			flag := cmd.Flags().Lookup("format")
			previousChanged := flag.Changed
			t.Cleanup(func() { flag.Changed = previousChanged })
			require.NoError(t, cmd.Flags().Set("format", format))
			importMessengerSource, importMessengerAccount, importMessengerSelfName = "instagram", "example", "Nova Example"
			fixture := filepath.Join("..", "..", "..", "internal", "fbmessenger", "testdata", "instagram")
			require.ErrorContains(t, cmd.RunE(cmd, []string{fixture}), "valid: auto, json")
		})
	}
}
