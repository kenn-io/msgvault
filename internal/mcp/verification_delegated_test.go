package mcp

import (
	"context"
	"fmt"
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

func TestVerificationDelegatedBodySearchWithheldContext(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "reader@example.test", "glacier", "glacier Your verification code is AB4C9D")
	requirements.NoError(err)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Engine: engine, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(owner.Close()) })
	_, err = owner.RebuildCLIFTS(t.Context(), nil)
	requirements.NoError(err)
	grant, err := owner.IssueAgentToken(t.Context(), "reader", []string{"search.read"}, []int64{src.ID}, nil, time.Time{})
	requirements.NoError(err)
	agent, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(agent.Close()) })
	session := task5ConnectClient(t, agentServeOptions(t, agent), true)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSearchMessageBodies, Arguments: map[string]any{"query": "glacier"}})
	requirements.NoError(err)
	requirements.False(result.IsError, task5SDKResultText(result))
	text := task5SDKResultText(result)
	assertions.Contains(text, fmt.Sprintf(`"id":%d`, id))
	assertions.Contains(text, `"masked":true`)
	assertions.NotContains(text, "AB4C")
	assertions.NotContains(text, `"snippet":"glacier Your`)
}
