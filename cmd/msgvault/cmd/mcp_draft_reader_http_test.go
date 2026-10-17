package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/mcpdiscovery"
	"go.kenn.io/msgvault/internal/testutil"
)

// Exercises the actual owner HTTP MCP draft_get adapter and daemon CLI runner.
// The read needs no provider credentials and never contacts a provider.
func TestMCPDraftGetOwnerHTTPReadsBeeperAndLocalChat(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const owner = "synthetic-draft-reader-owner"
	st := testutil.NewTestStore(t)
	beeperSource, err := st.GetOrCreateSource("beeper", "synthetic-beeper")
	require.NoError(err)
	beeperDraft, err := st.CreateBeeperDraftContext(t.Context(), beeperSource.ID, "synthetic-chat", "Synthetic Beeper draft body")
	require.NoError(err)
	beeperDraft, err = st.FinishBeeperDraftContext(t.Context(), beeperDraft.DraftID, beeperDraft.Revision, "Synthetic Beeper draft body")
	require.NoError(err)
	chatSource, err := st.GetOrCreateSource("slack", "synthetic-slack")
	require.NoError(err)
	conversation, err := st.EnsureConversationWithType(chatSource.ID, "synthetic-channel", "channel", "Synthetic channel")
	require.NoError(err)
	chatDraft, err := st.CreateChatDraftContext(t.Context(), conversation, 0, "Synthetic local chat draft body", func(sourceType, identifier string) error {
		assert.Equal("slack", sourceType)
		assert.Equal("synthetic-slack", identifier)
		return nil
	})
	require.NoError(err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: owner}}
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st, config: cfg}, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { assert.NoError(server.Shutdown(context.Background())) })
	daemon := httptest.NewServer(server.Router())
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: owner, AllowInsecure: true})
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(client.Close()) })
	ctx := withStoreResolverConfig(t, cfg)
	opts := daemonMCPServeOptions(ctx, client, invocationFromContext(ctx))
	require.NotNil(opts.Drafts)
	require.Contains(opts.DraftCommands, "draft-get")
	discoveryDir := t.TempDir()
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	// Read-class draft_get remains available without opting into HTTP writes.
	go func() {
		done <- mcpserver.ServeHTTPWithOptions(runCtx, opts, mcpserver.HTTPOptions{Addr: "127.0.0.1:0", APIKey: owner, DiscoveryDirectory: discoveryDir, BackendURL: daemon.URL})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assert.True(err == nil || errors.Is(err, context.Canceled), "%v", err)
		case <-time.After(30 * time.Second):
			assert.Fail("MCP listener did not stop")
		}
	})
	var endpoint string
	require.Eventually(func() bool {
		endpoints, err := mcpdiscovery.List(discoveryDir)
		if err != nil || len(endpoints) != 1 {
			return false
		}
		endpoint = endpoints[0].URL
		return endpoint != ""
	}, 30*time.Second, 20*time.Millisecond, "MCP listener was not published")

	beeperResult := mcpOwnerHTTPDraftRead(t, endpoint, owner, map[string]any{"draft_id": beeperDraft.DraftID})
	assert.Equal("Synthetic Beeper draft body", beeperResult["content"])
	chatResult := mcpOwnerHTTPDraftRead(t, endpoint, owner, map[string]any{"draft_id": chatDraft.DraftID})
	assert.Equal("Synthetic local chat draft body", chatResult["body"])
	listResult := mcpOwnerHTTPDraftRead(t, endpoint, owner, map[string]any{"conversation": float64(conversation)})
	drafts, ok := listResult["data"].([]any)
	require.True(ok)
	require.Len(drafts, 1)
	draft, ok := drafts[0].(map[string]any)
	require.True(ok)
	assert.Equal(chatDraft.DraftID, draft["draft_id"])
	assert.Equal("Synthetic local chat draft body", draft["body"])
}

func mcpOwnerHTTPDraftRead(t *testing.T, endpoint, owner string, arguments map[string]any) map[string]any {
	t.Helper()
	require := require.New(t)
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "draft_get", "arguments": arguments, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}}})
	require.NoError(err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, bytes.NewReader(raw))
	require.NoError(err)
	request.Header.Set("Authorization", "Bearer "+owner)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", "draft_get")
	response, err := http.DefaultClient.Do(request)
	require.NoError(err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(http.StatusOK, response.StatusCode)
	var wire map[string]any
	require.NoError(json.NewDecoder(response.Body).Decode(&wire))
	require.Nil(wire["error"])
	result, ok := wire["result"].(map[string]any)
	require.True(ok)
	require.NotEqual(true, result["isError"], "%v", result["content"])
	content, ok := result["structuredContent"].(map[string]any)
	require.True(ok)
	return content
}
