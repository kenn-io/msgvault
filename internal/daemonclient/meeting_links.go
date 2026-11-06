package daemonclient

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) DecideMeetingLink(ctx context.Context, messageID int64, event, decision string) (generated.MeetingLinkDecisionResponse, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.DecideMeetingLinkResp, error) {
		return client.DecideMeetingLinkWithResponse(ctx, &generated.DecideMeetingLinkRequestOptions{Body: &generated.DecideMeetingLinkBody{MessageID: messageID, Event: event, Decision: generated.MeetingLinkDecisionRequestDecision(decision)}})
	})
	if err != nil {
		return generated.MeetingLinkDecisionResponse{}, err
	}
	if response.JSON200 == nil {
		return generated.MeetingLinkDecisionResponse{}, errors.New("meeting link decision: empty response")
	}
	return *response.JSON200, nil
}

func (c *Client) SuggestMeetingLinks(ctx context.Context, ref string) (store.MeetingSuggestionPage, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.SuggestMeetingLinksResp, error) {
		return client.SuggestMeetingLinksWithResponse(ctx, &generated.SuggestMeetingLinksRequestOptions{Query: &generated.SuggestMeetingLinksQuery{Ref: ref}})
	})
	if err != nil {
		return store.MeetingSuggestionPage{}, err
	}
	if response.JSON200 == nil {
		return store.MeetingSuggestionPage{}, errors.New("meeting suggestions: empty response")
	}
	decoded, err := decodeMeetingResponse[store.MeetingSuggestionPage]("meeting suggestions", response.Body)
	if err != nil {
		return store.MeetingSuggestionPage{}, err
	}
	if decoded.Suggestions == nil {
		decoded.Suggestions = []store.MeetingSuggestion{}
	}
	if decoded.Unexpandable == nil {
		decoded.Unexpandable = []store.CalendarUnexpandable{}
	}
	return *decoded, nil
}

func (c *Client) MeetingRecord(ctx context.Context, ref string) (store.MeetingRecord, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetMeetingRecordResp, error) {
		return client.GetMeetingRecordWithResponse(ctx, &generated.GetMeetingRecordRequestOptions{Query: &generated.GetMeetingRecordQuery{Ref: ref}})
	})
	if err != nil {
		return store.MeetingRecord{}, err
	}
	if response.JSON200 == nil {
		return store.MeetingRecord{}, errors.New("meeting record: empty response")
	}
	decoded, err := decodeMeetingResponse[store.MeetingRecord]("meeting record", response.Body)
	if err != nil {
		return store.MeetingRecord{}, err
	}
	return *decoded, nil
}
