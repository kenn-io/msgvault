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

func TestBackupRestorePermissionsOnlyRejectsRestoreFlags(t *testing.T) {
	for _, flag := range []string{"overwrite", "jobs", "integrity-check", "loose-attachments"} {
		t.Run(flag, func(t *testing.T) {
			cfg := &config.Config{HomeDir: t.TempDir()}
			repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
			require.NoError(t, err)
			prepareBackupTokenRestore(t)
			args := []string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "snapshot", "--repo", repo.Root(), "--target", filepath.Join(t.TempDir(), "target"), "--permissions-only"}
			if flag == "jobs" {
				args = append(args, "--jobs", "1")
			} else {
				args = append(args, "--"+flag)
			}
			rootCmd.SetArgs(args)
			err = executeRootContext(t.Context(), rootCmd)
			require.ErrorContains(t, err, "--"+flag+" cannot be used with --permissions-only")
		})
	}
}
