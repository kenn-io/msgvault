package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDocumentCatalogRequiresFamilyGateAndOwner(t *testing.T) {
	assertions := assert.New(t)
	reads := []string{"get_document_index_status", "get_document_processing_policy"}
	writes := []string{"consent_document_processing", "build_document_index", "resume_document_index", "retry_document_extraction"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: append(reads, writes...), OperationWriteFamilies: []OperationFamily{OperationFamilyDocuments}}
	tools := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range opts.OperationCapabilities {
		assertions.Contains(tools, name)
	}
	for _, name := range reads {
		assertions.Contains(toolsByName(t, rawListTools(t, opts, false)), name)
	}
	for _, name := range writes {
		assertions.NotContains(toolsByName(t, rawListTools(t, opts, false)), name)
	}
	opts.DelegatedOnly = true
	assertions.Empty(operationalCatalog(opts, true))
}
