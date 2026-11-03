package cmd

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/docs"
)

//go:embed quickstart.md
var quickstartText string

var quickstartCmd = &cobra.Command{
	Use:   "quickstart",
	Short: "Print a quickstart guide for AI agents",
	Long: `Print a markdown quickstart guide explaining how to use the msgvault CLI.

The output works offline, before any config or archive exists.

Example:
  msgvault quickstart | pbcopy
  msgvault quickstart --mcp`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		mcp, _ := cmd.Flags().GetBool("mcp")
		text := quickstartText
		if mcp {
			text = mcpQuickstart(docs.AgentQuickstart)
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), text)
		return err
	},
}

func init() {
	quickstartCmd.Flags().Bool("mcp", false, "Print the MCP connection and safety guide")
	rootCmd.AddCommand(quickstartCmd)
}

func mcpQuickstart(markdown string) string {
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	if strings.HasPrefix(markdown, "---\n") {
		_, markdown, _ = strings.Cut(markdown[4:], "\n---\n")
	}
	markdown = strings.TrimLeft(markdown, "\n")
	markdown = strings.ReplaceAll(markdown, "](../", "](https://msgvault.io/docs/")
	return "# msgvault MCP quickstart\n\n" + markdown
}
