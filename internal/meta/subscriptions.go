package meta

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
)

// PageWebhookFields is the Page subscription surface supported by this client.
var PageWebhookFields = []string{"feed", "mention", "messages", "message_echoes", "message_reactions", "message_edits", "messaging_postbacks", "messaging_referrals", "messaging_optins", "marketing_message_echoes", "marketing_message_deliveries", "marketing_message_reads", "marketing_message_clicks", "marketing_message_delivery_failed", "standby", "messaging_handovers"}

const graphFieldsParam = "fields"

// TokenApp identifies the app that issued this client's Page token.
func (c *Client) TokenApp(ctx context.Context) (string, error) {
	var app struct {
		ID string `json:"id"`
	}
	if err := c.get(ctx, "app", url.Values{graphFieldsParam: {"id"}}, &app); err != nil {
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
		Paging struct {
			Next string `json:"next"`
		} `json:"paging"`
	}
	if err := c.get(ctx, pageID+"/subscribed_apps", url.Values{graphFieldsParam: {"id,subscribed_fields"}}, &wire); err != nil {
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
	if !found && wire.Paging.Next != "" {
		return false, nil, errors.New("cannot identify the app in an incomplete subscription listing")
	}
	return found, fields, nil
}

// Subscribe validates requested fields and preserves the existing fields read
// from the selected app, including fields outside this client's supported set.
func (c *Client) Subscribe(ctx context.Context, pageID string, fields []string, existing ...[]string) error {
	if !ValidAccountID(pageID) || len(fields) == 0 {
		return errors.New("invalid selected Page or empty webhook fields")
	}
	for _, field := range fields {
		if !slices.Contains(PageWebhookFields, field) {
			return errors.New("unsupported Page webhook field")
		}
	}
	fields = slices.Clone(fields)
	for _, preserved := range existing {
		fields = append(fields, preserved...)
	}
	slices.Sort(fields)
	fields = slices.Compact(fields)
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
