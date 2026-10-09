package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
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

func TestDaemonSyncChildStoreCapturesLiveEventsWithoutResettingCoverage(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Server.APIKey = "synthetic-events-owner"
	cfg.MCP.Events.Enabled = true
	cfg.MCP.Events.Sources = []string{"gmail"}
	parentStore, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(parentStore.InitSchema())
	require.NoError(runStartupMigrationsForIngest(parentStore))
	t.Cleanup(func() { Require.NoError(t, parentStore.Close()) })
	source, err := parentStore.GetOrCreateSource("gmail", "sync-fixture@example.test")
	require.NoError(err)
	conversationID, err := parentStore.EnsureConversation(source.ID, "sync-fixture-thread", "Sync fixture")
	require.NoError(err)
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: parentStore, Logger: testLoggerValue(), OperationGate: api.NewSerialOperationGate()})
	t.Cleanup(func() { Require.NoError(t, server.Shutdown(context.Background())) })
	_, err = newDaemonMCPEventsService(t.Context(), cfg, parentStore, server)
	require.NoError(err)
	var initialEpoch int64
	require.NoError(parentStore.DB().QueryRowContext(t.Context(), `SELECT capture_epoch FROM mcp_event_clock WHERE singleton=1`).Scan(&initialEpoch))

	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	captureOptions, err := daemonMCPEventsOptions(cfg)
	require.NoError(err)
	captureConfig := mcpevents.StoreCaptureConfig(captureOptions)
	childContext := (&storeAPIAdapter{config: cfg, mcpEventsCapture: true, mcpEventsCaptureConfig: &captureConfig}).invocationContext(t.Context())
	childEnv := daemonRuntimeChildEnv(childContext, []string{
		daemonMCPEventsCaptureEnv + "=0",
		daemonMCPEventsCaptureConfigEnv + "=forged",
	})
	require.Contains(childEnv, daemonMCPEventsCaptureEnv+"=1")
	assert.NotContains(childEnv, daemonMCPEventsCaptureEnv+"=0", "the daemon's effective runtime state overrides inherited environment")
	var encodedCaptureConfig string
	for _, entry := range childEnv {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == daemonMCPEventsCaptureConfigEnv {
			encodedCaptureConfig = value
		}
	}
	require.NotEmpty(encodedCaptureConfig)
	assert.NotContains(childEnv, daemonMCPEventsCaptureConfigEnv+"=forged", "the daemon's effective snapshot overrides inherited environment")
	disabledContext := (&storeAPIAdapter{config: cfg}).invocationContext(t.Context())
	disabledEnv := daemonRuntimeChildEnv(disabledContext, []string{
		daemonMCPEventsCaptureEnv + "=1",
		daemonMCPEventsCaptureConfigEnv + "=forged",
	})
	assert.NotContains(disabledEnv, daemonMCPEventsCaptureEnv+"=1", "disabled or unavailable parent Events capture cannot be forged by inherited environment")
	assert.NotContains(disabledEnv, daemonMCPEventsCaptureConfigEnv+"=forged", "disabled parent capture removes inherited snapshots")
	// A child reload can observe newer settings than the running daemon. It must
	// apply the parent's effective capture snapshot instead of resetting coverage.
	cfg.MCP.Events.Sources = []string{"gcal"}
	t.Setenv(daemonMCPEventsCaptureEnv, "1")
	t.Setenv(daemonMCPEventsCaptureConfigEnv, encodedCaptureConfig)
	childStore, cleanup, err := openWritableStoreAndInitWithInvocation(&invocation{cfg: cfg, logger: testDiscardLogger()}, runStartupMigrationsForIngest)
	require.NoError(err)
	t.Cleanup(cleanup)

	message := storetest.NewMessage(source.ID, conversationID).WithSourceMessageID("daemon-sync-live-message").Build()
	view := childStore.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: time.Now().UTC()})
	messageID, err := view.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: message})
	require.NoError(err)
	var count int
	require.NoError(childStore.DB().QueryRowContext(t.Context(), childStore.Rebind(`SELECT COUNT(*) FROM mcp_event_log WHERE family=? AND message_id=?`), "msgvault.message_archived", messageID).Scan(&count))
	assert.Equal(1, count, "a daemon sync child must produce the live occurrence consumed by the parent service")
	var finalEpoch int64
	require.NoError(childStore.DB().QueryRowContext(t.Context(), `SELECT capture_epoch FROM mcp_event_clock WHERE singleton=1`).Scan(&finalEpoch))
	assert.Equal(initialEpoch, finalEpoch, "reapplying identical child capture coverage must not reset its epoch")
}

