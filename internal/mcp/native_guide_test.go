package mcp

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentguide"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

const nativeGuideFixtureBody = "Hello, reader. The quarterly report includes an invoice."

func nativeGuideFixture(t *testing.T) (*sdkmcp.ClientSession, int64) {
	t.Helper()
	f := storetest.New(t)
	id := f.PersistEmail(storetest.EmailSpec{
		From: "sender@example.com", To: []string{storetest.OwnerAddress},
		Subject: "Invoice", Body: nativeGuideFixtureBody,
		SentAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, f.Store.IndexStoredMessageContext(t.Context(), id))
	engine := query.NewSQLiteEngine(f.Store.DB())
	if f.Store.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(f.Store.DB(), query.PostgreSQLQueryDialect{})
	}
	return task5ConnectClient(t, ServeOptions{Engine: engine}, false), id
}

// This exercises the offline renderer's examples through the real MCP wire and
// fixture archive. Fetching the guide over msgvault.skills needs its transport
// dependency and is exercised separately in the integration worktree.
func TestNativeQuickstartExamples(t *testing.T) {
	session, id := nativeGuideFixture(t)
	exerciseNativeQuickstart(t, session, agentguide.QuickstartMarkdown(), id)
}

func exerciseNativeQuickstart(t *testing.T, session *sdkmcp.ClientSession, markdown string, id int64) {
	t.Helper()
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	schemas := make(map[string]*jsonschema.Resolved)
	for _, tool := range listed.Tools {
		encoded, err := json.Marshal(tool.InputSchema)
		require.NoError(t, err)
		var schema jsonschema.Schema
		require.NoError(t, json.Unmarshal(encoded, &schema))
		schemas[tool.Name], err = schema.Resolve(nil)
		require.NoError(t, err)
	}
	examples := regexp.MustCompile("(?s)```json\n(.*?)\n```").FindAllStringSubmatch(markdown, -1)
	require.NotEmpty(t, examples)
	for _, example := range examples {
		var call struct {
			Tool      string         `json:"tool"`
			Arguments map[string]any `json:"arguments"`
		}
		require.NoError(t, json.Unmarshal([]byte(example[1]), &call))
		t.Run(call.Tool, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			schema, available := schemas[call.Tool]
			require.True(available, "published example must use an available native tool")
			require.NoError(schema.Validate(call.Arguments), "published literal example must match its real schema")
			// The guide instructs callers to replace synthetic IDs with their own
			// search result. Keep every other published argument unchanged.
			if _, hasID := call.Arguments["id"]; hasID {
				call.Arguments["id"] = id
			}
			arguments, err := json.Marshal(call.Arguments)
			require.NoError(err)
			t.Logf("schema validated %s; wire arguments %s", call.Tool, arguments)
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: call.Tool, Arguments: call.Arguments})
			require.NoError(err)
			require.False(result.IsError, "%s", task5SDKResultText(result))
			encoded, err := json.Marshal(result.StructuredContent)
			require.NoError(err)
			switch call.Tool {
			case ToolSearchMetadata, ToolSearchMessageBodies, ToolListThread:
				var page struct {
					Data []struct {
						ID int64 `json:"id"`
					} `json:"data"`
					Messages []struct {
						ID int64 `json:"id"`
					} `json:"messages"`
				}
				require.NoError(json.Unmarshal(encoded, &page))
				if call.Tool == ToolListThread {
					require.Len(page.Messages, 1)
					assert.Equal(id, page.Messages[0].ID)
				} else {
					require.Len(page.Data, 1, "%s", encoded)
					assert.Equal(id, page.Data[0].ID)
				}
			case ToolGetMessage:
				var body struct {
					Text     string `json:"body_text"`
					Returned int    `json:"body_returned"`
					HasMore  bool   `json:"has_more"`
				}
				var complete strings.Builder
				offset := 0
				for {
					require.NoError(json.Unmarshal(encoded, &body))
					complete.WriteString(body.Text)
					offset += body.Returned
					if !body.HasMore {
						break
					}
					require.Positive(body.Returned, "a continued page must advance")
					require.Less(offset, len(nativeGuideFixtureBody))
					call.Arguments["offset"] = offset
					result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: call.Tool, Arguments: call.Arguments})
					require.NoError(err)
					require.False(result.IsError)
					encoded, err = json.Marshal(result.StructuredContent)
					require.NoError(err)
				}
				assert.Equal(nativeGuideFixtureBody, complete.String())
			case ToolAggregate:
				assert.Contains(string(encoded), "sender@example.com")
			case ToolGetStats:
				assert.Contains(string(encoded), storetest.OwnerAddress)
			}
		})
	}
}
