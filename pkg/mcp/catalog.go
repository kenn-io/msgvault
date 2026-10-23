// Package mcp exposes native MCP tool definitions for applications that supply
// their own handlers and transport.
package mcp

import (
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	internalmcp "go.kenn.io/msgvault/internal/mcp"
)

// ReadTool returns an independent copy of a named tool from the base read-only
// catalog, including its input schema, output schema, and annotations. Schemas
// are JSON objects represented by map[string]any. Unknown names, write tools,
// and tools requiring optional backends return an error.
//
// This is a definition only: it does not open an archive, register a handler,
// validate calls, or authorize access. Callers own admission and transport; Reader provides native execution for its
// supported subset. Use the SDK's generic
// AddTool function to validate inputs and outputs against these schemas.
func ReadTool(name string) (*sdkmcp.Tool, error) {
	return internalmcp.ReadTool(name)
}
