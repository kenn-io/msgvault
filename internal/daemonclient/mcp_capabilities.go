package daemonclient

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"

	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// MCPCapabilities reads current caller admission, without observing providers
// or treating a schema version as proof that an operation is implemented.
func (c *Client) MCPCapabilities(ctx context.Context) (*apiprotocol.MCPCapabilities, error) {
	compatible, err := c.SupportsAPISchemaVersion(ctx, InboxMinAPISchemaVersion)
	if err != nil || !compatible {
		return nil, inboxcontrol.ErrUnavailable
	}
	transport := *c.httpClient
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := c.doGeneratedRequestWithHTTPClient(ctx, http.MethodGet, "/api/v1/mcp/capabilities", nil, &transport)
	if err != nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, inboxcontrol.ErrUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return nil, inboxcontrol.ErrUnavailable
	}
	var descriptor apiprotocol.MCPCapabilities
	if json.Unmarshal(body, &descriptor) != nil || descriptor.Version != 1 || len(descriptor.Routes) > 256 || len(descriptor.Commands) > 128 || len(descriptor.InboxOperations) > 32 {
		return nil, inboxcontrol.ErrUnavailable
	}
	return &descriptor, nil
}