func TestDirectWriteStoreJournalsLiveEventsWithDaemonSettings(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Server.APIKey = "synthetic-events-owner"
	cfg.MCP.Events.Enabled = true
	cfg.MCP.Events.Sources = []string{"gmail"}
	daemonStore, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(daemonStore.InitSchema())
	require.NoError(runStartupMigrationsForIngest(daemonStore))
	source, err := daemonStore.GetOrCreateSource("gmail", "direct-write@example.test")
	require.NoError(err)
	conversationID, err := daemonStore.EnsureConversation(source.ID, "direct-write-thread", "Direct write fixture")
	require.NoError(err)
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: daemonStore, Logger: testLoggerValue(), OperationGate: api.NewSerialOperationGate()})
	_, err = newDaemonMCPEventsService(t.Context(), cfg, daemonStore, server)
	require.NoError(err)
	require.NoError(server.Shutdown(context.Background()))
	var initialEpoch int64
	require.NoError(daemonStore.DB().QueryRowContext(t.Context(), `SELECT capture_epoch FROM mcp_event_clock WHERE singleton=1`).Scan(&initialEpoch))
	require.NoError(daemonStore.Close())

	// A standalone CLI process with the daemon's settings journals its live
	// writes for the daemon to deliver later.
	cliStore, cleanup, err := openWritableStoreAndInitWithInvocation(&invocation{cfg: cfg, logger: testDiscardLogger()}, runStartupMigrationsForIngest)
	require.NoError(err)
	t.Cleanup(cleanup)
	message := storetest.NewMessage(source.ID, conversationID).WithSourceMessageID("direct-write-live-message").Build()
	view := cliStore.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: time.Now().UTC()})
	messageID, err := view.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: message})
	require.NoError(err)
	var count int
	require.NoError(cliStore.DB().QueryRowContext(t.Context(), cliStore.Rebind(`SELECT COUNT(*) FROM mcp_event_log WHERE family=? AND message_id=?`), "msgvault.message_archived", messageID).Scan(&count))
	assert.Equal(1, count)
	var finalEpoch int64
	require.NoError(cliStore.DB().QueryRowContext(t.Context(), `SELECT capture_epoch FROM mcp_event_clock WHERE singleton=1`).Scan(&finalEpoch))
	assert.Equal(initialEpoch, finalEpoch, "matching settings keep the daemon's capture epoch")
}

func TestDirectWriteStoreWithoutEventsRecordsCaptureGap(t *testing.T) {
	require := Require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Server.APIKey = "synthetic-events-owner"
	cfg.MCP.Events.Enabled = true
	daemonStore, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(daemonStore.InitSchema())
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: daemonStore, Logger: testLoggerValue(), OperationGate: api.NewSerialOperationGate()})
	_, err = newDaemonMCPEventsService(t.Context(), cfg, daemonStore, server)
	require.NoError(err)
	require.NoError(server.Shutdown(context.Background()))
	var initialEpoch int64
	require.NoError(daemonStore.DB().QueryRowContext(t.Context(), `SELECT capture_epoch FROM mcp_event_clock WHERE singleton=1`).Scan(&initialEpoch))
	require.NoError(daemonStore.Close())

	// A CLI process that does not capture must not let subscribers resume as
	// if nothing happened: its writes start a new capture epoch.
	cfg.MCP.Events.Enabled = false
	cliStore, cleanup, err := openWritableStoreAndInitWithInvocation(&invocation{cfg: cfg, logger: testDiscardLogger()}, runStartupMigrationsForIngest)
	require.NoError(err)
	t.Cleanup(cleanup)
	var epoch int64
	var enabled bool
	require.NoError(cliStore.DB().QueryRowContext(t.Context(), `SELECT capture_epoch, enabled FROM mcp_event_clock WHERE singleton=1`).Scan(&epoch, &enabled))
	Assert.Greater(t, epoch, initialEpoch)
	Assert.False(t, enabled)
}

func TestDirectWriteStoreWithEventsOffKeepsOwnerSubscriptions(t *testing.T) {
	require := Require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Server.APIKey = "synthetic-events-owner"
	cfg.MCP.Events.Enabled = true
	cfg.MCP.Events.Sources = []string{"gmail"}
	daemonStore, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(daemonStore.InitSchema())
	source, err := daemonStore.GetOrCreateSource("gmail", "direct-write-owner@example.test")
	require.NoError(err)
	conversationID, err := daemonStore.EnsureConversation(source.ID, "direct-write-owner-thread", "Owner fixture")
	require.NoError(err)
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: daemonStore, Logger: testLoggerValue(), OperationGate: api.NewSerialOperationGate()})
	_, err = newDaemonMCPEventsService(t.Context(), cfg, daemonStore, server)
	require.NoError(err)
	require.NoError(server.Shutdown(context.Background()))
	now := time.Now().UTC()
	subscription := store.MCPSubscription{
		ID: "sub_" + strings.Repeat("0", 63) + "1", Principal: mcpevents.Principal(cfg.Server.APIKey),
		Name: "msgvault.message_archived", Arguments: []byte(`{"conversation_id":"` + strconv.FormatInt(conversationID, 10) + `","include_from_me":false}`),
		ScopeKind: "conversation", ScopeID: conversationID, SourceID: source.ID,
		CallbackURL: "https://receiver.example.net/hook", SecretEnc: []byte("synthetic-encrypted-secret"),
		SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(24 * time.Hour),
	}
	require.NoError(daemonStore.BindMCPSubscriptionScope(t.Context(), &subscription))
	_, _, err = daemonStore.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: subscription, Now: now})
	require.NoError(err)
	require.NoError(daemonStore.Close())

	// A CLI writer with Events off records a capture gap for the same owner;
	// it is not an owner change, so the owner can renew after re-enabling.
	cfg.MCP.Events.Enabled = false
	cliStore, cleanup, err := openWritableStoreAndInitWithInvocation(&invocation{cfg: cfg, logger: testDiscardLogger()}, runStartupMigrationsForIngest)
	require.NoError(err)
	t.Cleanup(cleanup)
	stopped, err := cliStore.GetMCPSubscription(t.Context(), subscription.ID)
	require.NoError(err)
	require.NotNil(stopped)
	Assert.Equal(t, "capture_gap", stopped.StopReason)
}
