package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPeopleProviderCatalogGates(t *testing.T) {
	a := assert.New(t)
	names := []string{"get_people_provider_settings", "select_people_provider", "disable_people_inference"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	a.Len(operationalCatalog(opts, true), 1)
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyProviders}
	a.Len(operationalCatalog(opts, true), 3)
	a.Len(operationalCatalog(opts, false), 1)
	opts.DelegatedOnly = true
	a.Empty(operationalCatalog(opts, true))
}
