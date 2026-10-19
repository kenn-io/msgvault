package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
)

func TestDaemonInboxIMAPDelegatedDiscoveryUsesCurrentGrant(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	a, target, counts := inboxIMAPDaemonFixtureWithCommands(t)
	a.config.Server.APIKey = "synthetic-imap-discovery-owner-key"
	a.config.Server.AgentAccess = true
	factory := a.emailTagClientFactory
	var factoryCalls atomic.Int64
	a.emailTagClientFactory = func(ctx context.Context, source *store.Source) (gmail.API, error) {
		factoryCalls.Add(1)
		return factory(ctx, source)
	}
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: a.config, Store: a, Logger: slog.New(slog.DiscardHandler), OperationGate: api.NewSerialOperationGate()}).Router())
	t.Cleanup(server.Close)
	delegated := inboxIMAPDelegatedClient(t, server.URL, a.config.Server.APIKey, target.SourceID, "inbox.read")
	grant, err := delegated.AgentTokenSelf(t.Context())
	requirements.NoError(err)
	requirements.NotNil(grant)
	requirements.Len(grant.Sources, 1)
	assertions.Equal("imap", grant.Sources[0].Type)
	assertions.Equal(target.SourceID, grant.Sources[0].ID)
	descriptor, err := delegated.MCPCapabilities(t.Context())
	requirements.NoError(err)
	requirements.True(descriptor.HasInboxContract(), "the live registered route must admit a real IMAP-only read grant")
	assertions.True(descriptor.Delegated)
	assertions.Contains(descriptor.InboxOperations, string(inboxcontrol.OpGetState))
	for _, operation := range []inboxcontrol.Operation{inboxcontrol.OpSetRead, inboxcontrol.OpArchive, inboxcontrol.OpTags, inboxcontrol.OpCreateFolder} {
		assertions.NotContains(descriptor.InboxOperations, string(operation))
	}
	assertions.Zero(factoryCalls.Load(), "live discovery never constructs a native provider")

	// A stale broader permission list cannot widen the current real read grant.
	narrowed := mcpserver.ServeOptions{GrantPermissions: []string{"inbox.read", "inbox.read-state"}}
	admitInboxMCP(t.Context(), delegated, &narrowed, false)
	requirements.NotNil(narrowed.Inbox)
	assertions.Contains(narrowed.InboxOperations, inboxcontrol.OpGetState)
	assertions.NotContains(narrowed.InboxOperations, inboxcontrol.OpSetRead)
	assertions.Zero(factoryCalls.Load(), "fresh narrowing remains discovery-only")

	// Use the production delegated admission path and real MCP transport.
	session := nativeInboxMCPSession(t, delegated)
	encoded, err := json.Marshal(target)
	requirements.NoError(err)
	var targetArgs map[string]any
	requirements.NoError(json.Unmarshal(encoded, &targetArgs))
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_get_state", Arguments: map[string]any{"target": targetArgs}})
	requirements.NoError(err)
	requirements.False(result.IsError)
	assertions.Positive(factoryCalls.Load(), "an admitted read reaches the real native provider")

	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: a.config.Server.APIKey, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = owner.Close() })
	requirements.NoError(owner.RevokeAgentToken(t.Context(), grant.ID))
	beforeDenied := factoryCalls.Load()
	_, err = delegated.MCPCapabilities(t.Context())
	requirements.ErrorIs(err, daemonclient.ErrMCPCapabilitiesUnavailable)
	stale := mcpserver.ServeOptions{GrantPermissions: grant.Permissions}
	admitInboxMCP(t.Context(), delegated, &stale, false)
	assertions.Nil(stale.Inbox, "cached grant permissions never override fresh live revocation")
	assertions.Empty(stale.InboxOperations)
	assertions.Equal(beforeDenied, factoryCalls.Load())
	descriptor, err = owner.MCPCapabilities(t.Context())
	requirements.NoError(err)
	assertions.True(descriptor.HasInboxContract(), "the route remains available to the current owner")
	assertions.Zero(counts.stores.Load())
	assertions.Zero(counts.moves.Load())
	assertions.Zero(counts.copies.Load())
	assertions.Zero(counts.expunges.Load())
	assertions.Zero(counts.creates.Load())
}
