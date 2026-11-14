package cmd

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/spf13/cobra"
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
	for _, flag := range []string{"overwrite", "jobs", "integrity-check", "loose-attachments"} {
		t.Run(flag, func(t *testing.T) {
			prepareBackupTokenRestore(t)
			args := []string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "snapshot", "--repo", repo.Root(), "--target", filepath.Join(t.TempDir(), "target"), "--permissions-only"}
			if flag == "jobs" {
				args = append(args, "--jobs", "1")
			} else {
				args = append(args, "--"+flag)
			}
			rootCmd.SetArgs(args)
			err := executeRootContext(t.Context(), rootCmd)
			require.ErrorContains(t, err, "--"+flag+" cannot be used with --permissions-only")
		})
	}
}

func TestBackupRestoreRejectsIncompatibleFlagsBeforeLiveDaemon(t *testing.T) {
	dataDir := t.TempDir()
	owner, err := tryAcquireDaemonOwnerLock(dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close()) })
	cfg := &config.Config{Data: config.DataConfig{DataDir: dataDir}}
	require.ErrorContains(t, refuseRestoreIntoLiveDaemonHome(dataDir, cfg), "running daemon")
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	prepareBackupTokenRestore(t)
	backupRestoreRepo, backupRestoreTarget, backupRestorePermissionsOnly = repo.Root(), dataDir, true
	cmd := &cobra.Command{Use: "restore"}
	cmd.Flags().Bool("overwrite", false, "")
	require.NoError(t, cmd.Flags().Set("overwrite", "true"))
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	err = runBackupRestore(cmd, []string{"snapshot"})
	require.ErrorContains(t, err, "--overwrite cannot be used with --permissions-only")
	cmd.Flags().Lookup("overwrite").Changed = false
	err = runBackupRestore(cmd, nil)
	require.ErrorContains(t, err, "requires an explicit snapshot ID")
	err = runBackupRestore(cmd, []string{"snapshot"})
	if runtime.GOOS == "windows" {
		require.ErrorContains(t, err, "running daemon")
	} else {
		require.ErrorContains(t, err, "available only on Windows")
		backupRestorePermissionsOnly = false
		err = runBackupRestore(cmd, []string{"snapshot"})
		require.ErrorContains(t, err, "running daemon")
	}
}
