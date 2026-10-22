package cmd

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Persisted prior-process permission is not current native credential evidence.
// Startup pauses it without replacing account or inbox lifetimes; the selected
// native adapter must verify its identity/grant again before Events resume.
func TestDaemonStartupPausesPersistedNativeGmailAuthority(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("gmail", "owner@example.net")
			require.NoError(err)
			resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			observation := store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Known: true, MessageRead: true}
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), observation))
			require.NoError(st.ObserveNativeMessageScopes(t.Context(), store.NativeMessageScopeInventory{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Kind: "gmail_label", Complete: true, Scopes: []store.NativeMessageScope{{ProviderID: "INBOX", DisplayName: "Inbox"}}}))
			page, err := st.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-startup-owner", Kind: "inbox", SourceID: source.ID})
			require.NoError(err)
			require.Len(page.Resources, 1)
			inbox := page.Resources[0]
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			cfg.Server.APIKey = "synthetic-startup-owner"
			cfg.MCP.Events.Enabled = enabled
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Logger: testLoggerValue(), OperationGate: api.NewSerialOperationGate()})
			t.Cleanup(func() { assert.NoError(server.Shutdown(context.Background())) })
			_, err = newDaemonMCPEventsService(t.Context(), cfg, st, server)
			require.NoError(err)
			allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.False(allowed, "a new daemon must not inherit native permission from a prior process without observing its current selected credentials")
			current, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			assert.Equal(resource.Reference, current.Reference)
			currentInbox, err := st.GetMCPResource(t.Context(), "inbox", inbox.ID)
			require.NoError(err)
			assert.Equal(inbox.Reference, currentInbox.Reference, "startup does not assert native scope removal")
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), observation))
			restored, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.True(restored)
			current, err = st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			assert.Equal(resource.Reference, current.Reference, "verified native restoration after an unknown pause keeps the same resource lifetime")
			if !enabled {
				assert.NoFileExists(filepath.Join(cfg.Data.DataDir, "mcp-events.key"))
			}
		})
	}
}

func TestDaemonStartupPausesPersistedNativeMicrosoftAuthority(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("msmail", "owner@example.net")
			require.NoError(err)
			resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			observation := store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "msmail", NativeIdentity: source.Identifier, Known: true, MessageRead: true}
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), observation))
			require.NoError(st.ObserveNativeMessageScopes(t.Context(), store.NativeMessageScopeInventory{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "msmail", NativeIdentity: source.Identifier, Kind: "msmail_folder", Complete: true, Scopes: []store.NativeMessageScope{{ProviderID: "inbox", DisplayName: "Inbox"}}}))
			page, err := st.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-startup-owner", Kind: "inbox", SourceID: source.ID})
			require.NoError(err)
			require.Len(page.Resources, 1)
			inbox := page.Resources[0]
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			cfg.Server.APIKey = "synthetic-startup-owner"
			cfg.MCP.Events.Enabled = enabled
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Logger: testLoggerValue(), OperationGate: api.NewSerialOperationGate()})
			t.Cleanup(func() { assert.NoError(server.Shutdown(context.Background())) })
			_, err = newDaemonMCPEventsService(t.Context(), cfg, st, server)
			require.NoError(err)
			allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.False(allowed, "a new daemon must not inherit native permission from a prior process without observing its current selected credentials")
			current, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			assert.Equal(resource.Reference, current.Reference)
			currentInbox, err := st.GetMCPResource(t.Context(), "inbox", inbox.ID)
			require.NoError(err)
			assert.Equal(inbox.Reference, currentInbox.Reference, "startup does not assert native scope removal")
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), observation))
			restored, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.True(restored)
			current, err = st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			assert.Equal(resource.Reference, current.Reference, "verified native restoration after an unknown pause keeps the same resource lifetime")
			if !enabled {
				assert.NoFileExists(filepath.Join(cfg.Data.DataDir, "mcp-events.key"))
			}
		})
	}
}

