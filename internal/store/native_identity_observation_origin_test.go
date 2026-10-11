package store_test

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Constructors remain available in subtests.
	Require "github.com/stretchr/testify/require" //nolint:importas // Constructors remain available in subtests.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

// Archive/resource APIs predate native inventories. An identity-only first
// observation may pause read admission, but cannot prove native revocation.
// Full old-schema fixture adoption is tested independently.
func TestNativeIdentityOnlyObservationPreservesExistingConversationOrigin(t *testing.T) {
	for _, tc := range []struct{ source, account, kind, provider string }{
		{"discord", "200", "discord_channel", "300"},
		{"teams", "owner@example.net", "teams_chat", "synthetic-chat"},
		{"matrix", "@archive:example.org", "matrix_room", "!room:example.org"},
		{"beeper", "synthetic-login", "beeper_member", "!member:example.org"},
	} {
		t.Run(tc.source, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(tc.source, tc.account)
			require.NoError(err)
			conversation, err := st.ApplyConversationSnapshotContext(t.Context(), source.ID, &store.ConversationPersistData{SourceConversationID: tc.provider, ConversationType: "group_chat"})
			require.NoError(err)
			original, err := st.GetMCPResource(t.Context(), "conversation", conversation)
			require.NoError(err)
			sourceResource, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: source.ID, SourceReference: sourceResource.Reference, SourceType: tc.source, NativeIdentity: tc.account, Known: true, MessageRead: true}))
			parent := ""
			if tc.source == "beeper" {
				parent = tc.account
			}
			observation := store.NativeMessageScopeInventory{SourceID: source.ID, SourceReference: sourceResource.Reference, SourceType: tc.source, NativeIdentity: tc.account, Kind: tc.kind, ParentProviderID: parent, Complete: true, IdentityOnly: true, Scopes: []store.NativeMessageScope{{ProviderID: tc.provider, DisplayName: "Synthetic container"}}}
			require.NoError(st.ObserveNativeMessageScopes(t.Context(), observation))
			unknown, err := st.GetMCPResource(t.Context(), "conversation", conversation)
			require.NoError(err)
			assert.Equal(original.Reference, unknown.Reference, "first metadata alone does not revoke the existing origin")
			observation.IdentityOnly = false
			require.NoError(st.ObserveNativeMessageScopes(t.Context(), observation))
			verified, err := st.GetMCPResource(t.Context(), "conversation", conversation)
			require.NoError(err)
			assert.Equal(original.Reference, verified.Reference, "fresh verified content can resume the same origin after an unknown pause")
			observation.Complete = false
			observation.Scopes = nil
			observation.RetiredProviderIDs = []string{tc.provider}
			require.NoError(st.ObserveNativeMessageScopes(t.Context(), observation))
			denied, err := st.GetMCPResource(t.Context(), "conversation", conversation)
			require.NoError(err)
			assert.NotEqual(original.Reference, denied.Reference, "positive native denial revokes the old origin")
			observation.RetiredProviderIDs = nil
			observation.Scopes = []store.NativeMessageScope{{ProviderID: tc.provider, DisplayName: "Restored synthetic container"}}
			require.NoError(st.ObserveNativeMessageScopes(t.Context(), observation))
			restored, err := st.GetMCPResource(t.Context(), "conversation", conversation)
			require.NoError(err)
			assert.NotEqual(original.Reference, restored.Reference, "restoration cannot revive the pre-denial origin")
		})
	}
}
