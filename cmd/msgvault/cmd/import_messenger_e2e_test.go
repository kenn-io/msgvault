package cmd

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func saveMessengerState(t *testing.T) func() {
	t.Helper()
	prevCfg := cfg
	prevLogger := logger
	prevMe := importMessengerMe
	prevFormat := importMessengerFormat
	prevLimit := importMessengerLimit
	prevNoResume := importMessengerNoResume
	prevCheckpoint := importMessengerCheckpointEvery
	prevCfgFile := cfgFile
	prevHomeDir := homeDir
	prevVerbose := verbose
	prevOut := rootCmd.OutOrStdout()
	prevErr := rootCmd.ErrOrStderr()
	return func() {
		cfg = prevCfg
		logger = prevLogger
		importMessengerMe = prevMe
		importMessengerFormat = prevFormat
		importMessengerLimit = prevLimit
		importMessengerNoResume = prevNoResume
		importMessengerCheckpointEvery = prevCheckpoint
		cfgFile = prevCfgFile
		homeDir = prevHomeDir
		verbose = prevVerbose
		rootCmd.SetOut(prevOut)
		rootCmd.SetErr(prevErr)
		rootCmd.SetArgs(nil)
	}
}

func TestImportMessenger_JSON_EndToEnd(t *testing.T) {
	markDaemonCLISubprocessForTest(t)

	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmp := t.TempDir()
	t.Cleanup(saveMessengerState(t))

	fixture, err := filepath.Abs("../../../internal/fbmessenger/testdata/json_simple")
	require.NoError(err)

	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{
		"--home", tmp,
		"import-messenger",
		"--me", "test.user@facebook.messenger",
		fixture,
	})
	require.NoError(rootCmd.ExecuteContext(context.Background()), "import-messenger")
	assert.Contains(stdout.String(), "Import complete", "stdout missing Import complete")

	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	var n int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE message_type='fbmessenger'").Scan(&n))
	assert.Equal(4, n, "messages")
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM participants WHERE email_address='test.user@facebook.messenger'").Scan(&n))
	assert.Equal(1, n, "self participant count")
}

func TestImportMessenger_HTML_EndToEnd(t *testing.T) {
	markDaemonCLISubprocessForTest(t)

	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmp := t.TempDir()
	t.Cleanup(saveMessengerState(t))

	fixture, err := filepath.Abs("../../../internal/fbmessenger/testdata/html_simple")
	require.NoError(err)

	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{
		"--home", tmp,
		"import-messenger",
		"--me", "test.user@facebook.messenger",
		fixture,
	})
	require.NoError(rootCmd.ExecuteContext(context.Background()), "import-messenger")
	assert.Contains(stdout.String(), "Import complete", "stdout missing Import complete")
	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })

	var n int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE message_type='fbmessenger'").Scan(&n))
	assert.Equal(3, n, "messages")
	var rawFormat string
	require.NoError(st.DB().QueryRow("SELECT DISTINCT raw_format FROM message_raw").Scan(&rawFormat))
	assert.Equal("fbmessenger_html", rawFormat, "raw_format")
}

func TestImportMessengerRunsPostSourceMigrationWithoutMessengerIdentity(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmp := t.TempDir()
	t.Cleanup(saveMessengerState(t))
	testCfg := lifecycleTestConfig(tmp)
	testCfg.Identity.Addresses = []string{"legacy@example.com"}
	withStoreResolverConfig(t, testCfg)

	st, err := store.Open(testCfg.DatabaseDSN())
	require.NoError(err, "open seed store")
	require.NoError(st.InitSchema(), "init seed schema")
	emailSource, err := st.GetOrCreateSource("gmail", "mailbox@example.com")
	require.NoError(err, "create eligible email source")
	require.NoError(st.Close(), "close seed store")

	fixture, err := filepath.Abs("../../../internal/fbmessenger/testdata/json_simple")
	require.NoError(err)
	importMessengerMe = "test.user@facebook.messenger"
	importMessengerFormat = "auto"
	importMessengerCheckpointEvery = 200

	var stdout bytes.Buffer
	cmd := &cobra.Command{Use: "import-messenger"}
	cmd.SetContext(context.Background())
	cmd.SetOut(&stdout)
	cmd.SetErr(io.Discard)

	require.NoError(runImportMessenger(cmd, fixture), "import-messenger")
	assert.Contains(stdout.String(), "Import complete", "stdout missing Import complete")

	st, err = store.Open(testCfg.DatabaseDSN())
	require.NoError(err, "open store after import")
	t.Cleanup(func() { _ = st.Close() })

	emailIDs, err := st.ListAccountIdentities(emailSource.ID)
	require.NoError(err, "ListAccountIdentities gmail")
	require.Len(emailIDs, 1, "post-source migration should run for eligible email sources")
	assert.Equal("legacy@example.com", emailIDs[0].Address, "migrated identity address")

	messengerSources, err := st.GetSourcesByIdentifier("test.user@facebook.messenger")
	require.NoError(err, "get messenger source")
	require.Len(messengerSources, 1, "messenger source")
	assert.Equal("facebook_messenger", messengerSources[0].SourceType, "messenger source type")
	messengerIDs, err := st.ListAccountIdentities(messengerSources[0].ID)
	require.NoError(err, "ListAccountIdentities messenger")
	assert.Empty(messengerIDs, "legacy email identities must not be written to the Messenger source")
}

func TestImportMessenger_MissingDir(t *testing.T) {
	markDaemonCLISubprocessForTest(t)

	tmp := t.TempDir()
	t.Cleanup(saveMessengerState(t))

	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{
		"--home", tmp,
		"import-messenger",
		"--me", "test.user@facebook.messenger",
		filepath.Join(tmp, "does", "not", "exist"),
	})
	err := rootCmd.ExecuteContext(context.Background())
	requirepkg.Error(t, err, "expected error for missing dir")
	msg := err.Error()
	assertpkg.True(t, strings.Contains(msg, "not found") || strings.Contains(msg, "no such"),
		"error should describe missing path, got %v", err)
}
