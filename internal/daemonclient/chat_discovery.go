package daemonclient

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const ChatDiscoveryMinAPISchemaVersion = "3.11.0"

// SearchChats forwards name discovery to the archive-owning daemon.
func (c *Client) SearchChats(ctx context.Context, q store.ChatDiscoveryQuery) (*store.ChatDiscoveryPage, error) {
	compatible, err := c.SupportsAPISchemaVersion(ctx, ChatDiscoveryMinAPISchemaVersion)
	if err != nil {
		return nil, fmt.Errorf("check chat discovery capability: %w", err)
	}
	if !compatible {
		return nil, fmt.Errorf("chat discovery requires daemon API schema %s or newer", ChatDiscoveryMinAPISchemaVersion)
	}
	if q.Limit == 0 {
		q.Limit = store.DefaultChatDiscoveryLimit
	}
	params := &generated.SearchChatsQuery{Q: q.Query, Limit: int64FromInt(q.Limit)}
	if q.SourceID != 0 {
		params.SourceID = &q.SourceID
	}
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.SearchChatsResp, error) {
		return client.SearchChatsWithResponse(ctx, &generated.SearchChatsRequestOptions{Query: params})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("empty chat discovery response")
	}
	page := &store.ChatDiscoveryPage{
		Results: make([]store.ChatDiscoveryResult, len(response.JSON200.Results)),
		HasMore: response.JSON200.HasMore,
	}
	for i, result := range response.JSON200.Results {
		page.Results[i] = store.ChatDiscoveryResult{
			ConversationID: result.ConversationID, MessageID: result.MessageID, SourceConversationID: stringValue(result.SourceConversationID),
			SourceID: result.SourceID, SourceType: result.SourceType,
			SourceIdentifier: stringValue(result.SourceIdentifier), SourceDisplayName: stringValue(result.SourceDisplayName),
			Network: result.Network, Title: stringValue(result.Title), ConversationType: result.ConversationType,
			MatchedTokens: result.MatchedTokens, MatchedNames: result.MatchedNames,
			EvidenceTruncated: result.EvidenceTruncated, MatchKind: result.MatchKind,
		}
	}
	return page, nil
}
