package cmd

import (
	"encoding/json/v2"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/mcpdiscovery"
)

func newMCPStatusCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:               "status",
		Short:             "List running HTTP MCP listeners",
		Args:              cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(command *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgFile, homeDir)
			if err != nil {
				return err
			}
			directory := filepath.Join(cfg.HomeDir, "mcp")
			endpoints, err := mcpdiscovery.List(directory)
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.MarshalWrite(command.OutOrStdout(), endpoints)
			}
			if len(endpoints) == 0 {
				_, err := fmt.Fprintln(command.OutOrStdout(), "No HTTP MCP listeners are running.")
				if err != nil {
					return fmt.Errorf("write MCP status: %w", err)
				}
				return nil
			}
			for _, endpoint := range endpoints {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "MCP %s (pid %d)\n", endpoint.URL, endpoint.PID); err != nil {
					return fmt.Errorf("write MCP status: %w", err)
				}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print listener discovery as JSON")
	return command
}
