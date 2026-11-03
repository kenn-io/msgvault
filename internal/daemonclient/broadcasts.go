package daemonclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"go.kenn.io/msgvault/internal/meta"
)

// GetBroadcast reads a broadcast using the client's authentication and cancellation
// path. Raw subscription tokens are absent from its response.
func (c *Client) GetBroadcast(ctx context.Context, id string) (meta.BroadcastDetail, error) {
	var out meta.BroadcastDetail
	err := c.readBroadcastJSON(ctx, "/api/v1/meta/broadcasts/"+url.PathEscape(id), &out)
	return out, err
}
func (c *Client) ListBroadcasts(ctx context.Context, profile string) (meta.BroadcastListResponse, error) {
	var out meta.BroadcastListResponse
	err := c.readBroadcastJSON(ctx, "/api/v1/meta/broadcasts?"+url.Values{"profile": {profile}}.Encode(), &out)
	return out, err
}
func (c *Client) ListBroadcastSubscriptions(ctx context.Context, profile string, participant int64, status string) (meta.BroadcastSubscriptionsResponse, error) {
	var out meta.BroadcastSubscriptionsResponse
	q := url.Values{"profile": {profile}}
	if participant > 0 {
		q.Set("participant", strconv.FormatInt(participant, 10))
	}
	if status != "" {
		q.Set("status", status)
	}
	err := c.readBroadcastJSON(ctx, "/api/v1/meta/broadcast-subscriptions?"+q.Encode(), &out)
	return out, err
}
func (c *Client) readBroadcastJSON(ctx context.Context, path string, out any) error {
	response, err := c.DoGeneratedRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return HandleErrorResponse(response)
	}
	const maxBytes = 8 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxBytes {
		return errors.New("broadcast response exceeds byte limit")
	}
	if err = json.Unmarshal(data, out); err != nil {
		return errors.New("invalid broadcast response")
	}
	return nil
}
