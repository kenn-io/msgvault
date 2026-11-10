package cmd

import (
	"bytes"
	"database/sql"
	"io"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportGoogleChatRejectsClientFilesOnRemote(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	command := newImportGoogleChatCmd()
	command.SetContext(configureRemoteDaemonForTest(t, server.URL))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--me", "owner@example.com", "part-a", "part-b"})
	require.ErrorContains(t, command.Execute(), "run it on the daemon host with --local")
	assert.Zero(t, requests.Load())
}

func TestImportGoogleChatRequiresOwner(t *testing.T) {
	command := newImportGoogleChatCmd()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"part-a"})
	require.ErrorContains(t, command.Execute(), "--me")
}

func TestImportGoogleChatRefreshesCacheAfterPartialFailure(t *testing.T) {
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Data.DataDir = t.TempDir()
	root := t.TempDir()
	testutil.WriteFile(t, root, "Groups/DM a/messages.json", []byte(`{"messages":[{"message_id":"a/topic/one","creator":{"email":"owner@example.com"},"text":"committed synthetic text","created_date":"Monday, January 18, 2021 at 1:27:33 PM UTC"}]}`))
	testutil.WriteFile(t, root, "Groups/Space b/messages.json", []byte(`{"messages":[`))
	command := &cobra.Command{Use: "import-google-chat"}
	command.SetContext(withStoreResolverConfig(t, cfg))
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(io.Discard)
	err := runImportGoogleChat(command, []string{root}, googleChatCLIOptions{Me: "owner@example.com"})
	require.ErrorContains(t, err, "JSON")
	assert.Contains(t, out.String(), "1 added")
	db, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	var n int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?, hive_partitioning=true)`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&n))
	assert.Equal(t, 1, n)
}
