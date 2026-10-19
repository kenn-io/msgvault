package cmd

import (
	"context"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Constructors remain available inside callbacks.
	Require "github.com/stretchr/testify/require" //nolint:importas // Constructors remain available inside callbacks.
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"io"
	"path/filepath"
	"strconv"
	"testing"
)

func TestDaemonStartupPausesPersistedNativeMatrixAuthority(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("matrix", "@archive:example.org")
			require.NoError(err)
			resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			observation := store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "matrix", NativeIdentity: source.Identifier, Known: true, MessageRead: true}
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), observation))
			require.NoError(st.ObserveNativeMessageScopes(t.Context(), store.NativeMessageScopeInventory{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "matrix", NativeIdentity: source.Identifier, Kind: "matrix_room", Complete: true, Scopes: []store.NativeMessageScope{{ProviderID: "!room:example.org", DisplayName: "Inbox"}}}))
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
			assert.Equal(!enabled, allowed, "only Events-enabled startup pauses persisted native permission")
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

func TestNativeMatrixMissingCredentialsPausesOnlySelectedAccount(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	source, err := st.GetOrCreateSource("matrix", "@archive:example.org")
	require.NoError(err)
	original, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	require.NoError(st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: source.ID, SourceReference: original.Reference, SourceType: "matrix", NativeIdentity: source.Identifier, Known: true, MessageRead: true}))
	other, err := st.GetOrCreateSource("matrix", "@other:example.org")
	require.NoError(err)
	otherResource, err := st.GetMCPResource(t.Context(), "source", other.ID)
	require.NoError(err)
	require.NoError(st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: other.ID, SourceReference: otherResource.Reference, SourceType: "matrix", NativeIdentity: other.Identifier, Known: true, MessageRead: true}))
	err = runMatrixSync(t.Context(), st, cfg, source.Identifier, false, nil, io.Discard)
	require.Error(err)
	allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.False(allowed, "missing selected credentials pause prior native read authority")
	allowed, err = st.NativeSourceMessagePermission(t.Context(), other.ID)
	require.NoError(err)
	assert.True(allowed, "unselected account remains independent")
	current, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	assert.Equal(original.Reference, current.Reference, "local credential absence is not native identity removal")
}
