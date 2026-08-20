package daemonclient

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/query"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type PersonFileSearchOptions struct {
	PersonID     int64
	Directions   []personscope.Direction
	After        *time.Time
	Before       *time.Time
	Filename     string
	MIMEFamilies []query.FileMIMEFamily
	Limit        int
	Cursor       string
}

func (c *Client) SearchPersonFiles(
	ctx context.Context,
	request PersonFileSearchOptions,
) (generated.PersonFileSearchHTTPResponse, error) {
	directions := make([]generated.PersonFileSearchHTTPRequestDirections, len(request.Directions))
	for i, direction := range request.Directions {
		directions[i] = generated.PersonFileSearchHTTPRequestDirections(direction)
	}
	mimeFamilies := make([]string, len(request.MIMEFamilies))
	for i, family := range request.MIMEFamilies {
		mimeFamilies[i] = string(family)
	}
	filters := make([]generated.ExploreFilter, 0, 2)
	if request.After != nil {
		filters = append(filters, generated.ExploreFilter{
			Dimension: generated.ExploreFilterDimensionAfter,
			Values:    []string{request.After.UTC().Format(time.RFC3339Nano)},
		})
	}
	if request.Before != nil {
		filters = append(filters, generated.ExploreFilter{
			Dimension: generated.ExploreFilterDimensionBefore,
			Values:    []string{request.Before.UTC().Format(time.RFC3339Nano)},
		})
	}
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.SearchPersonFilesResp, error) {
		return client.SearchPersonFilesWithResponse(ctx, &generated.SearchPersonFilesRequestOptions{
			PathParams: &generated.SearchPersonFilesPath{ID: request.PersonID},
			Body: &generated.PersonFileSearchHTTPRequest{
				Predicate:  generated.ExploreHTTPRequest{Filters: filters},
				Directions: directions, FilenameQuery: optionalString(request.Filename),
				MimeFamilies: mimeFamilies, Limit: optionalPositiveInt64(request.Limit),
				Cursor: optionalString(request.Cursor),
				Sort:   generated.FileSearchSort{Field: "occurred_at", Direction: "desc"},
			},
		})
	})
	if err != nil {
		return generated.PersonFileSearchHTTPResponse{}, err
	}
	if response.JSON200 == nil {
		return generated.PersonFileSearchHTTPResponse{Files: []generated.PersonFileSearchRow{}}, nil
	}
	return *response.JSON200, nil
}
