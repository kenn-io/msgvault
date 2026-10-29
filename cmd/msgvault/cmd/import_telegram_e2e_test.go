package cmd

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestImportTelegramEndToEnd(t *testing.T) {
	markDaemonCLISubprocessForTest(t)

	require := require.New(t)
	assert := assert.New(t)
	tmp := t.TempDir()

	fixture, err := filepath.Abs("../../../internal/telegram/testdata/full")
	require.NoError(err)

	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{
		"--home", tmp,
		"import-telegram",
		fixture,
	})
	require.NoError(rootCmd.ExecuteContext(context.Background()), "import-telegram")
	assert.Contains(stdout.String(), "Import complete", "stdout missing Import complete")

	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	var messages int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE message_type = 'telegram'`).Scan(&messages))
	assert.Equal(11, messages, "telegram messages")

	var sourceType string
	require.NoError(st.DB().QueryRow(
		`SELECT source_type FROM sources WHERE identifier = 'user7000000000000123456'`).Scan(&sourceType))
	assert.Equal("telegram", sourceType, "source")
}

func TestImportTelegramSingleChatWithMeOverride(t *testing.T) {
	markDaemonCLISubprocessForTest(t)

	require := require.New(t)
	assert := assert.New(t)
	tmp := t.TempDir()

	fixture, err := filepath.Abs("../../../internal/telegram/testdata/single")
	require.NoError(err)

	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{
		"--home", tmp,
		"import-telegram",
		"--me", "7000000000000888888",
		fixture,
	})
	require.NoError(rootCmd.ExecuteContext(context.Background()), "import-telegram")
	assert.Contains(stdout.String(), "Import complete", "stdout missing Import complete")

	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	var fromMe int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE message_type = 'telegram' AND is_from_me = 1`).Scan(&fromMe))
	assert.Equal(2, fromMe, "--me override marks Carol's messages")
}

func TestImportTelegramMissingDir(t *testing.T) {
	markDaemonCLISubprocessForTest(t)

	tmp := t.TempDir()
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{
		"--home", tmp,
		"import-telegram",
		filepath.Join(tmp, "does", "not", "exist"),
	})
	err := rootCmd.ExecuteContext(context.Background())
	require.Error(t, err, "expected error for missing dir")
	assert.Contains(t, err.Error(), "not found")
}
