package testutil

import (
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// MCPErrorReason returns a structured reason from an MCP JSON-RPC error.
func MCPErrorReason(err error) (string, bool) {
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) || rpcErr == nil {
		return "", false
	}
	var data struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rpcErr.Data, &data); err != nil || data.Reason == "" {
		return "", false
	}
	return data.Reason, true
}
