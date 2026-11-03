package meta

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

// PrivateReplyResult contains the provider identifiers returned by a private reply.
type PrivateReplyResult struct{ RecipientID, MessageID string }

func (c *Client) ReplyToComment(ctx context.Context, id, body string) (Object, error) {
	if !safeObjectID(id) {
		return Object{}, errors.New("invalid comment identity")
	}
	var out Object
	err := c.post(ctx, url.PathEscape(id)+"/comments", url.Values{"message": {body}, "fields": {"id,message,created_time,from,parent"}}, &out)
	if err == nil && !safeObjectID(out.ID) {
		err = errors.New("comment reply acceptance is unknown")
	}
	return out, err
}
func (c *Client) PrivateReply(ctx context.Context, asset, commentID, body string) (PrivateReplyResult, error) {
	if !ValidAccountID(asset) || !safeObjectID(commentID) {
		return PrivateReplyResult{}, errors.New("invalid private reply identity")
	}
	var out struct {
		RecipientID string `json:"recipient_id"`
		MessageID   string `json:"message_id"`
	}
	err := c.postJSON(ctx, url.PathEscape(asset)+"/messages", map[string]any{"recipient": map[string]string{"comment_id": commentID}, "message": map[string]string{"text": body}}, &out)
	if err == nil && (strings.TrimSpace(out.RecipientID) == "" || strings.TrimSpace(out.MessageID) == "") {
		err = errors.New("private reply acceptance is unknown")
	}
	return PrivateReplyResult{RecipientID: out.RecipientID, MessageID: out.MessageID}, err
}
