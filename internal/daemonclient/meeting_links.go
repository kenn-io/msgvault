package daemonclient

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) DecideMeetingLink(ctx context.Context, messageID int64, event, decision string) error {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.DecideMeetingLinkResp, error) {
		return client.DecideMeetingLinkWithResponse(ctx, &generated.DecideMeetingLinkRequestOptions{Body: &generated.DecideMeetingLinkBody{MessageID: messageID, Event: event, Decision: generated.MeetingLinkDecisionRequestDecision(decision)}})
	})
	if err != nil {
		return err
	}
	if response.JSON200 == nil {
		return errors.New("meeting link decision: empty response")
	}
	return nil
}

func (c *Client) SuggestMeetingLinks(ctx context.Context, ref string) ([]store.MeetingSuggestion, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.SuggestMeetingLinksResp, error) {
		return client.SuggestMeetingLinksWithResponse(ctx, &generated.SuggestMeetingLinksRequestOptions{Query: &generated.SuggestMeetingLinksQuery{Ref: ref}})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("meeting suggestions: empty response")
	}
	decoded, err := decodeMeetingResponse[struct {
		Suggestions []store.MeetingSuggestion `json:"suggestions"`
	}]("meeting suggestions", response.Body)
	if err != nil {
		return nil, err
	}
	return decoded.Suggestions, nil
}

func (c *Client) MeetingLinks(ctx context.Context, messageID int64) ([]store.MeetingLink, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetMeetingLinksResp, error) {
		return client.GetMeetingLinksWithResponse(ctx, &generated.GetMeetingLinksRequestOptions{Query: &generated.GetMeetingLinksQuery{MessageID: messageID}})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("meeting links: empty response")
	}
	decoded, err := decodeMeetingResponse[struct {
		Links []store.MeetingLink `json:"links"`
	}]("meeting links", response.Body)
	if err != nil {
		return nil, err
	}
	return decoded.Links, nil
}
