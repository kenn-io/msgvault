package daemonclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
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
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	var descriptor apiprotocol.MCPCapabilities
	if json.Unmarshal(body, &descriptor) != nil || descriptor.Version != 1 || descriptor.Routes == nil || len(descriptor.Routes) > 256 || len(descriptor.InboxOperations) > 64 {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	seen := make(map[string]bool, len(descriptor.Routes))
	for _, route := range descriptor.Routes {
		if route.OperationID == "" || len(route.OperationID) > 256 || seen[route.OperationID] || len(route.Path) > 2048 || !strings.HasPrefix(route.Path, "/api/v1/") || len(route.QueryParameters)+len(route.RequestProperties) > 256 {
			return nil, ErrMCPCapabilitiesUnavailable
		}
		switch route.Method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			return nil, ErrMCPCapabilitiesUnavailable
		}
		seen[route.OperationID] = true
		for _, fields := range [][]string{route.QueryParameters, route.RequestProperties} {
			fieldSet := map[string]bool{}
			for _, field := range fields {
				if field == "" || len(field) > 256 || fieldSet[field] {
					return nil, ErrMCPCapabilitiesUnavailable
				}
				fieldSet[field] = true
			}
		}
	}
	seen = map[string]bool{}
	for _, operation := range descriptor.InboxOperations {
		if operation == "" || len(operation) > 256 || seen[operation] {
			return nil, ErrMCPCapabilitiesUnavailable
		}
		seen[operation] = true
	}
	return &descriptor, nil
}
