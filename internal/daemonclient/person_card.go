package daemonclient

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/personcard"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// GetPersonCard reads the daemon's deterministic projection without changing
// profile data, archive state, or any external service.
func (c *Client) GetPersonCard(ctx context.Context, ref string, opts personcard.Options) (personcard.PersonCardResult, error) {
	query := generated.GetPersonCardQuery{}
	if !opts.AsOf.IsZero() {
		value := opts.AsOf.Format(time.RFC3339Nano)
		query.AsOf = &value
	}
	if opts.MaxItems != 0 {
		value := int64(opts.MaxItems)
		query.MaxItems = &value
	}
	if opts.MaxBytes != 0 {
		value := int64(opts.MaxBytes)
		query.MaxBytes = &value
	}
	if opts.Format != "" {
		value := string(opts.Format)
		query.Format = &value
	}
	if opts.Affiliation != "" {
		query.Affiliation = &opts.Affiliation
	}
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetPersonCardResp, error) {
		return client.GetPersonCardWithResponse(ctx, &generated.GetPersonCardRequestOptions{PathParams: &generated.GetPersonCardPath{Ref: ref}, Query: &query})
	})
	if err != nil {
		return personcard.PersonCardResult{}, err
	}
	if response.JSON200 == nil {
		return personcard.PersonCardResult{}, errors.New("get person card: empty response")
	}
	result, err := decodeMeetingResponse[personcard.PersonCardResult]("get person card", response.Body)
	if err != nil {
		return personcard.PersonCardResult{}, err
	}
	return *result, nil
}
