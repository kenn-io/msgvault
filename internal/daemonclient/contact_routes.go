package daemonclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// FindContactCandidatesContext forwards read-only lookup to the owning daemon.
func (c *Client) FindContactCandidatesContext(ctx context.Context, q store.ContactCandidateQuery) (*store.ContactCandidatePage, error) {
	if err := store.ValidateContactCandidateQuery(q); err != nil {
		return nil, err
	}
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.FindContactCandidatesResp, error) {
		return client.FindContactCandidatesWithResponse(ctx, &generated.FindContactCandidatesRequestOptions{Query: &generated.FindContactCandidatesQuery{Query: q.Query, Limit: optionalContactCursor(int64(q.Limit)), AfterID: optionalContactCursor(q.AfterID)}})
	})
	if err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, errors.New("empty contact candidate response")
	}
	page := &store.ContactCandidatePage{}
	// Decode the original typed operation's body to preserve Store provenance
	// types without duplicating each contact envelope in the daemon adapter.
	if err := json.Unmarshal(resp.Body, page); err != nil {
		return nil, fmt.Errorf("decode contact candidate response: %w", err)
	}
	return page, nil
}

func (c *Client) GetPersonMessagingRoutesContext(ctx context.Context, q store.PersonMessagingRouteQuery) (*store.PersonMessagingRoutesPage, error) {
	if err := store.ValidatePersonMessagingRouteQuery(q); err != nil {
		return nil, err
	}
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetPersonMessagingRoutesResp, error) {
		return client.GetPersonMessagingRoutesWithResponse(ctx, &generated.GetPersonMessagingRoutesRequestOptions{Query: &generated.GetPersonMessagingRoutesQuery{
			PersonUID: q.PersonUID, Network: optionalString(q.Network), SourceID: optionalContactCursor(q.SourceID), Limit: optionalContactCursor(int64(q.Limit)),
			AfterConversationID: optionalContactCursor(q.AfterConversationID), AfterContactPointID: optionalContactCursor(q.AfterContactPointID), AfterObservationID: optionalContactCursor(q.AfterObservationID), AfterSuggestionID: optionalContactCursor(q.AfterSuggestionID),
		}})
	})
	if err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, errors.New("empty person messaging routes response")
	}
	page := &store.PersonMessagingRoutesPage{}
	if err := json.Unmarshal(resp.Body, page); err != nil {
		return nil, fmt.Errorf("decode person messaging routes response: %w", err)
	}
	return page, nil
}

func optionalContactCursor(id int64) *int64 {
	if id == 0 {
		return nil
	}
	return &id
}
