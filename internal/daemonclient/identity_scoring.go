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

func (c *Client) GrantIdentityScoringConsent(
	ctx context.Context, fingerprint string,
) (*generated.PersonMatchConsentDecisionResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.PersonMatchScoringConsentResp, error) {
		return api.PersonMatchScoringConsentWithResponse(ctx, &generated.PersonMatchScoringConsentRequestOptions{
			Body: &generated.PersonMatchConsentDecisionRequest{DisclosureFingerprint: fingerprint},
		})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("identity scoring consent response was empty")
	}
	return response.JSON200, nil
}

func (c *Client) RevokeIdentityScoringConsent(
	ctx context.Context, fingerprint string,
) (*generated.PersonMatchConsentDecisionResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.PersonMatchScoringRevokeResp, error) {
		return api.PersonMatchScoringRevokeWithResponse(ctx, &generated.PersonMatchScoringRevokeRequestOptions{
			Body: &generated.PersonMatchConsentDecisionRequest{DisclosureFingerprint: fingerprint},
		})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("identity scoring consent response was empty")
	}
	return response.JSON200, nil
}

func (c *Client) ScoreIdentityMatches(
	ctx context.Context, limit *int64,
) (*generated.PersonMatchDryRunResponse, error) {
	return c.RunIdentityScoring(ctx, limit, true)
}

func (c *Client) RunIdentityScoring(
	ctx context.Context, limit *int64, dryRun bool,
) (*generated.PersonMatchDryRunResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.RunPersonMatchScoringResp, error) {
		return api.RunPersonMatchScoringWithResponse(ctx, &generated.RunPersonMatchScoringRequestOptions{
			Body: &generated.PersonMatchDryRunRequest{DryRun: dryRun, Limit: limit},
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
