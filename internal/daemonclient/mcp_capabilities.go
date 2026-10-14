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
	const wireLimit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, wireLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > wireLimit {
		return nil, errors.New("MCP capability discovery exceeds byte limit")
	}
	var capabilities apiprotocol.MCPCapabilities
	if err := json.Unmarshal(data, &capabilities); err != nil {
		return nil, errors.New("invalid MCP capability discovery")
	}
	if capabilities.Version != 1 {
		return nil, errors.New("unsupported MCP capability discovery")
	}
	if !validMCPCapabilities(&capabilities) {
		return nil, errors.New("invalid MCP capability descriptors")
	}
	return &capabilities, nil
}

var mcpDescriptorIdentity = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

func validMCPCapabilities(capabilities *apiprotocol.MCPCapabilities) bool {
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
