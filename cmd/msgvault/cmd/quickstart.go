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
into your agent's context to give it full knowledge of available commands.

Example:
  msgvault quickstart | pbcopy`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Print(quickstartText)
	},
}

func init() {
	rootCmd.AddCommand(quickstartCmd)
}
