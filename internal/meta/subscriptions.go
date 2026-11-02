package meta

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
)

// PageWebhookFields is the Page subscription surface supported by this client.
var PageWebhookFields = []string{"feed", "mention", "messages", "message_echoes", "message_reactions", "message_edits", "messaging_postbacks", "messaging_referrals", "messaging_optins", "standby", "messaging_handovers"}

// TokenApp identifies the app that issued this client's Page token.
func (c *Client) TokenApp(ctx context.Context) (string, error) {
	var app struct {
		ID string `json:"id"`
	}
	if err := c.get(ctx, "app", url.Values{"fields": {"id"}}, &app); err != nil {
		return "", err
	}
	if !ValidAccountID(app.ID) {
		return "", errors.New("cannot identify the token app")
	}
	return app.ID, nil
}

// SubscribedFields refuses to guess when the returned apps cannot be identified.
func (c *Client) SubscribedFields(ctx context.Context, pageID, appID string) (bool, []string, error) {
	if !ValidAccountID(pageID) || !ValidAccountID(appID) {
		return false, nil, errors.New("invalid selected Page or app ID")
	}
	var wire struct {
		Data []struct {
			ID     string   `json:"id"`
			Fields []string `json:"subscribed_fields"`
		} `json:"data"`
	}
	if err := c.get(ctx, pageID+"/subscribed_apps", url.Values{"fields": {"id,subscribed_fields"}}, &wire); err != nil {
		return false, nil, err
	}
	if wire.Data == nil {
		return false, nil, errors.New("cannot identify the app in subscriptions")
	}
	var found bool
	var fields []string
	for _, app := range wire.Data {
		if !ValidAccountID(app.ID) {
			return false, nil, errors.New("cannot identify the app in subscriptions")
		}
		if app.ID == appID {
			found, fields = true, app.Fields
		}
	}
	return found, fields, nil
}

// Subscribe installs only explicitly supported fields. Callers preserve existing
// fields by reading them first and passing the union with requested fields.
func (c *Client) Subscribe(ctx context.Context, pageID string, fields []string) error {
	if !ValidAccountID(pageID) || len(fields) == 0 {
		return errors.New("invalid selected Page or empty webhook fields")
	}
	for _, field := range fields {
		if !slices.Contains(PageWebhookFields, field) {
			return errors.New("unsupported Page webhook field")
		}
	}
	var result struct {
		Success bool `json:"success"`
	}
	if err := c.post(ctx, pageID+"/subscribed_apps", url.Values{"subscribed_fields": {strings.Join(fields, ",")}}, &result); err != nil {
		return err
	}
	if !result.Success {
		return errors.New("graph subscription was not confirmed")
	}
	return nil
}
