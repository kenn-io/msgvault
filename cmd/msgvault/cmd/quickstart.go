package cmd

import (
	_ "embed"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/docs"
)

//go:embed quickstart.md
var quickstartText string

var quickstartCmd = &cobra.Command{
	Use:   "quickstart",
	Short: "Print a quickstart guide for AI agents",
	Long: `Print a markdown quickstart guide explaining how to use the msgvault CLI.

Both guides work offline, before any config or archive exists.

Example:
  msgvault quickstart | pbcopy
  msgvault quickstart --mcp`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		mcp, _ := cmd.Flags().GetBool("mcp")
		text := quickstartText
		if mcp {
			text = docs.MCPQuickstart(docs.AgentQuickstart)
		}
		if _, err := fmt.Fprint(cmd.OutOrStdout(), text); err != nil {
			return fmt.Errorf("write quickstart guide: %w", err)
		}
		return nil
	},
}

func init() {
	quickstartCmd.Flags().Bool("mcp", false, "Print the MCP connection and safety guide")
	rootCmd.AddCommand(quickstartCmd)
}
