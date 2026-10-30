package threads

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

var ErrInvalidReplyBody = errors.New("Threads reply body must be valid UTF-8, nonblank, without NUL, and at most 500 bytes")

// ValidateReplyBody applies the conservative outgoing UTF-8 byte limit. Drafts
// retain their original text even when it is not eligible for publication.
func ValidateReplyBody(body string) error {
	if !utf8.ValidString(body) || len(body) > 500 || strings.ContainsRune(body, 0) || strings.TrimSpace(body) == "" {
		return ErrInvalidReplyBody
	}
	return nil
}
func (c *Client) ReplyQuota(ctx context.Context) (ReplyQuota, error) {
	var wire struct {
		Data []struct {
			Usage  *int64 `json:"reply_quota_usage"`
			Config *struct {
				Total    *int64 `json:"quota_total"`
				Duration *int64 `json:"quota_duration"`
			} `json:"reply_config"`
		} `json:"data"`
	}
	err := c.getJSON(ctx, "/me/threads_publishing_limit", url.Values{"fields": {"reply_quota_usage,reply_config"}}, &wire)
	if err != nil {
		return ReplyQuota{}, err
	}
	if len(wire.Data) != 1 {
		return ReplyQuota{}, ErrDecodeResponse
	}
	item := wire.Data[0]
	if item.Usage == nil || *item.Usage < 0 || item.Config == nil || item.Config.Total == nil || *item.Config.Total <= 0 || item.Config.Duration == nil || *item.Config.Duration <= 0 {
		return ReplyQuota{}, ErrDecodeResponse
	}
	return ReplyQuota{Usage: *item.Usage, Total: *item.Config.Total, Duration: *item.Config.Duration}, nil
}
func (c *Client) CreateReply(ctx context.Context, parent, body string) (string, error) {
	if !validID(parent) {
		return "", errors.New("invalid Threads reply parent")
	}
	if err := ValidateReplyBody(body); err != nil {
		return "", err
	}
	var wire struct {
		ID string `json:"id"`
	}
	err := c.postJSON(ctx, "/me/threads", url.Values{"media_type": {"TEXT"}, "reply_to_id": {parent}, "text": {body}}, &wire)
	if err != nil && (!isPause(err) || wire.ID == "") {
		return "", err
	}
	if !validID(wire.ID) {
		return "", ErrDecodeResponse
	}
	return wire.ID, err
}
func (c *Client) ContainerStatus(ctx context.Context, id string) (string, error) {
	if !validID(id) {
		return "", errors.New("invalid Threads container ID")
	}
	var wire struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	err := c.getJSON(ctx, "/"+id, url.Values{"fields": {"status,error_message"}}, &wire)
	if err != nil {
		return "", err
	}
	if wire.ID != "" && wire.ID != id {
		return "", ErrDecodeResponse
	}
	switch wire.Status {
	case "IN_PROGRESS", "FINISHED", "PUBLISHED", "ERROR", "EXPIRED":
		return wire.Status, nil
	}
	return "", ErrDecodeResponse
}
func (c *Client) PublishReply(ctx context.Context, owner, container string) (string, error) {
	if !validID(owner) || !validID(container) {
		return "", errors.New("invalid Threads publish identity")
	}
	var wire struct {
		ID string `json:"id"`
	}
	err := c.postJSON(ctx, "/"+owner+"/threads_publish", url.Values{"creation_id": {container}}, &wire)
	if err != nil && (!isPause(err) || wire.ID == "") {
		return "", err
	}
	if !validID(wire.ID) {
		return "", ErrDecodeResponse
	}
	return wire.ID, err
}

// postJSON makes exactly one dispatch. The shared transport also refuses
// redirects, connection replay and hidden retries. No idempotency key is used.
func (c *Client) postJSON(ctx context.Context, path string, params url.Values, out any) error {
	u := *c.base
	u.Path += path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(params.Encode()))
	if err != nil {
		return errors.New("could not construct Threads write request")
	}
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return safeRequestError(ctx, err)
	}
	body, err := readBody(response)
	if err != nil {
		if ctx.Err() != nil {
			return safeRequestError(ctx, ctx.Err())
		}
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeAPIError(response.StatusCode, body)
	}
	if err = json.Unmarshal(body, out); err != nil {
		return ErrDecodeResponse
	}
	if highUsage(response.Header) {
		return &PauseError{Reason: "high_usage"}
	}
	return nil
}

// Admission remains separate from transport accounting. A known exhausted
// workload must pause before committing the irreversible publish barrier.
func (c *Client) checkPublishBudget(ctx context.Context) error {
	if ctx.Err() != nil {
		return safeRequestError(ctx, ctx.Err())
	}
	c.budget.mu.Lock()
	defer c.budget.mu.Unlock()
	return c.budget.check()
}
