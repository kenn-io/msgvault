package mcp

import (
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

var updateGuideExamples = flag.Bool("update-guide-examples", false, "Update synthetic MCP guide response fixtures")

func TestQuickstartMessageExample(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	message := &query.MessageDetail{
		ID: 9001, SourceMessageID: "example-message-9001", ConversationID: 41,
		SourceConversationID: "example-thread-41", Subject: "Project update", MessageType: "email",
		SentAt:   time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		From:     []query.Address{{Name: "Alice Example", Email: "alice@example.com"}},
		To:       []query.Address{{Name: "Bob Example", Email: "bob@example.com"}},
		BodyText: "Hello, Alice. Here is the project update.",
	}
	session := task5ConnectClient(t, ServeOptions{Engine: &querytest.MockEngine{Messages: map[int64]*query.MessageDetail{message.ID: message}}}, false)
	arguments := map[string]any{"id": message.ID, "max_chars": 13}
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessage, Arguments: arguments})
	requirements.NoError(err)
	requirements.False(result.IsError)
	response, ok := result.StructuredContent.(map[string]any)
	requirements.True(ok)
	assertions.Equal("Hello, Alice.", response["body_text"])
	assertions.Equal(true, response["has_more"])
	fixture := map[string]any{"tool": ToolGetMessage, "arguments": arguments, "response": response}
	encoded, err := json.MarshalIndent(fixture, "", "  ")
	requirements.NoError(err)
	encoded = append(encoded, '\n')
	const path = "../agentguide/testdata/get_message.json"
	if *updateGuideExamples {
		requirements.NoError(os.MkdirAll("../agentguide/testdata", 0o755))
		requirements.NoError(os.WriteFile(path, encoded, 0o644))
	}
	golden, err := os.ReadFile(path)
	requirements.NoError(err, "regenerate with -update-guide-examples")
	assertions.JSONEq(string(golden), string(encoded))
	const guidePath = "../../docs/guides/agent-quickstart.md"
	guide, err := os.ReadFile(guidePath)
	requirements.NoError(err)
	const begin = "<!-- BEGIN GENERATED example get_message -->"
	const end = "<!-- END GENERATED example get_message -->"
	prefix, rest, found := strings.Cut(string(guide), begin)
	requirements.True(found)
	_, suffix, found := strings.Cut(rest, end)
	requirements.True(found)
	example := "\n```json\n" + string(encoded) + "```\n"
	if *updateGuideExamples {
		guide = []byte(prefix + begin + example + end + suffix)
		requirements.NoError(os.WriteFile(guidePath, guide, 0o644))
	}
	assertions.Contains(strings.ReplaceAll(string(guide), "\r\n", "\n"), begin+example+end,
		"regenerate the guide example with -update-guide-examples, then make agent-guide")

	// Follow the example's returned byte window through the real handler.
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessage, Arguments: map[string]any{"id": message.ID, "offset": 13}})
	requirements.NoError(err)
	requirements.False(result.IsError)
	response, ok = result.StructuredContent.(map[string]any)
	requirements.True(ok)
	assertions.Equal(" Here is the project update.", response["body_text"])
	assertions.Equal(false, response["has_more"])
}
