package mcpevents

import (
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"path/filepath"
	"testing"
)

func TestMCPAllEligibleSelectionReachesFutureNativeSources(t *testing.T) {
	for _, selection := range []struct {
		name    string
		sources []string
		matrix  bool
		gmail   bool
	}{
		{"all", []string{"*"}, true, true}, {"explicit", []string{"gmail"}, false, true}, {"matrix", []string{"matrix"}, true, false},
	} {
		t.Run(selection.name, func(t *testing.T) {
			require := Require.New(t)

			f := storetest.New(t)
			requireNativeGmailFixtureRead(t, f)
			service, err := New(t.Context(), f.Store, Options{Enabled: true, Sources: selection.sources, OwnerKey: "synthetic-owner", KeyPath: filepath.Join(t.TempDir(), "events.key")})
			require.NoError(err)
			require.NotEmpty(service.Catalog().Events)
			_, _, err = f.Store.ValidateMCPEventScope(t.Context(), messageFamily, "conversation", f.ConvID, []string{"message"})
			if selection.gmail {
				require.NoError(err)
			} else {
				require.ErrorContains(err, "source_not_capable")
			}
			// This source appears after service startup: selection cannot be a snapshot
			// of the archive IDs that existed when configuration was loaded.
			source, err := f.Store.GetOrCreateSource("matrix", "@archive:example.net")
			require.NoError(err)
			resource, err := f.Store.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			require.NoError(f.Store.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "matrix", NativeIdentity: source.Identifier, Known: true, MessageRead: true}))
			require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), store.NativeMessageScopeInventory{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "matrix", NativeIdentity: source.Identifier, Kind: "matrix_room", Scopes: []store.NativeMessageScope{{ProviderID: "!room:example.net", DisplayName: "Synthetic room"}}}))
			conversation, err := f.Store.EnsureConversation(source.ID, "!room:example.net", "Synthetic room")
			require.NoError(err)
			_, _, err = f.Store.ValidateMCPEventScope(t.Context(), messageFamily, "conversation", conversation, []string{"message"})
			if selection.matrix {
				require.NoError(err)
			} else {
				require.ErrorContains(err, "source_not_capable")
			}
			unsupported, err := f.Store.GetOrCreateSource("twilio", "synthetic-account")
			require.NoError(err)
			conversation, err = f.Store.EnsureConversation(unsupported.ID, "synthetic-call", "Synthetic call")
			require.NoError(err)
			_, _, err = f.Store.ValidateMCPEventScope(t.Context(), messageFamily, "conversation", conversation, []string{"message"})
			require.ErrorContains(err, "source_not_capable")
		})
	}
}
