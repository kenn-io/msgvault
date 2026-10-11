package slack

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"testing"
)

// Native channel_not_found after a successful inventory is positive evidence,
// even when the next inventory still enumerates the unreadable conversation.
func TestNativeSlackPositiveChannelReadDenialRetiresItsThread(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	native := nativeSlackInboxWorkspace(t)
	imp, opts := testImporter(t, native)
	enableNativeSlackInboxEvents(t, imp.store)
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	page, err := imp.store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: sum.SourceID})
	require.NoError(err)
	resources := map[string]store.MCPResource{}
	for _, resource := range page.Resources {
		resources[resource.ProviderID] = resource
	}
	require.Contains(resources, "CEVENTS")
	require.Contains(resources, "CEVENTS:"+ts(0))
	require.Contains(resources, "COTHER")
	native.handleGhost(native.convs[0])
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err, "native gone conversations keep the ordinary archive sync usable")
	for _, key := range []string{"CEVENTS", "CEVENTS:" + ts(0)} {
		_, lookupErr := imp.store.GetMCPResource(t.Context(), "inbox", resources[key].ID)
		require.Error(lookupErr, "native positive read denial retires %s immediately", key)
	}
	current, err := imp.store.GetMCPResource(t.Context(), "inbox", resources["COTHER"].ID)
	require.NoError(err)
	assert.Equal(resources["COTHER"].Reference, current.Reference)
	var count int
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id IN (?,?)`), sum.SourceID, "CEVENTS:"+ts(0), "CEVENTS:"+ts(1)).Scan(&count))
	assert.Equal(2, count, "retirement preserves previously archived content")
}
