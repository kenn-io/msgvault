package cmd

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/line/linetest"
	"go.kenn.io/msgvault/internal/store"
)

func lineE2EFixture() linetest.Fixture {
	const owner = "u0000000000000000000000000000000a"
	const alice = "u0000000000000000000000000000000b"
	return linetest.Fixture{
		Users: []linetest.User{{MID: alice, Name: "Alice"}},
		Chats: []linetest.Chat{
			{MID: owner, Type: 100, Messages: []linetest.Message{{ID: "1", Millis: 1700000000000, Text: "note"}}},
			{MID: alice, Type: 0, Members: []string{alice}, Messages: []linetest.Message{
				{ID: "2", Millis: 1700000001000, Sender: alice, Text: "hello"},
				{ID: "3", Millis: 1700000002000, Text: "hi"},
				{ID: "4", Millis: 1700000003000, Sender: alice, Text: "how are you"},
			}},
		},
	}
}

func TestImportLineEndToEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	markDaemonCLISubprocessForTest(t)
	command, _, err := rootCmd.Find([]string{"import-line"})
	require.NoError(err)
	homeFlag := rootCmd.PersistentFlags().Lookup("home")
	previousHome, previousHomeChanged := homeFlag.Value.String(), homeFlag.Changed
	t.Cleanup(func() {
		command.Flags().VisitAll(func(flag *pflag.Flag) {
			require.NoError(flag.Value.Set(flag.DefValue))
			flag.Changed = false
		})
		require.NoError(homeFlag.Value.Set(previousHome))
		homeFlag.Changed = previousHomeChanged
	})
	tmp := t.TempDir()
	prevOut, prevErr := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr()
	t.Cleanup(func() { rootCmd.SetOut(prevOut); rootCmd.SetErr(prevErr); rootCmd.SetArgs(nil) })
	fixture := linetest.Write(t, t.TempDir(), lineE2EFixture())
	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{"--home", tmp, "import-line", "--limit", "3", fixture})
	require.NoError(rootCmd.ExecuteContext(t.Context()))
	assert.Contains(stdout.String(), "Import paused at --limit.")
	assert.Contains(stdout.String(), "Owner:            u0000000000000000000000000000000a (from the store's Keep Memo chat)")
	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE message_type = 'line'").Scan(&n))
	assert.Equal(3, n)
	var sourceID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM sources WHERE source_type = 'line'").Scan(&sourceID))
	identities, err := st.ListAccountIdentitiesContext(t.Context(), sourceID)
	require.NoError(err)
	assert.NotEmpty(identities, "the owner's default identity is recorded")
}

func TestImportLineRejectsClientFilesOnRemote(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	command := newImportLineCmd()
	command.SetContext(ctx)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"backup"})
	err := command.Execute()
	require.ErrorContains(t, err, "run it on the daemon host with --local")
	assert.Zero(t, requests.Load())
}

func TestImportLineRejectsNegativeLimit(t *testing.T) {
	command := newImportLineCmd()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--limit=-1", "backup"})
	require.ErrorContains(t, command.Execute(), "--limit must be non-negative")
}
