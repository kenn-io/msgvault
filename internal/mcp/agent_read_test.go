package mcp

import (
	"database/sql"
	"log/slog"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestAgentReadsThroughMCPSDK(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("test", "reader@example.test")
	requirements.NoError(err)
	other, err := st.GetOrCreateSource("test", "outside@example.test")
	requirements.NoError(err)
	ids := make([]int64, 2)
	for i, source := range []*store.Source{src, other} {
		conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic")
		requirements.NoError(err)
		ids[i], err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: source.Identifier, MessageType: "email", Subject: sql.NullString{String: "glacier", Valid: true}})
		requirements.NoError(err)
		requirements.NoError(st.UpsertFTS(ids[i], "glacier", "", source.Identifier, "", ""))
	}
	_, err = st.CreateCollection("mixed", "", []int64{src.ID, other.ID})
	requirements.NoError(err)
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = owner.Close() })
	grant, err := owner.IssueAgentToken(t.Context(), "reader", []string{"search.read", "stats.read", "message.read"}, []int64{src.ID}, nil)
	requirements.NoError(err)
	agent, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = agent.Close() })
	session := task5ConnectClient(t, ServeOptions{Engine: daemonclient.NewEngineAdapter(agent), AgentReadOnly: true}, true)
	tools, err := session.ListTools(t.Context(), nil)
	requirements.NoError(err)
	templates, err := session.ListResourceTemplates(t.Context(), nil)
	requirements.NoError(err)
	assertions.Empty(templates.ResourceTemplates)
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	assertions.NotContains(names, ToolStageDeletion)
	assertions.NotContains(names, ToolExportEML)
	assertions.NotContains(names, ToolExportAttachment)
	assertions.NotContains(names, ToolQuerySQL)
	for _, tc := range []struct {
		name   string
		args   map[string]any
		denied bool
	}{
		{ToolGetMessage, map[string]any{"id": ids[0]}, false},
		{ToolGetMessage, map[string]any{"id": ids[1]}, true},
		{ToolGetStats, map[string]any{}, false},
		{ToolListMessages, map[string]any{}, false},
		{ToolSearchMetadata, map[string]any{"query": "glacier"}, false},
		{ToolSearchByDomains, map[string]any{"domains": "example.test"}, false},
		{ToolSearchMetadata, map[string]any{"query": "glacier", "collection": "mixed"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tc.name, Arguments: tc.args})
			requirements.NoError(err)
			requirements.NotNil(result)
			assertions.Equal(tc.denied, result.IsError, task5SDKResultText(result))
			if !tc.denied {
				assertions.NotContains(task5SDKResultText(result), other.Identifier)
			}
		})
	}
	requirements.NoError(owner.RevokeAgentToken(t.Context(), grant.ID))
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessage, Arguments: map[string]any{"id": ids[0]}})
	requirements.NoError(err)
	assertions.True(result.IsError)
}
