package daemonclient

import (
	"context"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/msgvault/internal/businessreviews"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// ControlBusinessReviews uses the generated authenticated route. APIResponse
// retries only explicit pre-admission busy responses, never an unknown attempt.
func (c *Client) ControlBusinessReviews(ctx context.Context, request businessreviews.Request) (*businessreviews.Result, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, businessreviews.ErrInvalid
	}
	var body generated.ControlBusinessReviewsBody
	if json.Unmarshal(data, &body) != nil {
		return nil, businessreviews.ErrInvalid
	}
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.ControlBusinessReviewsResp, error) {
		return client.ControlBusinessReviewsWithResponse(ctx, &generated.ControlBusinessReviewsRequestOptions{Body: &body})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("business review response unavailable; use the pre-issued receipt to check an attempted confirmation")
	}
	var result businessreviews.Result
	if json.Unmarshal(response.Body, &result) != nil {
		return nil, errors.New("business review response invalid; use the pre-issued receipt to check an attempted confirmation")
	}
	return &result, nil
}
