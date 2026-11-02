package cmd

import (
	_ "embed"
	"fmt"

	"github.com/spf13/cobra"
)

//go:embed quickstart.md
var quickstartText string

var quickstartCmd = &cobra.Command{
	Use:   "quickstart",
	Short: "Print a quickstart guide for AI agents",
	Long: `Print a markdown quickstart guide explaining how to use the msgvault CLI.

This is designed for AI agents that do not have MCP access. Pipe the output
into your agent's context for setup, source selection, search, exports, and recovery.

Example:
  msgvault quickstart | pbcopy`,
	Example: "  msgvault quickstart > QUICKSTART.md",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := fmt.Fprint(cmd.OutOrStdout(), quickstartText); err != nil {
			return fmt.Errorf("write quickstart guide: %w", err)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(quickstartCmd)
}
