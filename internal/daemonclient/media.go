package daemonclient

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/personscope"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type MediaSearchOptions struct {
	Query      string
	Mode       string
	PersonID   int64
	Directions []personscope.Direction
	Limit      int
}

func (c *Client) SearchMedia(ctx context.Context, request MediaSearchOptions) (generated.MediaSearchResponse, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.SearchMediaResp, error) {
		return client.SearchMediaWithResponse(ctx, &generated.SearchMediaRequestOptions{Query: &generated.SearchMediaQuery{
			Q: request.Query, Mode: optionalString(request.Mode), PersonID: optionalPositiveInt64Value(request.PersonID),
			Direction: documentDirectionStrings(request.Directions), Limit: optionalPositiveInt64(request.Limit),
		}})
	})
	if err != nil {
		return generated.MediaSearchResponse{}, err
	}
	if response.JSON200 == nil {
		return generated.MediaSearchResponse{}, errors.New("media search: empty response")
	}
	return *response.JSON200, nil
}
