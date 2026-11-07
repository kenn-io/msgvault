package cmd

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/docs"
	"go.kenn.io/msgvault/internal/agentguide"
)

//go:embed quickstart.md
var quickstartText string

var quickstartCmd = &cobra.Command{
	Use:   "quickstart",
	Short: "Print a quickstart guide for AI agents",
	Long: `Print a markdown quickstart guide explaining how to use the msgvault CLI.

All guides work offline, before any config or archive exists.

Example:
  msgvault quickstart | pbcopy
  msgvault quickstart --mcp
  msgvault quickstart --mcp-only`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		mcp, _ := cmd.Flags().GetBool("mcp")
		mcpOnly, _ := cmd.Flags().GetBool("mcp-only")
		text := quickstartText
		if mcp {
			text = mcpQuickstart(docs.AgentQuickstart)
		}
		if mcpOnly {
			text = agentguide.QuickstartMarkdown()
		}
		if _, err := fmt.Fprint(cmd.OutOrStdout(), text); err != nil {
			return fmt.Errorf("write quickstart guide: %w", err)
		}
		return nil
	},
}

func init() {
	quickstartCmd.Flags().Bool("mcp", false, "Print the MCP connection and safety guide")
	quickstartCmd.Flags().Bool("mcp-only", false, "Print native tool workflows for an already connected MCP assistant")
	quickstartCmd.MarkFlagsMutuallyExclusive("mcp", "mcp-only")
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
