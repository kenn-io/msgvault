package beeper

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"math/big"
	"net/url"
	"strings"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// Decode only identity and ordering metadata from one bounded newest page. No
// text, attachment, sender or other message content enters control state.
type inboxMessageMetadata struct {
	ID        string `json:"id"`
	AccountID string `json:"accountID"`
	ChatID    string `json:"chatID"`
	SortKey   string `json:"sortKey"`
}

func (c *Client) inboxLatestMessage(ctx context.Context, target inboxcontrol.Target) (*inboxMessageMetadata, error) {
	path := fmt.Sprintf("/v1/chats/%s/messages", url.PathEscape(target.ProviderID))
	data, err := c.inboxBytes(ctx, path)
	if err != nil {
		return nil, err
	}
	var page struct {
		Items   *[]inboxMessageMetadata `json:"items"`
		HasMore *bool                   `json:"hasMore"`
	}
	if json.Unmarshal(data, &page) != nil || page.Items == nil || len(*page.Items) > 200 {
		return nil, inboxcontrol.ErrUnavailable
	}
	if len(*page.Items) == 0 && (page.HasMore == nil || *page.HasMore) {
		return nil, inboxcontrol.ErrUnavailable
	}
	var latest *inboxMessageMetadata
	seen := map[string]bool{}
	for _, message := range *page.Items {
		if message.AccountID != target.AccountID || message.ChatID != target.ProviderID {
			return nil, inboxcontrol.ErrDenied
		}
		check := target
		check.ProviderID = message.ID
		if check.Validate() != nil || seen[message.ID] {
			return nil, inboxcontrol.ErrUnavailable
		}
		seen[message.ID] = true
		check.ProviderID = message.SortKey
		if check.Validate() != nil {
			return nil, inboxcontrol.ErrUnavailable
		}
		if latest != nil && compareInboxSortKeys(message.SortKey, latest.SortKey) == 0 {
			return nil, inboxcontrol.ErrUnavailable
		}
		if latest == nil || compareInboxSortKeys(message.SortKey, latest.SortKey) > 0 {
			observedMessage := message
			latest = &observedMessage
		}
	}
	return latest, nil
}
func compareInboxSortKeys(a, b string) int {
	left, leftOK := new(big.Int).SetString(a, 10)
	right, rightOK := new(big.Int).SetString(b, 10)
	if leftOK && rightOK {
		return left.Cmp(right)
	}
	return strings.Compare(a, b)
}
