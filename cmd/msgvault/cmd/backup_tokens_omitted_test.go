package cmd

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/config"
)

func TestBackupRestorePermissionsOnlyValidatesInputsBeforeLiveDaemon(t *testing.T) {
	require := require.New(t)
	dataDir := t.TempDir()
	owner, err := tryAcquireDaemonOwnerLock(dataDir)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(owner.Close()) })
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	prepareBackupTokenRestore(t)
	cmd := &cobra.Command{Use: "restore"}
	cmd.Flags().AddFlagSet(backupRestoreCmd.Flags())
	cmd.SetContext(testInvocationContext(t.Context(), &config.Config{Data: config.DataConfig{DataDir: dataDir}}, invocationOptions{}))
	run := func(args ...string) error {
		resetBackupRestoreFlags()
		require.NoError(cmd.ParseFlags(append([]string{"--repo", repo.Root(), "--target", dataDir}, args...)))
		return runBackupRestore(cmd, cmd.Flags().Args())
	}
	require.ErrorContains(run("--permissions-only"), "requires an explicit snapshot ID")
	require.ErrorContains(run("snapshot", "--permissions-only", "--target", ""), "target directory is required")
	for _, flag := range []string{"--overwrite", "--jobs=1", "--integrity-check", "--loose-attachments"} {
		name, _, _ := strings.Cut(flag, "=")
		require.ErrorContains(run("snapshot", "--permissions-only", flag), name+" cannot be used with --permissions-only")
	}
	if runtime.GOOS == "windows" {
		require.ErrorContains(run("snapshot", "--permissions-only"), "running daemon")
	} else {
		require.ErrorContains(run("snapshot", "--permissions-only"), "available only on Windows")
	}
	require.ErrorContains(run("snapshot"), "running daemon")
}
