package meta

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const marketingFieldsQueryKey = "fields"

// SubscriptionToken is secret consent data. Surfaces must project fingerprints.
type SubscriptionToken struct {
	Token, PSID, TopicTitle, Timezone, UserTokenStatus, Reoptin string
	Created, Expiry, NextEligible                               time.Time
}
type TokenPage struct {
	Data  []SubscriptionToken
	After string
}
type CampaignInfo struct {
	ID, PageID                            string
	HasBudgetWarning                      bool
	DailyBudgetCents, LifetimeBudgetCents int64
	EndsAt                                time.Time
}

// MarketingAPI is the provider boundary for consent walks and confirmed sends.
// Page-token and ads-token calls use separate Client instances at daemon wiring.
type MarketingAPI interface {
	GrantedPermissions(ctx context.Context) (map[string]bool, error)
	SubscriptionTokens(ctx context.Context, pageID, after string) (TokenPage, error)
	CreateMessageCampaign(ctx context.Context, adAccountID, pageID, name string, dailyBudget int64, endsAt time.Time) (CampaignInfo, error)
	CampaignPage(ctx context.Context, campaignID string) (CampaignInfo, error)
	PreviewMarketingMessage(ctx context.Context, adAccountID, pageID string, message jsontext.Value) (jsontext.Value, error)
	SendMarketingMessage(ctx context.Context, adAccountID, campaignID, token string, message jsontext.Value, gapSeconds int64) (string, error)
	UnsubscribePSIDs(ctx context.Context, pageID string, psids []string) (int, error)
	UnsubscribeTokens(ctx context.Context, pageID string, tokens []string) (int, error)
}

var _ MarketingAPI = (*Client)(nil)

func (c *Client) GrantedPermissions(ctx context.Context) (map[string]bool, error) {
	var wire struct {
		Data []struct {
			Permission string `json:"permission"`
			Status     string `json:"status"`
		} `json:"data"`
	}
	if err := c.get(ctx, "me/permissions", nil, &wire); err != nil {
		return nil, err
	}
	if wire.Data == nil {
		return nil, errors.New("invalid Graph permission response")
	}
	out := map[string]bool{}
	for _, p := range wire.Data {
		out[p.Permission] = p.Status == "granted"
	}
	return out, nil
}

type marketingTimestamp struct{ time.Time }

func (v *marketingTimestamp) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return errors.New("invalid marketing timestamp")
	}
	if n > 0 {
		v.Time = time.Unix(n, 0).UTC()
	}
	return nil
}

func (c *Client) SubscriptionTokens(ctx context.Context, pageID, after string) (TokenPage, error) {
	if !ValidAccountID(pageID) || !c.safeCursor(after) {
		return TokenPage{}, errors.New("invalid marketing subscription request")
	}
	q := url.Values{"limit": {"1000"}}
	if after != "" {
		q.Set("after", after)
	}
	var wire struct {
		Data []struct {
			Token    string             `json:"notification_messages_token"`
			PSID     string             `json:"recipient_id"`
			Title    string             `json:"topic_title"`
			Timezone string             `json:"notification_messages_timezone"`
			Status   string             `json:"user_token_status"`
			Reoptin  string             `json:"notification_messages_reoptin"`
			Created  marketingTimestamp `json:"creation_timestamp"`
			Expiry   marketingTimestamp `json:"token_expiry_timestamp"`
			Next     marketingTimestamp `json:"next_eligible_time"`
		} `json:"data"`
		Paging struct {
			Cursors struct {
				After string `json:"after"`
			} `json:"cursors"`
			Next string `json:"next"`
		} `json:"paging"`
	}
	if err := c.get(ctx, pageID+"/notification_message_tokens", q, &wire); err != nil {
		return TokenPage{}, err
	}
	if wire.Data == nil || len(wire.Data) > 1000 {
		return TokenPage{}, errors.New("invalid marketing subscription page")
	}
	out := TokenPage{Data: make([]SubscriptionToken, 0, len(wire.Data))}
	if wire.Paging.Next != "" {
		out.After = wire.Paging.Cursors.After
		if out.After == "" || out.After == after || !c.safeCursor(out.After) {
			return TokenPage{}, errors.New("invalid Graph pagination progress")
		}
	}
	for _, v := range wire.Data {
		if v.Token == "" {
			return TokenPage{}, errors.New("invalid marketing subscription")
		}
		if (after != "" && strings.Contains(after, v.Token)) || (out.After != "" && strings.Contains(out.After, v.Token)) {
			return TokenPage{}, errors.New("invalid Graph cursor")
		}
		out.Data = append(out.Data, SubscriptionToken{v.Token, v.PSID, v.Title, v.Timezone, v.Status, v.Reoptin, v.Created.Time, v.Expiry.Time, v.Next.Time})
	}
	return out, nil
}

