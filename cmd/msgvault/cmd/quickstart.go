package cmd

import (
	_ "embed"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/agentguide"
)

//go:embed quickstart.md
var quickstartText string

var quickstartCmd = &cobra.Command{
	Use:   "quickstart",
	Short: "Print a quickstart guide for AI agents",
	Long: `Print a markdown quickstart guide explaining how to use the msgvault CLI.

Use --mcp for the MCP connection, reading, drafting, and approval guide.
Both guides work offline without configuration or a running daemon.`,
	Example: `  msgvault quickstart | pbcopy
  msgvault quickstart --mcp`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		mcp, err := cmd.Flags().GetBool("mcp")
		if err != nil {
			return fmt.Errorf("read MCP guide option: %w", err)
		}
		text := quickstartText
		if mcp {
			text = agentguide.Quickstart()
		}
		_, err = fmt.Fprint(cmd.OutOrStdout(), text)
		if err != nil {
			return fmt.Errorf("print quickstart guide: %w", err)
		}
		return nil
	},
}

func init() {
	quickstartCmd.Flags().Bool("mcp", false, "Print the MCP connection and safety guide")
	rootCmd.AddCommand(quickstartCmd)
}
