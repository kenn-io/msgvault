package daemonclient

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"strconv"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// MCPEventsOwnerCredential reports whether the client carries a daemon owner
// credential. Runtime admission is still proved by the owner-only list route.
func (c *Client) MCPEventsOwnerCredential() bool {
	return c != nil && c.apiKey != "" && c.agentToken == ""
}

func (c *Client) mcpEventsRequest(ctx context.Context, method, path string, options runtime.RequestOptions, out any) error {
	resp, err := c.DoGeneratedRequestWithContext(ctx, method, path, options)
	if err != nil {
		return &mcpevents.Error{Code: -32015, Reason: "events_unavailable"}
	}
	defer func() { _ = resp.Body.Close() }()
	// Never expose request URLs, response bodies or driver diagnostics. The
	// error body admits only a fixed code/reason pair from the service boundary.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return &mcpevents.Error{Code: -32012, Reason: "owner_required"}
		}
		var wire mcpevents.Error
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
		if readErr == nil && len(data) <= 4096 && json.Unmarshal(data, &wire) == nil {
			if safe, ok := mcpevents.SafeError(wire.Code, wire.Reason); ok {
				return safe
			}
		}
		return &mcpevents.Error{Code: -32015, Reason: "events_unavailable"}
	}
	if out == nil {
		return nil
	}
	const responseLimit = 8 << 20
	reader := io.LimitReader(resp.Body, responseLimit+1)
	_, messageResponse := out.(*generated.GetCLIMessageResponse)
	if messageResponse {
		// Full archive details are paged by the MCP tool, as with the existing CLI message route.
		reader = resp.Body
	}
	data, err := io.ReadAll(reader)
	if err != nil || (!messageResponse && len(data) > responseLimit) || json.Unmarshal(data, out) != nil {
		return &mcpevents.Error{Code: -32015, Reason: "events_unavailable"}
	}
	return nil
}
func (c *Client) MCPEventsList(ctx context.Context) (mcpevents.ListResult, error) {
	var out mcpevents.ListResult
	err := c.mcpEventsRequest(ctx, http.MethodPost, "/api/v1/mcp/events/list", &generated.ListMCPEventsRequestOptions{Body: &generated.ListMCPEventsBody{}}, &out)
	return out, err
}
func (c *Client) MCPEventsSubscribe(ctx context.Context, req mcpevents.SubscribeRequest) (mcpevents.SubscribeResult, error) {
	var out mcpevents.SubscribeResult
	err := c.mcpEventsRequest(ctx, http.MethodPost, "/api/v1/mcp/events/subscribe", &generated.SubscribeMCPEventsRequestOptions{Body: &generated.SubscribeMCPEventsBody{
		Name: req.Name, Arguments: req.Arguments, Delivery: mcpEventsDelivery(req.Delivery), Cursor: req.Cursor, TTLMs: req.TTLMS,
	}}, &out)
	return out, err
}
func (c *Client) MCPEventsUnsubscribe(ctx context.Context, req mcpevents.UnsubscribeRequest) error {
	return c.mcpEventsRequest(ctx, http.MethodPost, "/api/v1/mcp/events/unsubscribe", &generated.UnsubscribeMCPEventsRequestOptions{Body: &generated.UnsubscribeMCPEventsBody{
		Name: req.Name, Arguments: req.Arguments, Delivery: mcpEventsDelivery(req.Delivery),
	}}, nil)
}
func (c *Client) GetMCPEvent(ctx context.Context, id string) (mcpevents.Envelope, error) {
	var out mcpevents.Envelope
	err := c.mcpEventsRequest(ctx, http.MethodGet, "/api/v1/mcp/events/event", &generated.GetMCPEventRequestOptions{Query: &generated.GetMCPEventQuery{EventID: id}}, &out)
	return out, err
}
func (c *Client) ListMCPCalendarSources(ctx context.Context) ([]mcpevents.CalendarSource, error) {
	var out []mcpevents.CalendarSource
	err := c.mcpEventsRequest(ctx, http.MethodGet, "/api/v1/mcp/events/calendar-sources", nil, &out)
	return out, err
}
func (c *Client) MCPEventsStatus(ctx context.Context) ([]mcpevents.SubscriptionStatus, error) {
	var out []mcpevents.SubscriptionStatus
	err := c.mcpEventsRequest(ctx, http.MethodGet, "/api/v1/mcp/events/status", nil, &out)
	return out, err
}

func (c *Client) GetMCPEventMessage(ctx context.Context, eventID string, messageID int64) (*query.MessageDetail, error) {
	var out generated.GetMCPEventMessageResponse
	err := c.mcpEventsRequest(ctx, http.MethodGet, "/api/v1/mcp/events/messages/{id}", &generated.GetMCPEventMessageRequestOptions{PathParams: &generated.GetMCPEventMessagePath{ID: strconv.FormatInt(messageID, 10)}, Query: &generated.GetMCPEventMessageQuery{EventID: eventID}}, &out)
	if err != nil {
		return nil, err
	}
	detail := cliMessageDetailFromGenerated(&out)
	detail.WebURL = c.messageWebURL(detail.ID)
	return detail, nil
}

func mcpEventsDelivery(delivery mcpevents.Delivery) generated.Delivery {
	out := generated.Delivery{Mode: delivery.Mode, URL: delivery.URL}
	if delivery.Secret != "" {
		out.Secret = &delivery.Secret
	}
	return out
}
