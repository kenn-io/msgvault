package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCardDAVOperationalCatalogUsesExistingWriteGate(t *testing.T) {
	names := []string{"list_carddav_connections", "list_carddav_books", "list_carddav_runs", "list_carddav_conflicts", "get_carddav_conflict", "update_carddav_book_roles", "resolve_carddav_conflict", "unpublish_carddav_person"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	read := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names[:5] {
		assert.Contains(t, read, name)
	}
	for _, name := range names[5:] {
		assert.NotContains(t, read, name)
	}
	opts.AllowCardDAVWrites = true
	write := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names {
		schema, ok := write[name]["inputSchema"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, false, schema["additionalProperties"])
	}
	assert.Equal(t, []string{"before_id", "connection", "limit"}, toolPropertyNames(t, write["list_carddav_runs"]))
	assert.Equal(t, []string{"book_id", "lookup_source", "subscribed", "write_target"}, toolPropertyNames(t, write["update_carddav_book_roles"]))
	for _, name := range names[5:] {
		require := require.New(t)
		require.Contains(write, name)
		readOnly, ok := toolReadOnlyHint(t, write[name]).(bool)
		require.True(ok)
		assert.False(t, readOnly)
	}
	for _, name := range names[5:] {
		assert.NotContains(t, toolsByName(t, rawListTools(t, opts, false)), name)
	}
	opts.DelegatedOnly = true
	assert.Empty(t, operationalCatalog(opts, true))
	opts.DelegatedOnly = false
	opts.Families = &ArchiveFamilies{}
	assert := assert.New(t)
	assert.Empty(operationalCatalog(opts, true))
}

func TestCardDAVSelectorRefusesLegacyBackend(t *testing.T) {
	backend := newIdentityReviewMCPBackend(t)
	_, _, err := cardDAVSelector(map[string]any{"connection": "work"}, backend, true)
	require.Error(t, err)
	_, _, err = cardDAVSelector(map[string]any{"connection": "work"}, backend, false)
	require.Error(t, err)
	_, _, err = cardDAVSelector(map[string]any{"connection": "../bad"}, backend, true)
	require.Error(t, err)
	connection, _, err := cardDAVSelector(nil, backend, true)
	require := require.New(t)
	require.NoError(err)
	assert.Nil(t, connection)
}
