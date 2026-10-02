package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCardDAVOperationalCatalogHasIndependentGate(t *testing.T) {
	assertions := assert.New(t)
	reads := []string{"list_carddav_connections", "list_carddav_books", "list_carddav_runs", "get_carddav_connection_status", "list_carddav_conflicts", "get_carddav_conflict"}
	writes := []string{"sync_carddav_connections", "update_carddav_book_roles", "resolve_carddav_conflict", "unpublish_carddav_person"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: append(reads, writes...), OperationWriteFamilies: []OperationFamily{OperationFamilyCardDAV}}
	tools := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range opts.OperationCapabilities {
		assertions.Contains(tools, name)
	}
	for _, name := range writes {
		assertions.NotContains(toolsByName(t, rawListTools(t, opts, false)), name)
	}
	assertions.NotContains(tools, "publish_carddav_person")
	assertions.NotContains(tools, "save_carddav_account")
	opts.DelegatedOnly = true
	assertions.Empty(operationalCatalog(opts, true))
}
