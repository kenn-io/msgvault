package daemonclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"go.kenn.io/msgvault/internal/apiprotocol"
)

// MCPCapabilities reads implementation metadata with this client's existing
// authentication. Missing, unknown or malformed discovery is an error; callers
// must hide optional tools rather than guess support from an API version.
func (c *Client) MCPCapabilities(ctx context.Context) (*apiprotocol.MCPCapabilities, error) {
	response, err := c.DoGeneratedRequestWithContext(ctx, http.MethodGet, "/api/v1/mcp/capabilities", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("MCP capability discovery unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, mcpCLIWireLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > mcpCLIWireLimit {
		return nil, errors.New("MCP capability discovery exceeds byte limit")
	}
	var capabilities apiprotocol.MCPCapabilities
	if err := json.Unmarshal(data, &capabilities); err != nil {
		return nil, errors.New("invalid MCP capability discovery")
	}
	if capabilities.Version != 1 || capabilities.Delegated != (c.agentToken != "") {
		return nil, errors.New("unsupported MCP capability discovery")
	}
	if !validMCPCapabilities(&capabilities) {
		return nil, errors.New("invalid MCP capability descriptors")
	}
	return &capabilities, nil
}

var mcpDescriptorIdentity = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
var mcpCommandIdentity = regexp.MustCompile(`^[a-z][a-z0-9-]*( [a-z][a-z0-9-]*)*$`)

func validMCPCapabilities(capabilities *apiprotocol.MCPCapabilities) bool {
	if capabilities.Delegated && len(capabilities.Routes) != 0 {
		return false
	}
	routes := make(map[string]bool)
	for _, route := range capabilities.Routes {
		if !mcpDescriptorIdentity.MatchString(route.OperationID) || routes[route.OperationID] {
			return false
		}
		routes[route.OperationID] = true
		switch route.Method {
		case http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		default:
			return false
		}
		if !strings.HasPrefix(route.Path, "/api/v1/") || strings.ContainsAny(route.Path, "?#\\ ") || strings.Contains(route.Path, "/../") {
			return false
		}
		if !validMCPDescriptorNames(route.QueryParameters) || !validMCPDescriptorNames(route.RequestProperties) {
			return false
		}
	}
	commands := make(map[string]bool)
	for _, command := range capabilities.Commands {
		if !mcpCommandIdentity.MatchString(command.Name) || commands[command.Name] || (capabilities.Delegated && !command.Delegated) {
			return false
		}
		commands[command.Name] = true
		if !validMCPDescriptorNames(command.Flags) {
			return false
		}
	}
	return true
}

func validMCPDescriptorNames(names []string) bool {
	seen := make(map[string]bool)
	for _, name := range names {
		if !mcpDescriptorIdentity.MatchString(name) || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}
