package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

func newSchemaVersionCommand() *cobra.Command {
	var database bool
	cmd := &cobra.Command{
		Use:   "schema-version",
		Short: "Print the binary's expected archive schema version",
		Long: `Print the expected main archive schema version as an integer.
With --database, read the configured local archive's version without migrating
or starting a daemon. A missing archive is an error. Remote archives must be
probed on their host; --local intentionally selects this machine's archive.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			version := store.SchemaVersion
			if database {
				inv := invocationFromCommand(cmd)
				if IsRemoteMode(inv) {
					return errors.New("schema-version --database is local-only; run it on the archive host or pass --local")
				}
				currentCfg, _ := invocationConfigLogger(inv)
				if currentCfg == nil {
					return errors.New("configuration is unavailable")
				}
				s, err := store.OpenReadOnlyContext(cmd.Context(), currentCfg.DatabaseDSN())
				if err != nil {
					return fmt.Errorf("open archive for schema probe: %w", err)
				}
				defer func() {
					if err := s.Close(); err != nil {
						runErr = errors.Join(runErr, err)
					}
				}()
				version, err = s.SchemaVersionContext(cmd.Context())
				if err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), version); err != nil {
				return fmt.Errorf("print schema version: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&database, "database", false, "Read the configured local archive version without migrations")
	return cmd
}

func init() { rootCmd.AddCommand(newSchemaVersionCommand()) }
