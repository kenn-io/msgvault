package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

func newMigrateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply this binary's archive schema migrations offline",
		Long: `Apply the main archive schema migrations without starting a daemon.
Stop the daemon and all other archive users first, and back up the archive.
This command is local-only; run it on the archive host or use --local to select
this machine's archive. It can initialize a fresh archive and is safe to repeat.
On failure, retry with this binary or restore the backup before using an older
binary. Optional vector and analytics cache upgrades run in their own subsystems.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return runMigrate(cmd) },
	}
}

func runMigrate(cmd *cobra.Command) (runErr error) {
	inv := invocationFromCommand(cmd)
	currentCfg, _ := invocationConfigLogger(inv)
	if currentCfg == nil {
		return errors.New("configuration is unavailable")
	}
	if IsRemoteMode(inv) {
		return errors.New("migrate is local-only; run it on the archive host or pass --local")
	}
	daemonLock, err := tryAcquireDaemonOwnerLock(currentCfg.Data.DataDir)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer func() { runErr = errors.Join(runErr, daemonLock.Close()) }()
	if !store.IsPostgresURL(currentCfg.DatabaseDSN()) {
		// Always claim ownership: even a daemon-child environment must not bypass
		// the offline command's exclusion contract.
		writer, err := tryAcquireWriteOwnerLock(currentCfg.Data.DataDir)
		if err != nil {
			if errors.As(err, &writeOwnerLockHeldError{}) {
				return archiveOwnedError(currentCfg.Data.DataDir)
			}
			return fmt.Errorf("migrate: %w", err)
		}
		defer func() { runErr = errors.Join(runErr, writer.Close()) }()
	}
	s, err := store.OpenContext(cmd.Context(), currentCfg.DatabaseDSN())
	if err != nil {
		return fmt.Errorf("open archive for migration: %w", err)
	}
	defer func() { runErr = errors.Join(runErr, s.Close()) }()
	if err := s.InitSchemaContext(cmd.Context()); err != nil {
		return fmt.Errorf("migrate archive schema: %w", err)
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Schema version: %d\n", store.SchemaVersion); err != nil {
		return fmt.Errorf("write migration result: %w", err)
	}
	return nil
}

func init() { rootCmd.AddCommand(newMigrateCommand()) }
