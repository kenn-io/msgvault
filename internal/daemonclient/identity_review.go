package daemonclient

import (
	"context"
	"errors"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) ListIdentityMatches(ctx context.Context, state string, limit, offset int64) (*generated.IdentityMatchCandidatesResponse, error) {
	query := &generated.ListIdentityMatchCandidatesQuery{Limit: &limit, Offset: &offset}
	if state != "" && state != "all" {
		query.State = &state
	}
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.ListIdentityMatchCandidatesResp, error) {
		return api.ListIdentityMatchCandidatesWithResponse(ctx, &generated.ListIdentityMatchCandidatesRequestOptions{Query: query})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("identity match list response was empty")
	}
	return response.JSON200, nil
}

func (c *Client) GetIdentityMatch(ctx context.Context, id int64) (*generated.IdentityMatchCandidate, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.GetIdentityMatchCandidateResp, error) {
		return api.GetIdentityMatchCandidateWithResponse(ctx, &generated.GetIdentityMatchCandidateRequestOptions{PathParams: &generated.GetIdentityMatchCandidatePath{ID: id}})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("identity match response was empty")
	}
	return response.JSON200, nil
}

func (c *Client) AcceptIdentityMatch(ctx context.Context, id int64, token string, notes *string) (*generated.IdentityMatchAcceptResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.ReviewAcceptIdentityMatchCandidateResp, error) {
		return api.ReviewAcceptIdentityMatchCandidateWithResponse(ctx, &generated.ReviewAcceptIdentityMatchCandidateRequestOptions{
			PathParams: &generated.ReviewAcceptIdentityMatchCandidatePath{ID: id},
			Body:       &generated.DecideIdentityMatchReviewedRequest{ReviewToken: token, Notes: notes},
		})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("identity match acceptance response was empty")
	}
	return response.JSON200, nil
}

func (c *Client) RejectIdentityMatch(ctx context.Context, id int64, token string, notes *string) (*generated.IdentityMatchRejectResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.ReviewRejectIdentityMatchCandidateResp, error) {
		return api.ReviewRejectIdentityMatchCandidateWithResponse(ctx, &generated.ReviewRejectIdentityMatchCandidateRequestOptions{
			PathParams: &generated.ReviewRejectIdentityMatchCandidatePath{ID: id},
			Body:       &generated.DecideIdentityMatchReviewedRequest{ReviewToken: token, Notes: notes},
		})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("identity match rejection response was empty")
	}
	return response.JSON200, nil
}
