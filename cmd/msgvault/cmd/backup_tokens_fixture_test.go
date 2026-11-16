package cmd

import "github.com/spf13/pflag"

// resetBackupRestoreFlags clears values and Changed marks that persist between in-process runs.
func resetBackupRestoreFlags() {
	backupRestoreCmd.Flags().VisitAll(func(flag *pflag.Flag) {
		_ = flag.Value.Set(flag.DefValue)
		flag.Changed = false
	})
}
