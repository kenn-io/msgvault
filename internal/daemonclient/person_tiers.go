package daemonclient

import (
	"context"
	"errors"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) PreviewPersonTiers(ctx context.Context) (generated.PersonTierPreview, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.PreviewPersonTiersResp, error) {
		return client.PreviewPersonTiersWithResponse(ctx)
	})
	if err != nil {
		return generated.PersonTierPreview{}, err
	}
	if resp.JSON200 == nil {
		return generated.PersonTierPreview{}, errors.New("empty person tier preview response")
	}
	return *resp.JSON200, nil
}

func (c *Client) ExplainPersonTier(ctx context.Context, personID int64) (generated.PersonTierExplanation, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.ExplainPersonTierResp, error) {
		return client.ExplainPersonTierWithResponse(ctx, &generated.ExplainPersonTierRequestOptions{
			PathParams: &generated.ExplainPersonTierPath{ID: personID},
		})
	})
	if err != nil {
		return generated.PersonTierExplanation{}, err
	}
	if resp.JSON200 == nil {
		return generated.PersonTierExplanation{}, errors.New("empty person tier explanation response")
	}
	return *resp.JSON200, nil
}
