package daemonclient

import (
	"context"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/msgvault/internal/googlereviews"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// ControlGoogleReviews uses the generated authenticated route. APIResponse
// retries only explicit pre-admission busy responses, never an unknown attempt.
func (c *Client) ControlGoogleReviews(ctx context.Context, request googlereviews.Request) (*googlereviews.Result, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, googlereviews.ErrInvalid
	}
	var body generated.ControlGoogleReviewsBody
	if json.Unmarshal(data, &body) != nil {
		return nil, googlereviews.ErrInvalid
	}
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.ControlGoogleReviewsResp, error) {
		return client.ControlGoogleReviewsWithResponse(ctx, &generated.ControlGoogleReviewsRequestOptions{Body: &body})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("google review response unavailable; use the pre-issued receipt to check an attempted confirmation")
	}
	var result googlereviews.Result
	if json.Unmarshal(response.Body, &result) != nil {
		return nil, errors.New("google review response invalid; use the pre-issued receipt to check an attempted confirmation")
	}
	return &result, nil
}

// SearchGoogleReviewsArchive reads the dedicated review archive endpoint.
// The generated API request builder still supplies authentication and tracing
// editors; the response is decoded into the feature's stable domain model.
func (c *Client) SearchGoogleReviewsArchive(ctx context.Context, request googlereviews.ArchiveSearchRequest) (*googlereviews.ArchiveSearchResponse, error) {
	page, pageSize := int64(request.Page), int64(request.PageSize)
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.SearchGoogleReviewsArchiveResp, error) {
		return client.SearchGoogleReviewsArchiveWithResponse(ctx, &generated.SearchGoogleReviewsArchiveRequestOptions{
			Query: &generated.SearchGoogleReviewsArchiveQuery{
				Q: request.Query, Target: optionalGoogleReviewString(request.Target),
				Page: optionalGoogleReviewInt(page), PageSize: optionalGoogleReviewInt(pageSize),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("google Reviews archive search response unavailable")
	}
	var result googlereviews.ArchiveSearchResponse
	if json.Unmarshal(response.Body, &result) != nil {
		return nil, errors.New("google Reviews archive response was invalid")
	}
	return &result, nil
}

func optionalGoogleReviewString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func optionalGoogleReviewInt(value int64) *int64 {
	if value == 0 {
		return nil
	}
	return &value
}
