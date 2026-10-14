package daemonclient

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/query"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// PersonFileSuggestionOptions selects the explicit filename suggestion lane.
// A zero limit omits the field and uses the daemon's default of 20.
type PersonFileSuggestionOptions struct {
	PersonID      int64
	Filename      string
	Directions    []personscope.Direction
	After, Before *time.Time
	MIMEFamilies  []query.FileMIMEFamily
	Limit         int
}

func (c *Client) SuggestPersonFiles(ctx context.Context, options PersonFileSuggestionOptions) (generated.PersonFileSuggestionHTTPResponse, error) {
	if options.Limit < 0 || options.Limit > 100 {
		return generated.PersonFileSuggestionHTTPResponse{}, errors.New("filename suggestion limit must be between 1 and 100, or omitted")
	}
	directions := make([]generated.PersonFileSuggestionHTTPRequestDirections, len(options.Directions))
	for i, direction := range options.Directions {
		directions[i] = generated.PersonFileSuggestionHTTPRequestDirections(direction)
	}
	mimeFamilies := make([]generated.PersonFileSuggestionHTTPRequestMimeFamilies, len(options.MIMEFamilies))
	for i, family := range options.MIMEFamilies {
		mimeFamilies[i] = generated.PersonFileSuggestionHTTPRequestMimeFamilies(family)
	}
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.SuggestPersonFilesResp, error) {
		return client.SuggestPersonFilesWithResponse(ctx, &generated.SuggestPersonFilesRequestOptions{
			PathParams: &generated.SuggestPersonFilesPath{ID: options.PersonID},
			Body: &generated.PersonFileSuggestionHTTPRequest{
				FilenameQuery: options.Filename, Directions: directions,
				After: options.After, Before: options.Before, MimeFamilies: mimeFamilies,
				Limit: optionalPositiveInt64(options.Limit),
			},
		})
	})
	if err != nil {
		return generated.PersonFileSuggestionHTTPResponse{}, err
	}
	if response.JSON200 == nil {
		return generated.PersonFileSuggestionHTTPResponse{}, errors.New("filename suggestion response is empty")
	}
	return *response.JSON200, nil
}
