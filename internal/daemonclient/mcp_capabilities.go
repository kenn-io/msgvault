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

const MCPDiscoveryMinAPISchemaVersion = InboxMinAPISchemaVersion

var ErrMCPCapabilitiesUnavailable = errors.New("MCP operation discovery unavailable")

// MCPCapabilities reads current caller admission without following redirects or
// observing a provider. Version eligibility alone never admits a control call.
func (c *Client) MCPCapabilities(ctx context.Context) (*apiprotocol.MCPCapabilities, error) {
	version, err := c.APISchemaVersion(ctx)
	if err != nil || !strings.HasPrefix(version, "3.") || !APISchemaVersionAtLeast(version, MCPDiscoveryMinAPISchemaVersion) {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	transport := *c.httpClient
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := c.doGeneratedRequestWithHTTPClient(ctx, http.MethodGet, "/api/v1/mcp/capabilities", nil, &transport, 1<<20)
	if err != nil {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	const wireLimit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, wireLimit+1))
	if err != nil || len(data) > wireLimit {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	var capabilities apiprotocol.MCPCapabilities
	if json.Unmarshal(data, &capabilities) != nil || capabilities.Version != 1 || capabilities.Routes == nil || len(capabilities.Routes) > 256 || len(capabilities.InboxOperations) > 64 {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	if !validMCPCapabilities(&capabilities) {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	return &capabilities, nil
}

var mcpDescriptorIdentity = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

func validMCPCapabilities(capabilities *apiprotocol.MCPCapabilities) bool {
	routes := make(map[string]bool, len(capabilities.Routes))
	for _, route := range capabilities.Routes {
		if route.OperationID == "" || len(route.OperationID) > 256 || !mcpDescriptorIdentity.MatchString(route.OperationID) || routes[route.OperationID] || len(route.Path) > 2048 || len(route.QueryParameters)+len(route.RequestProperties) > 256 {
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
	operations := make(map[string]bool, len(capabilities.InboxOperations))
	for _, operation := range capabilities.InboxOperations {
		if operation == "" || len(operation) > 256 || operations[operation] {
			return false
		}
		operations[operation] = true
	}
	return true
}

func validMCPDescriptorNames(names []string) bool {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if name == "" || len(name) > 256 || !mcpDescriptorIdentity.MatchString(name) || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}
