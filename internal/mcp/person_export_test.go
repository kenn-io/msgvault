package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

type personExportTestBackend struct{}

func (personExportTestBackend) ExecuteOperation(context.Context, string, map[string]any) (*OperationResult, error) {
	return &OperationResult{Output: PersonExport{JSONL: "synthetic\n"}}, nil
}
func (personExportTestBackend) OperationDisclosure(context.Context, string, map[string]any) (string, error) {
	return "", nil
}

func TestPersonExportReadWithoutWriteOptIn(t *testing.T) {
	assertions := assert.New(t)
	opts := ServeOptions{Operations: personExportTestBackend{}, OperationCapabilities: []string{"export_person_messages"}}
	tools := toolsByName(t, rawListTools(t, opts, false))
	assertions.Contains(tools, "export_person_messages")
	response := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{
		"name": "export_person_messages",
		"arguments": map[string]any{
			"person_id": 1,
			"start":     "2026-07-20T00:00:00Z",
			"end":       "2026-07-21T00:00:00Z",
		},
	})
	assertions.Equal(map[string]any{"jsonl": "synthetic\n"}, response.Result["structuredContent"])
}
