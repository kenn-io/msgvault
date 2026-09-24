package daemonclient

import (
	"context"
	"errors"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) GetIdentityScoringStatus(ctx context.Context) (*generated.PersonMatchScoringStatus, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.GetPersonMatchScoringStatusResp, error) {
		return api.GetPersonMatchScoringStatusWithResponse(ctx)
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("identity scoring status response was empty")
	}
	return response.JSON200, nil
}

func (c *Client) ScoreIdentityMatches(ctx context.Context, limit *int64) (*generated.PersonMatchDryRunResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.RunPersonMatchScoringResp, error) {
		return api.RunPersonMatchScoringWithResponse(ctx, &generated.RunPersonMatchScoringRequestOptions{
			Body: &generated.PersonMatchDryRunRequest{DryRun: true, Limit: limit},
		})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("identity scoring run response was empty")
	}
	return response.JSON200, nil
}

func (c *Client) ListIdentityJudgments(ctx context.Context, candidateID, limit, beforeID int64) (*generated.PersonMatchJudgmentHistoryResponse, error) {
	query := &generated.ListPersonMatchJudgmentsQuery{Limit: &limit}
	if candidateID > 0 {
		query.CandidateID = &candidateID
	}
	if beforeID > 0 {
		query.BeforeID = &beforeID
	}
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.ListPersonMatchJudgmentsResp, error) {
		return api.ListPersonMatchJudgmentsWithResponse(ctx, &generated.ListPersonMatchJudgmentsRequestOptions{Query: query})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("identity judgment history response was empty")
	}
	return response.JSON200, nil
}
