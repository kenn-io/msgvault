package meta

import (
	"context"
	"errors"
	"net/url"
	"strconv"
)

type instagramActionClient struct{ *Client }

// InstagramActions selects Instagram write endpoints while sharing transport
// and private-reply behavior with the configured Graph client.
func (c *Client) InstagramActions() ActionAPI { return instagramActionClient{c} }
func (c instagramActionClient) SetCommentHidden(ctx context.Context, id string, hidden bool) error {
	if !safeObjectID(id) {
		return errors.New("invalid comment identity")
	}
	var out struct {
		Success bool `json:"success"`
	}
	if err := c.post(ctx, url.PathEscape(id), url.Values{"hide": {strconv.FormatBool(hidden)}}, &out); err != nil {
		return err
	}
	if !out.Success {
		return errors.New("comment mutation acceptance is unknown")
	}
	return nil
}
func (c instagramActionClient) ReplyToComment(ctx context.Context, id, body string) (Object, error) {
	if !safeObjectID(id) {
		return Object{}, errors.New("invalid comment identity")
	}
	var out Object
	err := c.post(ctx, url.PathEscape(id)+"/replies", url.Values{"message": {body}}, &out)
	if err == nil && !safeObjectID(out.ID) {
		err = errors.New("comment reply acceptance is unknown")
	}
	return Object{ID: out.ID}, err
}
