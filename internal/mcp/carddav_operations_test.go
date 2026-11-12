package mcp

import (
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCardDAVOperationalCatalogWriteGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	names := []string{"list_carddav_connections", "list_carddav_books", "list_carddav_runs", "list_carddav_conflicts", "get_carddav_conflict", "update_carddav_book_roles", "resolve_carddav_conflict", "unpublish_carddav_person"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	read := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names[:5] {
		assert.Contains(read, name)
	}
	for _, name := range names[5:] {
		assert.NotContains(read, name)
	}
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyCardDAV}
	write := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names {
		schema, ok := write[name]["inputSchema"].(map[string]any)
		require.True(ok)
		assert.Equal(false, schema["additionalProperties"])
	}
	assert.Equal([]string{"before_id", "connection", "limit"}, toolPropertyNames(t, write["list_carddav_runs"]))
	assert.Equal([]string{"book_id", "lookup_source", "subscribed", "write_target"}, toolPropertyNames(t, write["update_carddav_book_roles"]))
	for _, tc := range []struct {
		name      string
		openWorld bool
	}{
		{"update_carddav_book_roles", false},
		{"resolve_carddav_conflict", true},
	} {
		annotations, ok := write[tc.name]["annotations"].(map[string]any)
		require.True(ok)
		assert.Equal(true, annotations["destructiveHint"])
		assert.Equal(tc.openWorld, annotations["openWorldHint"])
	}
	encoded, err := json.Marshal(write["resolve_carddav_conflict"]["inputSchema"])
	require.NoError(err)
	var inputSchema jsonschema.Schema
	require.NoError(json.Unmarshal(encoded, &inputSchema))
	resolved, err := inputSchema.Resolve(nil)
	require.NoError(err)
	assert.Error(resolved.Validate(map[string]any{"conflict_id": float64(1), "choice": "keep_both"}))
	assert.NoError(resolved.Validate(map[string]any{"conflict_id": float64(1), "choice": "keep_local"}))
	for _, name := range names[5:] {
		require.Contains(write, name)
		readOnly, ok := toolReadOnlyHint(t, write[name]).(bool)
		require.True(ok)
		assert.False(readOnly)
	}
	for _, name := range names[5:] {
		assert.NotContains(toolsByName(t, rawListTools(t, opts, false)), name)
	}
	opts.DelegatedOnly = true
	delegated := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names {
		assert.NotContains(delegated, name)
	}
}

func TestCardDAVSelectorRefusesLegacyBackend(t *testing.T) {
	assert := assert.New(t)
	backend := newIdentityReviewMCPBackend(t)
	opts := ServeOptions{PersonCardDAV: backend, AllowCardDAVWrites: true}
	for _, name := range []string{ToolSyncCardDAV, ToolGetCardDAVSyncStatus} {
		result := rawCallTool(t, opts, name, map[string]any{"connection": "work"})
		assert.Equal(true, result["isError"])
		assert.Contains(fmt.Sprint(result), "named CardDAV connection is not supported")
	}
}
