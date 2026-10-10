package cmd

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDaemonMCPScopedCardDAVRecoveryUsesProductionStoreAdapter(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("adapter-recovery@example.test", "Synthetic Adapter Person", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}
	adapter := &storeAPIAdapter{store: st, config: cfg}
	daemon := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { assertions.NoError(daemon.Shutdown(context.Background())) })
	server := httptest.NewServer(daemon.Router())
	t.Cleanup(server.Close)
	backend, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(backend.Close()) })
	// Use the actual daemon MCP launch discovery, not a fabricated descriptor.
	opts := daemonMCPServeOptions(t.Context(), backend, nil)
	requirements.NotNil(opts.ScopedCardDAVReconcile)
	assertions.Nil(opts.ScopedCardDAVPreview)
	assertions.Nil(opts.ScopedCardDAVApprove)
	opts.AllowCardDAVWrites = true
	ctx, cancel := context.WithCancel(t.Context())
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- mcpserver.ServeTransport(ctx, opts, serverTransport) }()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-adapter-client", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(ctx, clientTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(session.Close()); cancel(); <-done })
	params := &sdkmcp.CallToolParams{Name: "reconcile_scoped_carddav_publication", Arguments: map[string]any{"person_id": person.ID, "approval_token": "synthetic-original-token", "idempotency_key": "synthetic-original-key"}}
	pending, err := session.CallTool(t.Context(), params)
	requirements.NoError(err)
	requirements.True(pending.NeedsInput())
	params.RequestState = pending.RequestState
	params.InputResponses = sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}
	result, err := session.CallTool(t.Context(), params)
	requirements.NoError(err)
	requirements.True(result.IsError)
	requirements.NotEmpty(result.Content)
	message, ok := result.Content[0].(*sdkmcp.TextContent)
	requirements.True(ok)
	assertions.Contains(message.Text, "carddav_receipt_not_found", "the production adapter must reach the native receipt lookup")
	var receipts int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM carddav_publication_receipts").Scan(&receipts))
	assertions.Zero(receipts, "recovery cannot admit a new publication")
}
