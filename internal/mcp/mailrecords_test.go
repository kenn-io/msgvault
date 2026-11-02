package mcp

import (
	"database/sql"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMessageRecordsDaemonAndEmbeddedScope(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st := testutil.NewTestStore(t)
	source, allowed, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "allowed", "Order", "Order B-42")
	require.NoError(err)
	_, outside, err := testutil.CreateIndexedSourceMessage(st, "outside@example.test", "outside", "Outside", "Secret order")
	require.NoError(err)
	body := `<p>Order B-42</p><script type="application/ld+json">{"@context":"https://schema.org","@type":"Order","orderNumber":"B-42"}</script>`
	require.NoError(st.UpsertMessageBody(allowed, sql.NullString{String: "Order B-42", Valid: true}, sql.NullString{String: body, Valid: true}))
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	require.NoError(err)
	t.Cleanup(func() { _ = owner.Close() })
	grant, err := owner.IssueAgentToken(t.Context(), "records", []string{"message.read"}, []int64{source.ID}, nil, time.Time{})
	require.NoError(err)
	agent, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true})
	require.NoError(err)
	t.Cleanup(func() { _ = agent.Close() })
	session := task5ConnectClient(t, agentServeOptions(t, agent), true)
	embedded := NewEmbeddedReader(st, "")
	t.Cleanup(embedded.Close)
	for _, mode := range []string{"daemon", "embedded"} {
		t.Run(mode, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			call := func(id int64) *sdkmcp.CallToolResult {
				t.Helper()
				args := map[string]any{"message_id": float64(id)}
				if mode == "daemon" {
					result, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessageRecords, Arguments: args})
					require.NoError(callErr)
					return result
				}
				result, structured, callErr := embedded.Call(t.Context(), []int64{source.ID}, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: ToolGetMessageRecords}}, args)
				require.NoError(callErr)
				result.StructuredContent = structured
				return result
			}
			result := call(allowed)
			require.False(result.IsError, task5SDKResultText(result))
			value := task5StructuredAs[messageRecordsResponse](t, result)
			assert.Equal(allowed, value.MessageID)
			assert.Equal("order", value.Kind)
			require.Len(value.Records, 1)
			assert.Equal("B-42", value.Records[0].Fields["order_number"].Value)
			assert.Equal("unknown", value.Records[0].Fields["order_number"].Origin)
			assert.Equal("untrusted_text", value.Records[0].Fields["order_number"].Trust)
			require.NotNil(value.SenderAuth)
			assert.False(value.SenderAuth.Verified)
			assert.Equal(senderauth.Basis("no_raw"), value.SenderAuth.Basis)
			denied, missing := call(outside), call(outside+100000)
			assert.True(denied.IsError)
			assert.True(missing.IsError)
			assert.Equal(task5SDKResultText(missing), task5SDKResultText(denied))
		})
	}
	// The tool reads again rather than retaining extracted records.
	require.NoError(st.UpsertMessageBody(allowed, sql.NullString{}, sql.NullString{String: `<script type="application/ld+json">{broken`, Valid: true}))
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessageRecords, Arguments: map[string]any{"message_id": allowed}})
	require.NoError(err)
	require.False(result.IsError)
	assert.Empty(task5StructuredAs[messageRecordsResponse](t, result).Records)
	require.NoError(st.UpsertMessageBody(allowed, sql.NullString{String: "Your sign-in verification code is 654321", Valid: true}, sql.NullString{String: body, Valid: true}))
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessageRecords, Arguments: map[string]any{"message_id": allowed}})
	require.NoError(err)
	require.False(result.IsError)
	value := task5StructuredAs[messageRecordsResponse](t, result)
	assert.Equal("verification", value.Kind)
	assert.Empty(value.Records)
	assert.NotContains(task5SDKResultText(result), "654321")
}
