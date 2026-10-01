package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonMessageExportCatalogIsReadOnlyOwnerScoped(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"export_person_messages"}}
	tools := toolsByName(t, rawListTools(t, opts, false))
	requirements.Contains(tools, "export_person_messages")
	assertions.Equal("integer", toolInputProperty(t, tools["export_person_messages"], "person_id")["type"])
	assertions.Equal("string", toolInputProperty(t, tools["export_person_messages"], "start")["type"])
	assertions.Equal("string", toolInputProperty(t, tools["export_person_messages"], "end")["type"])
	opts.DelegatedOnly = true
	delegated := toolsByName(t, rawListTools(t, opts, false))
	assertions.NotContains(delegated, "export_person_messages")
}
