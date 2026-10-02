package cmd

import (
	"encoding/json/v2"
	"log/slog"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMCPLaneReadinessUsesNativeOwnerAndTypedSDKOutput(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	requirements.NoError(cfg.Save())
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Logger: slog.New(slog.DiscardHandler), LaneReadinessReader: newDaemonLaneReadinessReader(cfg, st)})
	backend := sourceOperationFixture(t, srv)
	requirements.Contains(backend.capabilities(), "get_lane_readiness")
	result, err := backend.ExecuteOperation(t.Context(), "get_lane_readiness", nil)
	requirements.NoError(err)
	requirements.False(result.IsError)
	report := operationOutput[api.LaneReadinessResponse](t, result)
	assertions.Len(report.Lanes, 8)
	assertions.True(report.StoreAvailable)
	wire, err := json.Marshal(report)
	requirements.NoError(err)
	assertions.NotContains(string(wire), cfg.HomeDir)
	session := operationMCPSession(t, backend, nil, nil)
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_lane_readiness"})
	requirements.NoError(err)
	assertions.False(called.IsError)
	assertions.NotNil(called.StructuredContent)
	for _, arguments := range []map[string]any{{"path": "untrusted"}, {"env": "untrusted"}, {"coverage": true}, {"limit": nil}} {
		called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_lane_readiness", Arguments: arguments})
		requirements.NoError(err)
		assertions.True(called.IsError)
	}
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	stripped := *capabilities
	stripped.Routes = []apiprotocol.MCPRouteDescriptor{}
	for _, route := range capabilities.Routes {
		if route.OperationID != "getLaneReadiness" {
			stripped.Routes = append(stripped.Routes, route)
		}
	}
	assertions.NotContains(newDaemonMCPOperations(backend.client, &stripped).capabilities(), "get_lane_readiness")
	capabilities.Delegated = true
	assertions.NotContains(newDaemonMCPOperations(backend.client, capabilities).capabilities(), "get_lane_readiness")
	unavailable := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)}))
	result, err = unavailable.ExecuteOperation(t.Context(), "get_lane_readiness", nil)
	requirements.NoError(err)
	requirements.True(result.IsError)
	wire, err = json.Marshal(result.Output)
	requirements.NoError(err)
	assertions.Contains(string(wire), "lane_readiness_unavailable")
}

func TestMCPLaneReadinessUnavailableSchemaRemainsSafe(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	cfg := config.NewDefaultConfig()
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)}))
	session := operationMCPSession(t, backend, nil, nil)
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_lane_readiness"})
	requirements.NoError(err)
	assertions.True(called.IsError)
	wire, err := json.Marshal(called.StructuredContent)
	requirements.NoError(err)
	assertions.Contains(string(wire), "lane_readiness_unavailable")
}
