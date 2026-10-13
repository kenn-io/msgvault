package mcpevents

import (
	"path/filepath"
	"strconv"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available inside subtests.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available inside subtests.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Exact import admission works before an import run exists and does not derive
// live authority from the archive's source label. Discovery must agree.
func TestImportSourceDiscoveryDoesNotRequireLiveProviderAuthority(t *testing.T) {
	for _, sourceType := range []string{"gmail", "synthetic_offline_archive", "carddav"} {
		for _, enabled := range []bool{false, true} {
			t.Run(sourceType+"/"+strconv.FormatBool(enabled), func(t *testing.T) {
				assert := Assert.New(t)
				require := Require.New(t)
				f := storetest.New(t)
				source, err := f.Store.GetOrCreateSource(sourceType, "synthetic-import@example.net")
				require.NoError(err)
				svc, err := New(t.Context(), f.Store, Options{
					Enabled: true, ImportNotifications: enabled, Sources: []string{"gmail"},
					OwnerKey: "synthetic-import-discovery-owner", KeyPath: filepath.Join(t.TempDir(), "events.key"),
				})
				require.NoError(err)
				page, err := svc.DiscoverResources(t.Context(), svc.principal, ResourceQuery{
					Kind: "source", SourceID: strconv.FormatInt(source.ID, 10),
				})
				require.NoError(err)
				require.Len(page.Resources, 1)
				resource := page.Resources[0]
				assert.NotEmpty(resource.Reference)
				if enabled {
					assert.Contains(resource.Capabilities.Read, "get_message")
					assert.Equal([]string{"msgvault.import_message_archived"}, resource.Capabilities.Events)
					assert.Empty(resource.Capabilities.UnavailableReason)
					_, id, err := f.Store.ValidateMCPEventScope(t.Context(), "msgvault.import_message_archived", "source", source.ID, []string{"message"})
					require.NoError(err)
					assert.Equal(source.ID, id)
					if sourceType == "gmail" {
						require.NoError(f.Store.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{
							SourceID: source.ID, SourceReference: resource.Reference, SourceType: sourceType,
							NativeIdentity: source.Identifier, Known: true, MessageRead: true,
						}))
						page, err = svc.DiscoverResources(t.Context(), svc.principal, ResourceQuery{Kind: "source", SourceID: strconv.FormatInt(source.ID, 10)})
						require.NoError(err)
						require.Len(page.Resources, 1)
						assert.ElementsMatch([]string{importFamily, sourceMessageFamily}, page.Resources[0].Capabilities.Events, "import and native source families coexist after native permission is granted")
						assert.Empty(page.Resources[0].Capabilities.UnavailableReason)
					}
				} else {
					assert.Empty(resource.Capabilities.Events)
				}
			})
		}
	}
}