func marketingAccount(id string) (string, error) {
	if !ValidAccountID(id) {
		return "", errors.New("marketing ad account must be decimal")
	}
	return "act_" + id, nil
}
func (c *Client) CreateMessageCampaign(ctx context.Context, account, page, name string, budget int64, end time.Time) (CampaignInfo, error) {
	path, err := marketingAccount(account)
	if err != nil {
		return CampaignInfo{}, err
	}
	if !ValidAccountID(page) || budget <= 0 || end.IsZero() {
		return CampaignInfo{}, errors.New("invalid marketing campaign")
	}
	var wire struct {
		ID      string         `json:"id"`
		Warning jsontext.Value `json:"warning"`
		Info    jsontext.Value `json:"info"`
	}
	err = c.post(ctx, path+"/message_campaign", url.Values{"name": {name}, "page_id": {page}, "daily_budget": {strconv.FormatInt(budget, 10)}, "end_time": {strconv.FormatInt(end.Unix(), 10)}}, &wire)
	if err != nil {
		return CampaignInfo{}, err
	}
	if !safeObjectID(wire.ID) {
		return CampaignInfo{}, errors.New("invalid marketing campaign response")
	}
	return CampaignInfo{ID: wire.ID, PageID: page, HasBudgetWarning: len(wire.Warning) > 0 || len(wire.Info) > 0, DailyBudgetCents: budget, EndsAt: end}, nil
}
func (c *Client) CampaignPage(ctx context.Context, id string) (CampaignInfo, error) {
	if !safeObjectID(id) {
		return CampaignInfo{}, errors.New("invalid marketing campaign ID")
	}
	var wire struct {
		ID       string `json:"id"`
		Creative struct {
			Actor string `json:"actor_id"`
		} `json:"creative"`
		Adset struct {
			Daily    jsontext.Value `json:"daily_budget"`
			Lifetime jsontext.Value `json:"lifetime_budget"`
			End      string         `json:"end_time"`
		} `json:"adset"`
	}
	err := c.get(ctx, url.PathEscape(id), url.Values{marketingFieldsQueryKey: {"id,campaign{name},creative{actor_id},adset{daily_budget,lifetime_budget,end_time}"}}, &wire)
	if err != nil {
		return CampaignInfo{}, err
	}
	amount := func(raw jsontext.Value) (int64, error) {
		if len(raw) == 0 || string(raw) == "null" {
			return 0, nil
		}
		return strconv.ParseInt(strings.Trim(string(raw), `"`), 10, 64)
	}
	daily, dailyErr := amount(wire.Adset.Daily)
	lifetime, lifetimeErr := amount(wire.Adset.Lifetime)
	end, endErr := parseDMTime(wire.Adset.End)
	if wire.ID != id || !ValidAccountID(wire.Creative.Actor) || dailyErr != nil || lifetimeErr != nil || endErr != nil || daily <= 0 || lifetime < 0 || end.IsZero() {
		return CampaignInfo{}, errors.New("invalid marketing campaign settings")
	}
	return CampaignInfo{ID: id, PageID: wire.Creative.Actor, DailyBudgetCents: daily, LifetimeBudgetCents: lifetime, EndsAt: end.UTC()}, nil
}
func (c *Client) PreviewMarketingMessage(ctx context.Context, account, page string, message jsontext.Value) (jsontext.Value, error) {
	path, err := marketingAccount(account)
	if err != nil {
		return nil, err
	}
	if !ValidAccountID(page) || !message.IsValid() {
		return nil, errors.New("invalid marketing preview")
	}
	creative, _ := json.Marshal(map[string]any{"marketing_message_structured_spec": map[string]any{}, "object_story_spec": map[string]any{"page_id": page, "link_data": map[string]any{}}})
	var out jsontext.Value
	err = c.get(ctx, path+"/generatepreviews", url.Values{"ad_format": {"MESSENGER_MARKETING_MESSAGES_MEDIA"}, "creative": {string(creative)}, "message": {string(message)}}, &out)
	return out, err
}
func (c *Client) SendMarketingMessage(ctx context.Context, account, campaign, token string, message jsontext.Value, gap int64) (string, error) {
	path, err := marketingAccount(account)
	if err != nil {
		return "", err
	}
	if !safeObjectID(campaign) || token == "" || !message.IsValid() || gap < 0 {
		return "", errors.New("invalid marketing message")
	}
	delivery, _ := json.Marshal(map[string]string{"subscription_token": token})
	form := url.Values{"message_id": {campaign}, "messenger_delivery_data": {string(delivery)}, "message": {string(message)}}
	if gap > 0 {
		form.Set("min_conversation_gap_seconds", strconv.FormatInt(gap, 10))
	}
	var wire struct {
		Success  bool   `json:"success"`
		Tracking string `json:"marketing_message_tracking_id"`
	}
	if err := c.post(ctx, path+"/messages", form, &wire); err != nil {
		return "", err
	}
	if !wire.Success || wire.Tracking == "" {
		return "", errors.New("invalid marketing send response")
	}
	return wire.Tracking, nil
}
func (c *Client) UnsubscribePSIDs(ctx context.Context, page string, psids []string) (int, error) {
	return c.unsubscribeMarketing(ctx, page, "psid", psids)
}
func (c *Client) UnsubscribeTokens(ctx context.Context, page string, tokens []string) (int, error) {
	return c.unsubscribeMarketing(ctx, page, "subscriber_token", tokens)
}
func (c *Client) unsubscribeMarketing(ctx context.Context, page, key string, values []string) (int, error) {
	if !ValidAccountID(page) || len(values) == 0 {
		return 0, errors.New("invalid marketing unsubscribe")
	}
	rows := make([]map[string]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			return 0, errors.New("invalid marketing unsubscribe")
		}
		rows = append(rows, map[string]string{key: v})
	}
	data, _ := json.Marshal(rows)
	var wire struct {
		Invalid int `json:"invalid_rows_count"`
	}
	err := c.post(ctx, page+"/messaging_customer_base/unsubscribe", url.Values{"data": {string(data)}}, &wire)
	return wire.Invalid, err
}
