package mcp

import (
	"encoding/json/v2"
	"fmt"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ReadTool supplies the public catalog API without exposing native handlers or
// pointers into the immutable schemas shared by standalone servers.
func ReadTool(name string) (*sdkmcp.Tool, error) {
	for _, definition := range stableOperationCatalogs.get(catalogCapabilities{}) {
		if definition.name != name || definition.security != toolSecurityRead {
			continue
		}
		encoded, err := json.Marshal(definition.tool())
		if err != nil {
			return nil, fmt.Errorf("encode read tool %q: %w", name, err)
		}
		var tool sdkmcp.Tool
		if err := json.Unmarshal(encoded, &tool); err != nil {
			return nil, fmt.Errorf("copy read tool %q: %w", name, err)
		}
		return &tool, nil
	}
	return nil, fmt.Errorf("read tool %q is unavailable in the base catalog", name)
}
