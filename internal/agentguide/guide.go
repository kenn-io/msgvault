// Package agentguide provides the offline MCP guide without opening an archive.
package agentguide

import (
	_ "embed"
	"strings"
)

//go:embed quickstart.md
var source string

// Quickstart returns the public guide formatted for a terminal or MCP client.
func Quickstart() string {
	return terminalGuide(source)
}

func terminalGuide(markdown string) string {
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	if strings.HasPrefix(markdown, "---\n") {
		if _, body, ok := strings.Cut(markdown[4:], "\n---\n"); ok {
			markdown = strings.TrimLeft(body, "\n")
		}
	}
	markdown = strings.ReplaceAll(markdown, "](../", "](https://msgvault.io/docs/")
	return "# msgvault MCP quickstart\n\n" + markdown
}
