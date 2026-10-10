package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDaemonMCPEventsConfigurationAndStartup(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Server.APIKey = "synthetic-events-owner"
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Logger: testLoggerValue(), OperationGate: api.NewSerialOperationGate()})
	t.Cleanup(func() { Require.NoError(t, server.Shutdown(context.Background())) })
	keyPath := filepath.Join(cfg.Data.DataDir, "mcp-events.key")
	disabled, err := newDaemonMCPEventsService(t.Context(), cfg, st, server)
	require.NoError(err)
	assert.Empty(disabled.Catalog().Events)
	_, err = os.Stat(keyPath)
	require.ErrorIs(err, os.ErrNotExist)
	cfg.MCP.Events.Enabled = true
	enabled, err := newDaemonMCPEventsService(t.Context(), cfg, st, server)
	require.NoError(err)
	require.Len(enabled.Capabilities(), 5)
	key, err := os.ReadFile(keyPath)
	require.NoError(err)
	require.Len(key, 32)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- enabled.Run(ctx) }()
	cancel()
	require.NoError(<-done)
	require.NoError(os.WriteFile(keyPath, []byte("synthetic-corrupt-key"), 0600))
	_, err = newDaemonMCPEventsService(t.Context(), cfg, st, server)
	require.ErrorContains(err, "events_key_unavailable")
	after, err := os.ReadFile(keyPath)
	require.NoError(err)
	assert.Equal([]byte("synthetic-corrupt-key"), after)
}