func TestDaemonStartupPausesPersistedNativeIMAPAuthority(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("imap", "owner@example.net")
			require.NoError(err)
			resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			observation := store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "imap", NativeIdentity: source.Identifier, Known: true, MessageRead: true}
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), observation))
			require.NoError(st.ObserveNativeMessageScopes(t.Context(), store.NativeMessageScopeInventory{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "imap", NativeIdentity: source.Identifier, Kind: "imap_mailbox", Complete: true, Scopes: []store.NativeMessageScope{{ProviderID: "INBOX", DisplayName: "INBOX", Epoch: "42"}}}))
			page, err := st.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-startup-owner", Kind: "inbox", SourceID: source.ID})
			require.NoError(err)
			require.Len(page.Resources, 1)
			inbox := page.Resources[0]
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			cfg.Server.APIKey = "synthetic-startup-owner"
			cfg.MCP.Events.Enabled = enabled
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Logger: testLoggerValue(), OperationGate: api.NewSerialOperationGate()})
			t.Cleanup(func() { assert.NoError(server.Shutdown(context.Background())) })
			_, err = newDaemonMCPEventsService(t.Context(), cfg, st, server)
			require.NoError(err)
			allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.False(allowed, "a new daemon must not inherit native permission from a prior process without observing its current selected credentials")
			current, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			assert.Equal(resource.Reference, current.Reference)
			currentInbox, err := st.GetMCPResource(t.Context(), "inbox", inbox.ID)
			require.NoError(err)
			assert.Equal(inbox.Reference, currentInbox.Reference, "startup does not assert native scope removal")
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), observation))
			restored, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.True(restored)
			current, err = st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			assert.Equal(resource.Reference, current.Reference, "verified native restoration after an unknown pause keeps the same resource lifetime")
			if !enabled {
				assert.NoFileExists(filepath.Join(cfg.Data.DataDir, "mcp-events.key"))
			}
		})
	}
}

func TestDaemonStartupPausesPersistedNativeSlackAuthority(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("slack", "TSYNTHETIC:USYNTHETIC")
			require.NoError(err)
			resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			observation := store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "slack", NativeIdentity: source.Identifier, Known: true, MessageRead: true}
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), observation))
			require.NoError(st.ObserveNativeMessageScopes(t.Context(), store.NativeMessageScopeInventory{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "slack", NativeIdentity: source.Identifier, Kind: "slack_channel", Complete: true, Scopes: []store.NativeMessageScope{{ProviderID: "CSYNTHETIC", DisplayName: "Synthetic channel", PermissionDomain: "channels"}}}))
			page, err := st.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-startup-owner", Kind: "inbox", SourceID: source.ID})
			require.NoError(err)
			require.Len(page.Resources, 1)
			inbox := page.Resources[0]
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			cfg.Server.APIKey = "synthetic-startup-owner"
			cfg.MCP.Events.Enabled = enabled
			cfg.MCP.Events.Sources = []string{"slack"}
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Logger: testLoggerValue(), OperationGate: api.NewSerialOperationGate()})
			t.Cleanup(func() { assert.NoError(server.Shutdown(context.Background())) })
			_, err = newDaemonMCPEventsService(t.Context(), cfg, st, server)
			require.NoError(err)
			allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.False(allowed, "a new daemon must not inherit native permission from a prior process without observing its current selected credentials")
			current, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			assert.Equal(resource.Reference, current.Reference)
			currentInbox, err := st.GetMCPResource(t.Context(), "inbox", inbox.ID)
			require.NoError(err)
			assert.Equal(inbox.Reference, currentInbox.Reference, "startup does not assert native scope removal")
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), observation))
			restored, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.True(restored)
			current, err = st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			assert.Equal(resource.Reference, current.Reference, "verified native restoration after an unknown pause keeps the same resource lifetime")
			if !enabled {
				assert.NoFileExists(filepath.Join(cfg.Data.DataDir, "mcp-events.key"))
			}
		})
	}
}

