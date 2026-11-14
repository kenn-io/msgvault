package cmd

import (
	"io"
	"log/slog"
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
	extras, err := backupExtrasSpec(cfg)
	require.NoError(err)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	_, err = backup.Create(t.Context(), repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachments, DataDir: cfg.Data.DataDir,
		Extras: extras, IncludeTokens: true, AllowPlaintextSecrets: true,
	})
	require.NoError(err)

	return repo
}

func prepareBackupTokenRestore(t *testing.T) {
	t.Helper()
	oldRepo, oldTarget, oldOverwrite := backupRestoreRepo, backupRestoreTarget, backupRestoreOverwrite
	oldForce, oldJobs := backupRestoreForceUnlock, backupRestoreJobs
	oldLoose, oldIntegrity, oldPermissions := backupRestoreLooseAttachments, backupRestoreIntegrityCheck, backupRestorePermissionsOnly
	oldOut, oldErr, oldLogger := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr(), slog.Default()
	changed := make(map[string]bool)
	for _, name := range []string{"repo", "target", "jobs", "overwrite", "permissions-only"} {
		changed[name] = backupRestoreCmd.Flags().Lookup(name).Changed
	}
	t.Cleanup(func() {
		backupRestoreRepo, backupRestoreTarget, backupRestoreOverwrite = oldRepo, oldTarget, oldOverwrite
		backupRestoreForceUnlock, backupRestoreJobs = oldForce, oldJobs
		backupRestoreLooseAttachments, backupRestoreIntegrityCheck, backupRestorePermissionsOnly = oldLoose, oldIntegrity, oldPermissions
		for name, wasChanged := range changed {
			backupRestoreCmd.Flags().Lookup(name).Changed = wasChanged
		}
		rootCmd.SetOut(oldOut)
		rootCmd.SetErr(oldErr)
		rootCmd.SetArgs(nil)
		slog.SetDefault(oldLogger)
	})
}

func runBackupTokenRestore(t *testing.T, cfg *config.Config, repo *backup.Repo, target string, overwrite bool) error {
	t.Helper()
	backupRestoreOverwrite, backupRestoreForceUnlock = false, false
	backupRestoreLooseAttachments, backupRestoreIntegrityCheck, backupRestorePermissionsOnly = false, false, false
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	args := []string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "--repo", repo.Root(), "--target", target, "--jobs", "1"}
	if overwrite {
		args = append(args, "--overwrite")
	}
	rootCmd.SetArgs(args)
	return executeRootContext(t.Context(), rootCmd)
}
