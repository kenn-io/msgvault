package meta

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

// blockedUserForm keeps the provider's scoped user parameter in one place.
// Account eligibility and identifier acceptance still need live verification.
func blockedUserForm(userID string, block bool) (url.Values, error) {
	if !safeObjectID(userID) {
		return nil, errors.New("invalid commenter identity")
	}
	user := userID
	if block {
		data, err := json.Marshal([]string{userID})
		if err != nil {
			return nil, err
		}
		user = string(data)
	}
	return url.Values{"user": {user}}, nil
}
func (c *Client) BlockUser(ctx context.Context, pageID, userID string) error {
	if !ValidAccountID(pageID) {
		return errors.New("invalid Page identity")
	}
	form, err := blockedUserForm(userID, true)
	if err != nil {
		return err
	}
	var out map[string]bool
	if err := c.post(ctx, pageID+"/blocked", form, &out); err != nil {
		return err
	}
	if !out[userID] {
		return errors.New("commenter block acceptance is unknown")
	}
	return nil
}
func (c *Client) UnblockUser(ctx context.Context, pageID, userID string) error {
	if !ValidAccountID(pageID) {
		return errors.New("invalid Page identity")
	}
	form, err := blockedUserForm(userID, false)
	if err != nil {
		return err
	}
	var out struct {
		Success bool `json:"success"`
	}
	if err := c.do(ctx, http.MethodDelete, pageID+"/blocked", nil, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", &out); err != nil {
		return err
	}
	if !out.Success {
		return errors.New("commenter unblock acceptance is unknown")
	}
	return nil
}
