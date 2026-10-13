package store_test

import (
	"fmt"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPResourceReferencesTrackNativeLifetime(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	grantSyntheticSourceFixture(t, f)
	source, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	conversation, err := f.Store.GetMCPResource(t.Context(), "conversation", f.ConvID)
	require.NoError(err)
	assert.Regexp(`^mcp1\.[A-Za-z0-9_-]{43}$`, source.Reference)
	assert.NotEqual(source.Reference, conversation.Reference)
	require.NoError(f.Store.UpdateSourceDisplayName(f.Source.ID, "Renamed synthetic account"))
	again, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	assert.Equal(source.Reference, again.Reference)
	assert.Equal("Renamed synthetic account", again.DisplayName)
	unchanged, err := f.Store.GetMCPResource(t.Context(), "conversation", f.ConvID)
	require.NoError(err)
	assert.Equal(conversation.Reference, unchanged.Reference)
	require.NoError(f.Store.UpdateSourceIdentifier(f.Source.ID, "replacement@example.net"))
	require.NoError(f.Store.UpdateSourceIdentifier(f.Source.ID, f.Source.Identifier))
	replacement, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	assert.NotEqual(source.Reference, replacement.Reference)
	rebound, err := f.Store.GetMCPResource(t.Context(), "conversation", f.ConvID)
	require.NoError(err)
	assert.NotEqual(conversation.Reference, rebound.Reference)
	_, err = f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	grantSyntheticSourceFixture(t, f)
	_, _, err = f.Store.ResolveMCPSubscriptionResource(t.Context(), "msgvault.message_archived", "conversation", f.ConvID, nil, conversation.Reference)
	require.EqualError(err, "resource_unavailable")
	_, _, err = f.Store.ResolveMCPSubscriptionResource(t.Context(), "msgvault.message_archived", "conversation", f.ConvID, nil, source.Reference)
	require.EqualError(err, "resource_unavailable")
	_, binding, err := f.Store.ResolveMCPSubscriptionResource(t.Context(), "msgvault.message_archived", "conversation", f.ConvID, nil, rebound.Reference)
	require.NoError(err)
	require.NotNil(binding)
	assert.NoError(f.Store.CheckMCPResourceBinding(t.Context(), binding))
}

func TestMCPResourcePagesAuthenticateOwnerAndFilters(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	grantSyntheticSourceFixture(t, f)
	for i := range 4 {
		_, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("synthetic-page-%d", i), "Synthetic topic")
		require.NoError(err)
	}
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.net")
	require.NoError(err)
	_, err = f.Store.EnsureConversation(other.ID, "synthetic-page-0", "Other synthetic topic")
	require.NoError(err)
	opts := store.MCPResourcePageOptions{Principal: "owner:synthetic", Kind: "conversation", SourceID: f.Source.ID, Limit: 2}
	first, err := f.Store.ListMCPResourcePage(t.Context(), opts)
	require.NoError(err)
	require.Len(first.Resources, 2)
	require.NotEmpty(first.NextCursor)
	for _, resource := range first.Resources {
		assert.Equal(f.Source.ID, resource.SourceID)
	}
	for _, tc := range []struct {
		name   string
		change func(*store.MCPResourcePageOptions)
	}{
		{"owner", func(o *store.MCPResourcePageOptions) { o.Principal = "owner:other" }},
		{"kind", func(o *store.MCPResourcePageOptions) { o.Kind = "source" }},
		{"source", func(o *store.MCPResourcePageOptions) { o.SourceID = other.ID }},
		{"limit", func(o *store.MCPResourcePageOptions) { o.Limit = 3 }},
		{"bytes", func(o *store.MCPResourcePageOptions) { o.Cursor += "A" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := opts
			bad.Cursor = first.NextCursor
			tc.change(&bad)
			_, err := f.Store.ListMCPResourcePage(t.Context(), bad)
			Require.EqualError(t, err, "invalid_discovery_cursor")
		})
	}
	seen := []int64{first.Resources[0].ID, first.Resources[1].ID}
	opts.Cursor = first.NextCursor
	for opts.Cursor != "" {
		page, err := f.Store.ListMCPResourcePage(t.Context(), opts)
		require.NoError(err)
		require.LessOrEqual(len(page.Resources), 2)
		for _, resource := range page.Resources {
			assert.NotContains(seen, resource.ID)
			seen = append(seen, resource.ID)
		}
		opts.Cursor = page.NextCursor
	}
	assert.Len(seen, 5)
	require.NoError(f.Store.UpdateSourceDisplayName(f.Source.ID, "Renamed synthetic account"))
	opts.Cursor = first.NextCursor
	_, err = f.Store.ListMCPResourcePage(t.Context(), opts)
	require.NoError(err, "display-only changes preserve page anchors")
	require.NoError(f.Store.UpdateSourceIdentifier(f.Source.ID, "replacement@example.net"))
	require.NoError(f.Store.UpdateSourceIdentifier(f.Source.ID, f.Source.Identifier))
	_, err = f.Store.ListMCPResourcePage(t.Context(), opts)
	assert.EqualError(err, "invalid_discovery_cursor", "rebound anchors require restarting discovery")
}

func TestMCPResourceDiscoveryRejectsInvalidBounds(t *testing.T) {
	f := storetest.New(t)
	grantSyntheticSourceFixture(t, f)
	for _, tc := range []store.MCPResourcePageOptions{
		{Kind: "source", Limit: 1},
		{Principal: "owner:synthetic", Kind: "unknown", Limit: 1},
		{Principal: "owner:synthetic", Kind: "source", Limit: -1},
		{Principal: "owner:synthetic", Kind: "source", Limit: 101},
		{Principal: "owner:synthetic", Kind: "conversation", SourceID: -1, Limit: 1},
	} {
		_, err := f.Store.ListMCPResourcePage(t.Context(), tc)
		Require.Error(t, err)
	}
	for _, kind := range []string{"", "inbox", "unknown"} {
		_, err := f.Store.GetMCPResource(t.Context(), kind, f.Source.ID)
		Require.Error(t, err)
	}
}
