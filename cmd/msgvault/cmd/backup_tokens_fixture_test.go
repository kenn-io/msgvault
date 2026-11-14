package cmd

import (
	"log/slog"
	"testing"

	"github.com/spf13/pflag"
)

func prepareBackupTokenRestore(t *testing.T) {
	t.Helper()
	oldOut, oldErr, oldLogger := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr(), slog.Default()
	resetBackupRestoreFlags()
	t.Cleanup(func() {
		resetBackupRestoreFlags()
		rootCmd.SetOut(oldOut)
		rootCmd.SetErr(oldErr)
		rootCmd.SetArgs(nil)
		slog.SetDefault(oldLogger)
	})
}

// resetBackupRestoreFlags clears values and Changed marks that persist between in-process runs.
func resetBackupRestoreFlags() {
	backupRestoreCmd.Flags().VisitAll(func(flag *pflag.Flag) {
		_ = flag.Value.Set(flag.DefValue)
		flag.Changed = false
	})
}
