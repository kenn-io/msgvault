package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProviderCatalogHasSeparateWriteGateAndNoDelegation(t *testing.T) {
	assertions := assert.New(t)
	reads := []string{"get_people_provider_settings", "get_people_provider_status", "list_people_provider_history"}
	writes := []string{"create_people_provider_preset", "check_people_provider", "consent_people_provider", "select_people_provider", "revoke_people_provider_consent", "disable_people_inference", "update_people_provider_policy", "remove_people_provider"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: append(reads, writes...), OperationWriteFamilies: []OperationFamily{OperationFamilyProviders}}
	tools := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range opts.OperationCapabilities {
		assertions.Contains(tools, name)
	}
	read := toolsByName(t, rawListTools(t, opts, false))
	for _, name := range reads {
		assertions.Contains(read, name)
	}
	for _, name := range writes {
		assertions.NotContains(read, name)
	}
	opts.DelegatedOnly = true
	assertions.Empty(operationalCatalog(opts, true))
}
