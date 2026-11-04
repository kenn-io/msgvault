package mcp

import (
	"database/sql"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMessageRecordsDaemonAndEmbeddedScope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, allowed, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "allowed", "Order", "Order B-42")
	requirements.NoError(err)
	_, outside, err := testutil.CreateIndexedSourceMessage(st, "outside@example.test", "outside", "Outside", "Secret order")
	requirements.NoError(err)
	_, loginFooter, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "login-footer", "Your order receipt", "Order B-43")
	requirements.NoError(err)
	body := `<p>Order B-42</p><script type="application/ld+json">{"@context":"https://schema.org","@type":"Order","orderNumber":"B-42"}</script>`
	requirements.NoError(st.UpsertMessageBody(allowed, sql.NullString{String: "Order B-42", Valid: true}, sql.NullString{String: body, Valid: true}))
	loginFooterBody := `<p>Order B-43</p><p>Sign in to view your order.</p><a href="https://shop.example.test/login?return=%2Forders">Sign in</a><script type="application/ld+json">{"@context":"https://schema.org","@type":"Order","orderNumber":"B-43"}</script>`
	requirements.NoError(st.UpsertMessageBody(loginFooter, sql.NullString{String: "Order B-43", Valid: true}, sql.NullString{String: loginFooterBody, Valid: true}))
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = owner.Close() })
	grant, err := owner.IssueAgentToken(t.Context(), "records", []string{"message.read"}, []int64{source.ID}, nil, time.Time{})
	requirements.NoError(err)
	agent, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = agent.Close() })
	session := task5ConnectClient(t, agentServeOptions(t, agent), true)
	embedded := NewEmbeddedReader(st, "")
	t.Cleanup(embedded.Close)
	for _, mode := range []string{"daemon", "embedded"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			call := func(id int64) *sdkmcp.CallToolResult {
				t.Helper()
				args := map[string]any{"message_id": float64(id)}
				if mode == "daemon" {
					result, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessageRecords, Arguments: args})
					requirements.NoError(callErr)
					return result
				}
				result, structured, callErr := embedded.Call(t.Context(), []int64{source.ID}, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: ToolGetMessageRecords}}, args)
				requirements.NoError(callErr)
				result.StructuredContent = structured
				return result
			}
			result := call(allowed)
			requirements.False(result.IsError, task5SDKResultText(result))
			value := task5StructuredAs[messageRecordsResponse](t, result)
			assertions.Equal(allowed, value.MessageID)
			assertions.Equal("order", value.Kind)
			requirements.Len(value.Records, 1)
			assertions.Equal("B-42", value.Records[0].Fields["order_number"].Value)
			assertions.Equal("unknown", value.Records[0].Fields["order_number"].Origin)
			assertions.Equal("untrusted_text", value.Records[0].Fields["order_number"].Trust)
			assertions.Nil(value.SenderAuth)
			receipt := call(loginFooter)
			requirements.False(receipt.IsError, task5SDKResultText(receipt))
			receiptValue := task5StructuredAs[messageRecordsResponse](t, receipt)
			assertions.Equal("order", receiptValue.Kind)
			requirements.Len(receiptValue.Records, 1)
			assertions.Equal("B-43", receiptValue.Records[0].Fields["order_number"].Value)
			denied, missing := call(outside), call(outside+100000)
			assertions.True(denied.IsError)
			assertions.True(missing.IsError)
			assertions.Equal(task5SDKResultText(missing), task5SDKResultText(denied))
		})
	}
	// The tool reads again rather than retaining extracted records.
	requirements.NoError(st.UpsertMessageBody(allowed, sql.NullString{}, sql.NullString{String: `<script type="application/ld+json">{broken`, Valid: true}))
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessageRecords, Arguments: map[string]any{"message_id": allowed}})
	requirements.NoError(err)
	requirements.False(result.IsError)
	assertions.Empty(task5StructuredAs[messageRecordsResponse](t, result).Records)
	requirements.NoError(st.UpsertMessageBody(allowed, sql.NullString{String: "Your sign-in verification code is 654321", Valid: true}, sql.NullString{String: body, Valid: true}))
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessageRecords, Arguments: map[string]any{"message_id": allowed}})
	requirements.NoError(err)
	requirements.False(result.IsError)
	value := task5StructuredAs[messageRecordsResponse](t, result)
	assertions.Equal("verification", value.Kind)
	assertions.Empty(value.Records)
	assertions.NotContains(task5SDKResultText(result), "654321")
}
