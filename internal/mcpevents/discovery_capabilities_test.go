package mcpevents

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcecatalog"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPResourceDiscoveryReadToolsMatchResourceFamilies(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	calendar, err := f.Store.GetOrCreateSource(sourcecatalog.GoogleCalendar, "synthetic-calendar@example.test/primary")
	require.NoError(err)
	service, err := New(t.Context(), f.Store, Options{
		Enabled:  true,
		Sources:  []string{sourcecatalog.Gmail, sourcecatalog.GoogleCalendar},
		OwnerKey: "synthetic-resource-owner",
		KeyPath:  filepath.Join(t.TempDir(), "events.key"),
	})
	require.NoError(err)

	gmailResource, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	require.NoError(f.Store.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{
		SourceID:        f.Source.ID,
		SourceReference: gmailResource.Reference,
		SourceType:      sourcecatalog.Gmail,
		NativeIdentity:  f.Source.Identifier,
		Known:           true,
		MessageRead:     true,
	}))

	calendarPage, err := service.DiscoverResources(t.Context(), service.principal, ResourceQuery{
		Kind:     "source",
		SourceID: strconv.FormatInt(calendar.ID, 10),
	})
	require.NoError(err)
	require.Len(calendarPage.Resources, 1)
	assert.Equal([]string{calendarFamily}, calendarPage.Resources[0].Capabilities.Events)
	assert.Equal([]string{"get_message"}, calendarPage.Resources[0].Capabilities.Read)

	conversationPage, err := service.DiscoverResources(t.Context(), service.principal, ResourceQuery{
		Kind:     "conversation",
		SourceID: strconv.FormatInt(f.Source.ID, 10),
	})
	require.NoError(err)
	require.NotEmpty(conversationPage.Resources)
	assert.Equal([]string{"get_message", "list_thread", "get_attachment", "draft_get"}, conversationPage.Resources[0].Capabilities.Read)
}

func TestMCPResourceDiscoveryUnknownScopeIsNotNativeAuthorityFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	service, err := New(t.Context(), f.Store, Options{
		Enabled:  true,
		Sources:  []string{sourcecatalog.GoogleCalendar},
		OwnerKey: "synthetic-resource-owner",
		KeyPath:  filepath.Join(t.TempDir(), "events.key"),
	})
	require.NoError(err)

	capabilities, err := service.resourceCapabilities(t.Context(), store.MCPResource{
		Kind:       "source",
		ID:         999999,
		SourceID:   999999,
		SourceType: sourcecatalog.GoogleCalendar,
	})
	require.NoError(err)
	assert.Equal("scope_not_supported", capabilities.UnavailableReason)
}

func TestMCPResourceCapabilitiesOmitFamilyWhenNativeAuthorityValidationFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	primaryResource, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	require.NoError(f.Store.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{
		SourceID:        f.Source.ID,
		SourceReference: primaryResource.Reference,
		SourceType:      sourcecatalog.Gmail,
		NativeIdentity:  f.Source.Identifier,
		Known:           true,
		MessageRead:     true,
	}))

	otherSource, err := f.Store.GetOrCreateSource(sourcecatalog.Gmail, "secondary@example.test")
	require.NoError(err)
	otherConversation, err := f.Store.EnsureConversation(otherSource.ID, "synthetic-thread", "Synthetic thread")
	require.NoError(err)
	service, err := New(t.Context(), f.Store, Options{
		Enabled:  true,
		Sources:  []string{sourcecatalog.Gmail},
		OwnerKey: "synthetic-resource-owner",
		KeyPath:  filepath.Join(t.TempDir(), "events.key"),
	})
	require.NoError(err)
	service.caps = []store.MCPEventCapability{{Family: messageFamily, SourceType: sourcecatalog.Gmail, Kinds: []string{"message"}}}

	capabilities, err := service.resourceCapabilities(t.Context(), store.MCPResource{
		Kind:       "conversation",
		ID:         otherConversation,
		SourceID:   f.Source.ID,
		SourceType: sourcecatalog.Gmail,
	})
	require.NoError(err)
	assert.Empty(capabilities.Events)
	assert.Equal("native_authority_unavailable", capabilities.UnavailableReason)
}
