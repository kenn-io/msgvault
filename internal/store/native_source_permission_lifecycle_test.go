package store_test

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestNativeSourcePermissionPinsLifetimeAndRevocation(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f := storetest.New(t)
	config := mcpStoreConfig()
	config.Capabilities = append(config.Capabilities, store.MCPEventCapability{Family: "msgvault.source_message_archived", SourceType: "gmail", Kinds: []string{"message"}})
	_, err := f.Store.ConfigureMCPEvents(t.Context(), config)
	require.NoError(err)
	source, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	permission := store.NativeSourcePermission{SourceID: f.Source.ID, SourceReference: source.Reference, SourceType: "gmail", NativeIdentity: f.Source.Identifier, Known: true, MessageRead: true}
	require.NoError(f.Store.ObserveNativeSourcePermission(t.Context(), permission))
	allowed, err := f.Store.NativeSourceMessagePermission(t.Context(), f.Source.ID)
	require.NoError(err)
	assert.True(allowed)
	_, binding, err := f.Store.ResolveMCPSubscriptionResource(t.Context(), "msgvault.source_message_archived", "source", f.Source.ID, []string{"message"}, source.Reference)
	require.NoError(err)
	require.NotNil(binding)
	require.NoError(f.Store.UpdateSourceDisplayName(f.Source.ID, "Synthetic renamed account"))
	require.NoError(f.Store.CheckMCPResourceBinding(t.Context(), binding))

	permission.MessageRead = false
	require.NoError(f.Store.ObserveNativeSourcePermission(t.Context(), permission))
	allowed, err = f.Store.NativeSourceMessagePermission(t.Context(), f.Source.ID)
	require.NoError(err)
	assert.False(allowed)
	require.Error(f.Store.CheckMCPResourceBinding(t.Context(), binding))
	permission.MessageRead = true
	require.EqualError(f.Store.ObserveNativeSourcePermission(t.Context(), permission), "resource_unavailable", "a stale observer cannot restore old authority")
	fresh, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	assert.NotEqual(source.Reference, fresh.Reference)
	permission.SourceReference = fresh.Reference
	require.NoError(f.Store.ObserveNativeSourcePermission(t.Context(), permission))
	allowed, err = f.Store.NativeSourceMessagePermission(t.Context(), f.Source.ID)
	require.NoError(err)
	assert.True(allowed)
	require.Error(f.Store.CheckMCPResourceBinding(t.Context(), binding), "restored native access must not revive the old subscription")

	require.NoError(f.Store.UpdateSourceIdentifier(f.Source.ID, "replacement@example.net"))
	require.NoError(f.Store.UpdateSourceIdentifier(f.Source.ID, f.Source.Identifier))
	allowed, err = f.Store.NativeSourceMessagePermission(t.Context(), f.Source.ID)
	require.NoError(err)
	assert.False(allowed, "away-and-back identity replacement must lose permission evidence")
	assert.EqualError(f.Store.ObserveNativeSourcePermission(t.Context(), permission), "resource_unavailable")
}

func TestNativeSourcePermissionRejectsContradictoryEvidence(t *testing.T) {
	for _, invalid := range []string{"wrong_identity", "wrong_provider", "wrong_reference", "unknown_positive"} {
		t.Run(invalid, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f := storetest.New(t)
			source, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
			require.NoError(err)
			permission := store.NativeSourcePermission{SourceID: f.Source.ID, SourceReference: source.Reference, SourceType: "gmail", NativeIdentity: f.Source.Identifier, Known: true, MessageRead: true}
			switch invalid {
			case "wrong_identity":
				permission.NativeIdentity = "other@example.net"
			case "wrong_provider":
				permission.SourceType = "imap"
			case "wrong_reference":
				conversation, err := f.Store.GetMCPResource(t.Context(), "conversation", f.ConvID)
				require.NoError(err)
				permission.SourceReference = conversation.Reference
			case "unknown_positive":
				permission.Known = false
			}
			require.Error(f.Store.ObserveNativeSourcePermission(t.Context(), permission))
			allowed, err := f.Store.NativeSourceMessagePermission(t.Context(), f.Source.ID)
			require.NoError(err)
			assert.False(allowed)
			var rows int
			require.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM native_source_permissions").Scan(&rows))
			assert.Zero(rows, "a rejected observation must not persist authority")
		})
	}
}

func TestNativeSourcePermissionUnknownIsNotAReadGrant(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f := storetest.New(t)
	source, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	permission := store.NativeSourcePermission{SourceID: f.Source.ID, SourceReference: source.Reference, SourceType: "gmail", NativeIdentity: f.Source.Identifier}
	require.NoError(f.Store.ObserveNativeSourcePermission(t.Context(), permission))
	allowed, err := f.Store.NativeSourceMessagePermission(t.Context(), f.Source.ID)
	require.NoError(err)
	assert.False(allowed)
	again, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	assert.Equal(source.Reference, again.Reference, "unknown evidence does not assert native removal")
}
