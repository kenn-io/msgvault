package daemonclient

import (
	"context"
	"errors"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// GetPersonTier explains the tier inferred from archived interactions with a person.
func (c *Client) GetPersonTier(ctx context.Context, personID int64) (generated.PersonTierExplanation, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.GetPersonTierResp, error) {
		return api.GetPersonTierWithResponse(ctx, &generated.GetPersonTierRequestOptions{PathParams: &generated.GetPersonTierPath{ID: personID}})
	})
	if err != nil {
		return generated.PersonTierExplanation{}, err
	}
	if response.JSON200 == nil {
		return generated.PersonTierExplanation{}, errors.New("person tier explanation response was empty")
	}
	return *response.JSON200, nil
}
