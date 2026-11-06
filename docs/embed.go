// Package docs embeds published guides that the CLI prints.
package docs

import (
	_ "embed"
	"strings"
)

//go:embed guides/agent-quickstart.md
var AgentQuickstart string

// MCPQuickstart formats a published guide for standalone CLI and MCP reading.
// Relative documentation links become public URLs so no checkout is required.
func MCPQuickstart(markdown string) string {
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	if strings.HasPrefix(markdown, "---\n") {
		_, markdown, _ = strings.Cut(markdown[4:], "\n---\n")
	}
	markdown = strings.TrimLeft(markdown, "\n")
	markdown = strings.ReplaceAll(markdown, "](../", "](https://msgvault.io/docs/")
	return "# msgvault MCP quickstart\n\n" + markdown
}
