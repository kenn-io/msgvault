//go:build windows

package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func backupTokenFixture(t *testing.T) (*config.Config, string, string) {
	t.Helper()
	require := require.New(t)
	dataDir := t.TempDir()
	cfg := &config.Config{HomeDir: t.TempDir()}
	cfg.Data.DataDir = dataDir
	dbPath := filepath.Join(dataDir, "msgvault.db")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	require.NoError(st.Close())
	attachments := filepath.Join(dataDir, "attachments")
	require.NoError(os.MkdirAll(attachments, 0o700))

	return cfg, dbPath, attachments
}

func createBackupTokenSnapshot(t *testing.T, cfg *config.Config, dbPath, attachments string) *backup.Repo {
	t.Helper()
	require := require.New(t)
	oldInclude, oldConfig, oldPlain := backupCreateIncludeTokens, backupCreateIncludeConfig, backupCreateAllowPlaintextSecrets
	backupCreateIncludeTokens, backupCreateIncludeConfig, backupCreateAllowPlaintextSecrets = true, false, true
	t.Cleanup(func() {
		backupCreateIncludeTokens, backupCreateIncludeConfig, backupCreateAllowPlaintextSecrets = oldInclude, oldConfig, oldPlain
	})
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	captureBackupTokenSnapshot(t, repo, cfg, dbPath, attachments)
	return repo
}

func captureBackupTokenSnapshot(t *testing.T, repo *backup.Repo, cfg *config.Config, dbPath, attachments string) {
	t.Helper()
	extras, err := backupExtrasSpec(cfg)
	require.NoError(t, err)
	_, err = backup.Create(t.Context(), repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachments, DataDir: cfg.Data.DataDir,
		Extras: extras, IncludeTokens: true, AllowPlaintextSecrets: true,
	})
	require.NoError(t, err)
}

func runBackupTokenRestore(t *testing.T, cfg *config.Config, repo *backup.Repo, args ...string) (string, error) {
	t.Helper()
	resetBackupRestoreFlags()
	var output bytes.Buffer
	rootCmd.SetOut(&output)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs(append([]string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "--repo", repo.Root()}, args...))
	err := executeRootContext(t.Context(), rootCmd)
	return output.String(), err
}
