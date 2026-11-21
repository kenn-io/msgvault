// Package docs embeds published guides that the CLI and MCP server print.
package docs

import (
	_ "embed"
	"strings"
)

//go:embed guides/agent-quickstart.md
var AgentQuickstart string

// MCPQuickstart formats a published guide for reading without a checkout:
// it drops the front matter and turns relative links into public URLs.
func MCPQuickstart(markdown string) string {
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	if strings.HasPrefix(markdown, "---\n") {
		_, markdown, _ = strings.Cut(markdown[4:], "\n---\n")
	}
	markdown = strings.TrimLeft(markdown, "\n")
	markdown = strings.ReplaceAll(markdown, "](../", "](https://msgvault.io/docs/")
	return "# msgvault MCP quickstart\n\n" + markdown
}
