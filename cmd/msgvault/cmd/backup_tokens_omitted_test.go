package cmd

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBackupRestorePermissionsOnlyRequiresSnapshot(t *testing.T) {
	cfg, dbPath, attachments := backupTokenFixture(t)
	repo := createBackupTokenSnapshot(t, cfg, dbPath, attachments)
	prepareBackupTokenRestore(t)
	rootCmd.SetArgs([]string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "--repo", repo.Root(), "--target", filepath.Join(t.TempDir(), "target"), "--permissions-only"})
	err := executeRootContext(t.Context(), rootCmd)
	require.ErrorContains(t, err, "requires an explicit snapshot ID")
	rootCmd.SetArgs([]string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "invalid-snapshot", "--repo", repo.Root(), "--target", "", "--permissions-only"})
	err = executeRootContext(t.Context(), rootCmd)
	require.ErrorContains(t, err, "target directory is required")
	rootCmd.SetArgs([]string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "invalid-snapshot", "--repo", repo.Root(), "--target", filepath.Join(t.TempDir(), "target"), "--permissions-only"})
	err = executeRootContext(t.Context(), rootCmd)
	if runtime.GOOS == "windows" {
		require.Error(t, err)
	} else {
		require.ErrorContains(t, err, "available only on Windows")
	}
}
