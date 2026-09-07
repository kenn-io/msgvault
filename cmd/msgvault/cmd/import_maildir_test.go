package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestImportMaildirCommandArchivesMail(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()
	testCfg := lifecycleTestConfig(dataDir)
	withStoreResolverConfig(t, testCfg)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	root := filepath.Join(dataDir, "mail")
	for _, dir := range []string{"new", "cur", "tmp"} {
		require.NoError(os.MkdirAll(filepath.Join(root, dir), 0700))
	}
	require.NoError(os.WriteFile(filepath.Join(root, "new", "one"), []byte("From: alice@example.com\r\nSubject: Maildir CLI\r\n\r\nbody"), 0600))
	command := newImportMaildirCommand()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{root, "--identifier", "alice@example.com"})
	require.NoError(command.Execute())
	st, err := store.Open(testCfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	var subject, sourceType string
	require.NoError(st.DB().QueryRow(`SELECT m.subject,s.source_type FROM messages m JOIN sources s ON s.id=m.source_id`).Scan(&subject, &sourceType))
	assert.Equal("Maildir CLI", subject)
	assert.Equal("maildir", sourceType)
}
