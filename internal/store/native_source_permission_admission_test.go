package store_test

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Deleting the native permission check must allow these archive-only accounts
// to subscribe again. Archive rows and enabled Events configuration are not a
// provider grant; ordinary archived-message reads must remain available.
func TestMCPAdmissionRequiresCurrentNativeMessagePermission(t *testing.T) {
	for _, family := range []string{"msgvault.message_archived", "msgvault.source_message_archived"} {
		t.Run(family, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f := storetest.New(t)
			config := mcpStoreConfig()
			config.Capabilities = append(config.Capabilities, store.MCPEventCapability{Family: "msgvault.source_message_archived", SourceType: "gmail", Kinds: []string{"message"}})
			_, err := f.Store.ConfigureMCPEvents(t.Context(), config)
			require.NoError(err)
			id := f.CreateMessage("synthetic-permission-archive")
			message, err := f.Store.GetMessage(id)
			require.NoError(err)
			require.NotNil(message)
			scope, id := "conversation", f.ConvID
			if family == "msgvault.source_message_archived" {
				scope, id = "source", f.Source.ID
			}
			_, _, err = f.Store.ValidateMCPEventScope(t.Context(), family, scope, id, []string{"message"})
			require.EqualError(err, "native_authority_unavailable")
			archived, err := f.Store.GetMessage(message.ID)
			require.NoError(err)
			assert.Equal(message.ID, archived.ID)
		})
	}
}
