package testutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// SetMCPEventNameHeader adds the name header required by msgvault's custom
// Events methods. The Go MCP SDK sends standard name headers, but does not
// know that these custom methods use the same extension.
func SetMCPEventNameHeader(request *http.Request) error {
	if request == nil {
		return errors.New("MCP request is nil")
	}
	switch request.Header.Get("Mcp-Method") {
	case "events/subscribe", "events/unsubscribe", "events/poll", "events/stream":
	default:
		return nil
	}
	if request.Body == nil {
		return errors.New("MCP Events request body is nil")
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return fmt.Errorf("read MCP Events request body: %w", err)
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	var message struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &message); err != nil {
		return fmt.Errorf("decode MCP Events request: %w", err)
	}
	method := request.Header.Get("Mcp-Method")
	if message.Method != method {
		return fmt.Errorf("MCP method header %q does not match request method %q", method, message.Method)
	}
	if message.Params.Name == "" {
		return errors.New("MCP Events request has no params.name")
	}
	request.Header.Set("Mcp-Name", message.Params.Name)
	return nil
}
