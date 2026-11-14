package mcp

import (
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonFactCatalogOwnerCapabilitiesAndWriteGate(t *testing.T) {
	assertions := assert.New(t)
	names := []string{"list_person_fact_claims", "list_person_fact_pins", "set_person_fact_pin", "set_person_tracking"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	reads := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names[:2] {
		assertions.Contains(reads, name)
	}
	result := rawCallTool(t, opts, "list_person_fact_claims", map[string]any{"person_id": 1})
	assertions.NotEqual(true, result["isError"])
	result = rawCallTool(t, opts, "list_person_fact_claims", map[string]any{"person_id": 1, "target": ""})
	assertions.Equal(true, result["isError"])
	for _, name := range names[2:] {
		assertions.NotContains(reads, name)
	}
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyPersonFacts}
	tools := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names {
		assertions.Contains(tools, name)
	}
}

func TestPersonFactWriteHints(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"set_person_fact_pin", "set_person_tracking"}, OperationWriteFamilies: []OperationFamily{OperationFamilyPersonFacts}}
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	server, err := newMCPServer(opts, true).Connect(t.Context(), serverTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, server.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "person-fact-test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, session.Close()) })
	listed, err := session.ListTools(t.Context(), nil)
	requirements.NoError(err)
	found := map[string]bool{}
	for _, tool := range listed.Tools {
		if tool.Name != "set_person_fact_pin" && tool.Name != "set_person_tracking" {
			continue
		}
		found[tool.Name] = true
		requirements.NotNil(tool.Annotations)
		assertions.Equal(new(true), tool.Annotations.DestructiveHint, tool.Name)
		if tool.Name == "set_person_tracking" {
			assertions.Equal(new(true), tool.Annotations.OpenWorldHint)
		}
	}
	assertions.Len(found, 2)
}
