package agentguide

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGuideMatchesPublishedDocs(t *testing.T) {
	docs, err := os.ReadFile("../../docs/guides/agent-quickstart.md")
	require.NoError(t, err)
	assert.Equal(t, terminalGuide(string(docs)), Quickstart(),
		"the embedded offline guide must render the published guide; run make agent-guide")
}

func TestTerminalGuide(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		input := strings.ReplaceAll("---\ntitle: Guide\n---\n\nRead [MCP](../usage/chat.md#write-controls).\n", "\n", newline)
		assert.Equal(t, "# msgvault MCP quickstart\n\nRead [MCP](https://msgvault.io/docs/usage/chat.md#write-controls).\n", terminalGuide(input))
	}
}