func TestDaemonStartupPausesPersistedNativeTeamsAuthority(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("teams", "owner@example.test")
			require.NoError(err)
			resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			observation := store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "teams", NativeIdentity: source.Identifier, Known: true, MessageRead: true}
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), observation))
			require.NoError(st.ObserveNativeMessageScopes(t.Context(), store.NativeMessageScopeInventory{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "teams", NativeIdentity: source.Identifier, Kind: "teams_chat", Complete: true, Scopes: []store.NativeMessageScope{{ProviderID: "CSYNTHETIC", DisplayName: "Synthetic channel"}}}))
			page, err := st.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-startup-owner", Kind: "inbox", SourceID: source.ID})
			require.NoError(err)
			require.Len(page.Resources, 1)
			inbox := page.Resources[0]
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			cfg.Server.APIKey = "synthetic-startup-owner"
			cfg.MCP.Events.Enabled = enabled
			cfg.MCP.Events.Sources = []string{"teams"}
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Logger: testLoggerValue(), OperationGate: api.NewSerialOperationGate()})
			t.Cleanup(func() { assert.NoError(server.Shutdown(context.Background())) })
			_, err = newDaemonMCPEventsService(t.Context(), cfg, st, server)
			require.NoError(err)
			allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.False(allowed, "a new daemon must not inherit native permission from a prior process without observing its current selected credentials")
			current, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			assert.Equal(resource.Reference, current.Reference)
			currentInbox, err := st.GetMCPResource(t.Context(), "inbox", inbox.ID)
			require.NoError(err)
			assert.Equal(inbox.Reference, currentInbox.Reference, "startup does not assert native scope removal")
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), observation))
			restored, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.True(restored)
			current, err = st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			assert.Equal(resource.Reference, current.Reference, "verified native restoration after an unknown pause keeps the same resource lifetime")
			if !enabled {
				assert.NoFileExists(filepath.Join(cfg.Data.DataDir, "mcp-events.key"))
			}
		})
	}
}

// The actual scheduled credential factory must pause old authority if the
// selected account's local token is gone, before any native importer starts.
func TestDaemonTeamsMissingCredentialsPausesNativeAuthority(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("teams", "owner@example.test")
	require.NoError(err)
	resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	require.NoError(st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "teams", NativeIdentity: source.Identifier, Known: true, MessageRead: true}))
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Microsoft.ClientID = "synthetic-client-id"
	err = runScheduledTeamsSync(t.Context(), source, st, &invocation{cfg: cfg, logger: testLoggerValue()})
	require.Error(err)
	allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.False(allowed, "missing selected credentials cannot retain old Events authority")
	current, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	assert.Equal(resource.Reference, current.Reference, "unknown credential absence does not assert native deletion")
}

func TestDaemonStartupPausesPersistedNativeDiscordAuthority(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("discord", "200")
			require.NoError(err)
			resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			observation := store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "discord", NativeIdentity: source.Identifier, Known: true, MessageRead: true}
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), observation))
			require.NoError(st.ObserveNativeMessageScopes(t.Context(), store.NativeMessageScopeInventory{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "discord", NativeIdentity: source.Identifier, Kind: "discord_channel", Complete: true, Scopes: []store.NativeMessageScope{{ProviderID: "300", DisplayName: "Inbox"}}}))
			page, err := st.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-startup-owner", Kind: "inbox", SourceID: source.ID})
			require.NoError(err)
			require.Len(page.Resources, 1)
			inbox := page.Resources[0]
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			cfg.Server.APIKey = "synthetic-startup-owner"
			cfg.MCP.Events.Enabled = enabled
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Logger: testLoggerValue(), OperationGate: api.NewSerialOperationGate()})
			t.Cleanup(func() { assert.NoError(server.Shutdown(context.Background())) })
			_, err = newDaemonMCPEventsService(t.Context(), cfg, st, server)
			require.NoError(err)
			allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.False(allowed, "a new daemon must not inherit native permission from a prior process without observing its current selected credentials")
			current, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			assert.Equal(resource.Reference, current.Reference)
			currentInbox, err := st.GetMCPResource(t.Context(), "inbox", inbox.ID)
			require.NoError(err)
			assert.Equal(inbox.Reference, currentInbox.Reference, "startup does not assert native scope removal")
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), observation))
			restored, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.True(restored)
			current, err = st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			assert.Equal(resource.Reference, current.Reference, "verified native restoration after an unknown pause keeps the same resource lifetime")
			if !enabled {
				assert.NoFileExists(filepath.Join(cfg.Data.DataDir, "mcp-events.key"))
			}
		})
	}
}
