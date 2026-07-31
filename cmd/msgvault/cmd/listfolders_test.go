package cmd

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestListFoldersCmd_NoIMAPAccounts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))

	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")

	// Only Gmail source
	_, err = s.GetOrCreateSource("gmail", "g@example.com")
	require.NoError(err, "create gmail source")
	_ = s.Close()

	savedCfg := cfg
	savedLogger := logger
	defer func() {
		cfg = savedCfg
		logger = savedLogger
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	root := newTestRootCmd()
	root.AddCommand(newListFoldersCmd())
	root.SetArgs([]string{"list-folders"})

	err = root.Execute()
	require.Error(err)
	assert.ErrorContains(err, "no IMAP accounts configured")
}

func TestListFoldersCmd_GmailIdentifier(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))

	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")

	_, err = s.GetOrCreateSource("gmail", "g@example.com")
	require.NoError(err, "create gmail source")
	_ = s.Close()

	savedCfg := cfg
	savedLogger := logger
	defer func() {
		cfg = savedCfg
		logger = savedLogger
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	root := newTestRootCmd()
	root.AddCommand(newListFoldersCmd())
	root.SetArgs([]string{"list-folders", "g@example.com"})

	err = root.Execute()
	require.Error(err)
	assert.ErrorContains(err, "not an IMAP source")
}

func TestListFoldersCmd_IMAPNoCredentials(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))

	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")

	_, err = s.GetOrCreateSource("imap", "i@example.com")
	require.NoError(err, "create imap source")
	_ = s.Close()

	secretsPath := filepath.Join(tmpDir, "client_secret.json")
	require.NoError(os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0600),
		"write client secrets")

	savedCfg := cfg
	savedLogger := logger
	defer func() {
		cfg = savedCfg
		logger = savedLogger
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth:   config.OAuthConfig{ClientSecrets: secretsPath},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Capture stdout
	getOutput := captureStdout(t)

	root := newTestRootCmd()
	root.AddCommand(newListFoldersCmd())
	root.SetArgs([]string{"list-folders", "i@example.com"})

	err = root.Execute()
	// No error expected - it just prints "Credentials not found" and returns nil
	require.NoError(err)

	output := getOutput()
	assert.Contains(output, "no credentials")
}

func TestListFoldersCmd_ListAllPrintsEachSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))

	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")

	_, err = s.GetOrCreateSource("imap", "a@example.com")
	require.NoError(err, "create imap source a")
	_, err = s.GetOrCreateSource("imap", "b@example.com")
	require.NoError(err, "create imap source b")
	_ = s.Close()

	secretsPath := filepath.Join(tmpDir, "client_secret.json")
	require.NoError(os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0600),
		"write client secrets")

	savedCfg := cfg
	savedLogger := logger
	defer func() {
		cfg = savedCfg
		logger = savedLogger
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth:   config.OAuthConfig{ClientSecrets: secretsPath},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Both sources should have "Credentials not found" in stdout
	getOutput := captureStdout(t)

	root := newTestRootCmd()
	root.AddCommand(newListFoldersCmd())
	root.SetArgs([]string{"list-folders"})

	err = root.Execute()
	require.NoError(err)

	output := getOutput()
	assert.Contains(output, "a@example.com")
	assert.Contains(output, "b@example.com")
	assert.Contains(output, "no credentials")
}

func TestListFoldersCmd_BrokenOAuthDoesNotBlockIMAP(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))

	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")

	_, err = s.GetOrCreateSource("imap", "i@example.com")
	require.NoError(err, "create imap source")
	_, err = s.GetOrCreateSource("gmail", "g@example.com")
	require.NoError(err, "create gmail source")
	_ = s.Close()

	// Write a malformed client_secret.json.
	secretsPath := filepath.Join(tmpDir, "client_secret.json")
	require.NoError(os.WriteFile(secretsPath, []byte("not json"), 0600), "write secrets")

	savedCfg := cfg
	savedLogger := logger
	defer func() {
		cfg = savedCfg
		logger = savedLogger
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth:   config.OAuthConfig{ClientSecrets: secretsPath},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	getOutput := captureStdout(t)

	root := newTestRootCmd()
	root.AddCommand(newListFoldersCmd())
	root.SetArgs([]string{"list-folders"})

	err = root.Execute()
	// IMAP sources still get processed even with broken OAuth
	require.NoError(err)

	output := getOutput()
	assert.Contains(output, "i@example.com")
	assert.Contains(output, "no credentials")
}
