package cmd

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/config"
)

func TestBackupRestorePermissionsOnlyValidatesInputs(t *testing.T) {
	cfg := &config.Config{HomeDir: t.TempDir()}
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	prepareBackupTokenRestore(t)
	rootCmd.SetArgs([]string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "--repo", repo.Root(), "--target", filepath.Join(t.TempDir(), "target"), "--permissions-only"})
	err = executeRootContext(t.Context(), rootCmd)
	require.ErrorContains(t, err, "requires an explicit snapshot ID")
	rootCmd.SetArgs([]string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "invalid-snapshot", "--repo", repo.Root(), "--target", "", "--permissions-only"})
	err = executeRootContext(t.Context(), rootCmd)
	require.ErrorContains(t, err, "target directory is required")
	if runtime.GOOS != "windows" {
		rootCmd.SetArgs([]string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "invalid-snapshot", "--repo", repo.Root(), "--target", filepath.Join(t.TempDir(), "target"), "--permissions-only"})
		err = executeRootContext(t.Context(), rootCmd)
		require.ErrorContains(t, err, "available only on Windows")
	}
}
