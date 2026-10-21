package teams

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Local helpers leave callbacks bound to their test.
	Require "github.com/stretchr/testify/require" //nolint:importas // Local helpers leave callbacks bound to their test.
	"go.kenn.io/msgvault/internal/mcpevents"
	"path/filepath"
	"strconv"
	"testing"
)

// Resource discovery keeps provider-backed message events unavailable when
// grant evidence is unknown while retaining local draft capabilities.
func TestNativeTeamsUnknownAuthorityDiscoveryRemainsAvailable(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	_, imp, opts := newNativeTeamsInboxFeed(t)
	imp.client.grantedScopes = nil
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	const owner = "synthetic-teams-discovery-owner"
	service, err := mcpevents.New(t.Context(), imp.store, mcpevents.Options{Enabled: true, Sources: []string{"teams"}, OwnerKey: owner, KeyPath: filepath.Join(t.TempDir(), "events.key")})
	require.NoError(err)
	for _, kind := range []string{"source", "conversation"} {
		page, err := service.DiscoverResources(t.Context(), mcpevents.Principal(owner), mcpevents.ResourceQuery{Kind: kind, SourceID: strconv.FormatInt(sum.SourceID, 10)})
		require.NoError(err)
		require.NotEmpty(page.Resources)
		for _, resource := range page.Resources {
			assert.NotContains(resource.Capabilities.Events, "msgvault.message_archived")
			if kind == "source" {
				assert.Empty(resource.Capabilities.Events)
				assert.Equal("native_authority_unavailable", resource.Capabilities.UnavailableReason)
				continue
			}
			assert.Contains(resource.Capabilities.Events, "msgvault.draft_changed")
			assert.Empty(resource.Capabilities.UnavailableReason)
		}
	}
}
