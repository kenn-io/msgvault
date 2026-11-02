package mcp

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSenderAuthThroughDaemonMCP(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := testutil.NewTestStore(t)
	source, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "sender@example.test", "receipt", "receipt body")
	must.NoError(err)
	_, outside, err := testutil.CreateIndexedSourceMessage(st, "outside@example.test", "sender@example.test", "outside", "private body")
	must.NoError(err)
	pid, err := st.EnsureParticipant("sender@example.test", "Sender", "example.test")
	must.NoError(err)
	for _, messageID := range []int64{id, outside} {
		_, err = st.DB().Exec(st.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES (?,?,'from')`), messageID, pid)
		must.NoError(err)
	}
	raw := []byte("From: sender@example.test\r\nAuthentication-Results: mx.test; dmarc=pass header.from=example.test\r\n\r\nreceipt body")
	must.NoError(st.UpsertMessageRaw(id, raw))
	must.NoError(st.UpsertMessageRaw(outside, raw))
	cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}, SenderAuth: config.SenderAuthConfig{Sources: []config.SenderAuthSource{{SourceID: source.ID, TrustedAuthservIDs: []string{"mx.test"}}}}}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	must.NoError(err)
	t.Cleanup(func() { _ = owner.Close() })
	grant, err := owner.IssueAgentToken(t.Context(), "receipt reader", []string{"search.read", "message.read"}, []int64{source.ID}, nil, time.Time{})
	must.NoError(err)
	agent, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true})
	must.NoError(err)
	t.Cleanup(func() { _ = agent.Close() })
	for _, client := range []*daemonclient.Client{owner, agent} {
		cliDetail, err := client.GetCLIMessage(t.Context(), strconv.FormatInt(id, 10))
		must.NoError(err)
		must.NotNil(cliDetail.SenderAuth)
		checks.True(cliDetail.SenderAuth.Verified)
		opts := ServeOptions{Engine: daemonclient.NewEngineAdapter(client)}
		if client == agent {
			opts = agentServeOptions(t, agent)
		}
		session := task5ConnectClient(t, opts, true)
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessage, Arguments: map[string]any{"id": id}})
		must.NoError(err)
		must.False(result.IsError, task5SDKResultText(result))
		var detail struct {
			SenderAuth *senderauth.Verdict `json:"sender_auth"`
		}
		must.NoError(json.Unmarshal([]byte(task5SDKResultText(result)), &detail))
		must.NotNil(detail.SenderAuth)
		checks.True(detail.SenderAuth.Verified)
		for _, call := range []struct {
			name string
			args map[string]any
		}{
			{ToolListThread, map[string]any{"id": id}},
			{ToolSearchMessages, map[string]any{"query": "receipt"}},
		} {
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: call.name, Arguments: call.args})
			must.NoError(err)
			must.False(result.IsError, task5SDKResultText(result))
			checks.Contains(task5SDKResultText(result), `"verified":true`)
		}
	}
	// Replacement and request snapshots must retain the startup pins, even if
	// the caller later mutates the config object.
	cfg.SenderAuth.Sources[0].TrustedAuthservIDs[0] = "evil.test"
	srv.SetAnalyticsEngine(query.NewEngine(st.DB(), st.IsPostgreSQL()), api.AnalyticsModeSQL)
	ownerDetail, err := owner.GetCLIMessage(t.Context(), strconv.FormatInt(id, 10))
	must.NoError(err)
	must.NotNil(ownerDetail.SenderAuth)
	checks.True(ownerDetail.SenderAuth.Verified)
	engine := daemonclient.NewEngineAdapter(agent)
	detail, err := engine.GetMessage(t.Context(), id)
	must.NoError(err)
	must.NotNil(detail)
	must.NotNil(detail.SenderAuth)
	checks.True(detail.SenderAuth.Verified)
	denied, err := engine.GetMessage(t.Context(), outside)
	checks.Nil(denied)
	checks.NoError(err)
}
