package meta

import (
	"context"
	"errors"
	"net/url"
	"strconv"
)

func (c *Client) SetCommentHidden(ctx context.Context, id string, hidden bool) error {
	if !safeObjectID(id) {
		return errors.New("invalid comment identity")
	}
	var out struct {
		Success bool `json:"success"`
	}
	if err := c.post(ctx, url.PathEscape(id), url.Values{"is_hidden": {strconv.FormatBool(hidden)}}, &out); err != nil {
		return err
	}
	if !out.Success {
		return errors.New("comment mutation acceptance is unknown")
	}
	return nil
}
func (c *Client) DeleteComment(ctx context.Context, id string) error {
	if !safeObjectID(id) {
		return errors.New("invalid comment identity")
	}
	var out struct {
		Success bool `json:"success"`
	}
	if err := c.delete(ctx, url.PathEscape(id), &out); err != nil {
		return err
	}
	if !out.Success {
		return errors.New("comment mutation acceptance is unknown")
	}
	return nil
}

// PrivateReplyResult contains the provider identifiers returned by a private reply.
type PrivateReplyResult struct{ RecipientID, MessageID string }

func (c *Client) ReplyToComment(context.Context, string, string) (Object, error) {
	return Object{}, errors.New("comment replies are not enabled")
}
func (c *Client) PrivateReply(context.Context, string, string, string) (PrivateReplyResult, error) {
	return PrivateReplyResult{}, errors.New("comment replies are not enabled")
}
