package sourcecatalog

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
)

func TestSourceCatalogImportedProvenanceDoesNotInheritLiveCapabilities(t *testing.T) {
	// A configurable importer can use a native provider's type string. Its
	// provenance must still describe an import, never a live/send adapter.
	for _, sourceType := range []string{"vendor_archive", "gmail", "teams", "matrix"} {
		t.Run(sourceType, func(t *testing.T) {
			assert := Assert.New(t)
			got := Imported(sourceType)
			assert.Equal(sourceType, got.Type)
			assert.Equal("import", got.Transport)
			assert.True(got.Imports)
			assert.False(got.LiveMessages)
			assert.False(got.Send)
			assert.False(got.Calendars)
			assert.False(got.Meetings)
		})
	}
}

func TestSourceCatalogSnapshotsCannotAlterProviderCapabilities(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	got, ok := Lookup("matrix")
	require.True(ok)
	require.NotEmpty(got.Limitations)
	got.Limitations[0] = "mutated"
	again, ok := Lookup("matrix")
	require.True(ok)
	assert.Contains(again.Limitations, "encrypted_messages_unavailable")
	assert.NotContains(again.Limitations, "mutated")

	all := List()
	require.NotEmpty(all)
	all[0].Type = "mutated"
	assert.NotEqual("mutated", List()[0].Type)
}

func TestSourceCatalogContactOnlyAndUnknownSources(t *testing.T) {
	assert := Assert.New(t)
	got, ok := Lookup("carddav")
	Require.True(t, ok)
	assert.False(got.LiveMessages)
	assert.False(got.Calendars)
	assert.False(got.Send)
	assert.False(got.Imports)
	assert.Contains(got.Limitations, "contacts_only")
	_, ok = Lookup("unregistered-provider")
	assert.False(ok)
	assert.Empty(ForCommand("sync-unregistered-provider"))
}
