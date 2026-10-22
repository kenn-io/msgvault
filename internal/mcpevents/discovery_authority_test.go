package mcpevents

import (
	"path/filepath"
	"strconv"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPResourceDiscoveryKeepsDraftsVisibleWithoutNativeMessageAuthority(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	f := storetest.New(t)
	// The fixture has archive rows, but no authenticated native account or
	// observed permission evidence. Its source-type label is not live authority.
	service, err := New(t.Context(), f.Store, Options{Enabled: true, Sources: []string{"gmail"}, OwnerKey: "synthetic-resource-owner", KeyPath: filepath.Join(t.TempDir(), "events.key")})
	require.NoError(err)
	page, err := service.DiscoverResources(t.Context(), service.principal, ResourceQuery{Kind: "conversation", SourceID: strconv.FormatInt(f.Source.ID, 10)})
	require.NoError(err)
	require.NotEmpty(page.Resources)
	for _, resource := range page.Resources {
		assert.Equal([]string{draftFamily}, resource.Capabilities.Events)
		assert.Empty(resource.Capabilities.UnavailableReason)
		assert.NotEmpty(resource.Capabilities.Read)
	}
}
