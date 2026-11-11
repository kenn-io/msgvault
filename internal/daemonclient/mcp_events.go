package daemonclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"

	"go.kenn.io/msgvault/internal/mcpevents"
)

func (c *Client) mcpEventsRequest(ctx context.Context, method, path string, body any, out any) error {
	var options *muesliRequestOptions
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		options = &muesliRequestOptions{body: raw}
	}
	var resp *http.Response
	var err error
	if options == nil {
		resp, err = c.DoGeneratedRequestWithContext(ctx, method, path, nil)
	} else {
		resp, err = c.DoGeneratedRequestWithContext(ctx, method, path, options)
	}
	if err != nil {
		return errors.New("events daemon unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return errors.New("events daemon unavailable")
	}
	if resp.StatusCode != http.StatusOK {
		var e mcpevents.Error
		if json.Unmarshal(raw, &e) == nil {
			switch e.Code {
			case mcpevents.InvalidParams, mcpevents.NotFound, mcpevents.Forbidden, mcpevents.ResourceExhausted, mcpevents.Unsupported:
				reason := "request_rejected"
				if e.Reason == "invalid_scope" {
					reason = e.Reason
				}
				return &mcpevents.Error{Code: e.Code, Reason: reason}
			}
		}
		return errors.New("events daemon unavailable")
	}
	if err = json.Unmarshal(raw, out); err != nil {
		return errors.New("invalid events response")
	}
	return nil
}
func (c *Client) MCPEventsList(ctx context.Context) (mcpevents.ListResult, error) {
	var out mcpevents.ListResult
	err := c.mcpEventsRequest(ctx, http.MethodGet, "/api/v1/mcp-events/list", nil, &out)
	return out, err
}
func (c *Client) MCPEventsSubscribe(ctx context.Context, in mcpevents.SubscribeRequest) (mcpevents.SubscribeResult, error) {
	var out mcpevents.SubscribeResult
	err := c.mcpEventsRequest(ctx, http.MethodPost, "/api/v1/mcp-events/subscribe", in, &out)
	return out, err
}
func (c *Client) MCPEventsUnsubscribe(ctx context.Context, in mcpevents.UnsubscribeRequest) error {
	var out struct{}
	return c.mcpEventsRequest(ctx, http.MethodPost, "/api/v1/mcp-events/unsubscribe", in, &out)
}
