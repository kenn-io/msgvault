package daemonclient

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/apiprotocol"
	apiclient "go.kenn.io/msgvault/pkg/client"
)

const MCPDiscoveryMinAPISchemaVersion = "3.15.0"
const ScopedCardDAVMinAPISchemaVersion = "3.16.0"

var ErrMCPCapabilitiesUnavailable = errors.New("MCP operation discovery unavailable")

// MCPCapabilities reads bounded caller-specific admission without following
// redirects or observing providers. Older or unknown API contracts fail closed.
func (c *Client) MCPCapabilities(ctx context.Context) (*apiprotocol.MCPCapabilities, error) {
	transport := *c.httpClient
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client, err := apiclient.New(c.baseURL,
		runtime.WithHTTPClient(mcpDiscoveryDoer{httpDoer{client: &transport, rootContext: c.requestContext()}}),
		runtime.WithRequestEditorFn(requestEditor(c.apiKey, c.agentToken, c.requestMode, c.localDaemonToken)))
	if err != nil {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	health, err := client.GetHealthWithResponse(ctx)
	if err != nil || health == nil || health.StatusCode != http.StatusOK || health.JSON200 == nil || health.JSON200.APISchemaVersion == nil {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	version := *health.JSON200.APISchemaVersion
	if !strings.HasPrefix(version, "3.") || !APISchemaVersionAtLeast(version, MCPDiscoveryMinAPISchemaVersion) {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	response, err := client.GetMCPCapabilitiesWithResponse(ctx)
	if err != nil || response == nil || response.StatusCode != http.StatusOK || response.JSON200 == nil {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	body := response.Body
	var caller struct {
		Delegated *bool `json:"delegated"`
	}
	if json.Unmarshal(body, &caller) != nil || caller.Delegated == nil {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	var descriptor apiprotocol.MCPCapabilities
	if json.Unmarshal(body, &descriptor) != nil || descriptor.Version != 1 || descriptor.Routes == nil || len(descriptor.Routes) > 256 || len(descriptor.IdentityOperations) > 64 {
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
	return &descriptor, nil
}

// Bound both the schema probe and descriptor before the generated decoder
// reads them. The isolated transport also refuses all redirects.
type mcpDiscoveryDoer struct{ httpDoer }

func (d mcpDiscoveryDoer) Do(ctx context.Context, request *http.Request) (*http.Response, error) {
	response, err := d.httpDoer.Do(ctx, request)
	if err != nil {
		return nil, err
	}
	originalBody := response.Body
	defer func() { _ = originalBody.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}
