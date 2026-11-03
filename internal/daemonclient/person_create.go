package daemonclient

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"

	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// CreateStandalonePerson creates a profile through the daemon without promotion
// or publication. CLI and MCP share this generated API path.
func (c *Client) CreateStandalonePerson(ctx context.Context, input store.PersonCreateInput) (*generated.Person, error) {
	var body generated.CreateStandalonePersonBody
	data, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode person creation: %w", err)
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("convert person creation: %w", err)
	}
	resp, err := APIResponseWithStatuses(c, []int{http.StatusCreated},
		func(client *apiclient.Client) (*generated.CreateStandalonePersonResp, error) {
			return client.CreateStandalonePersonWithResponse(ctx,
				&generated.CreateStandalonePersonRequestOptions{Body: &body})
		})
	if err != nil {
		return nil, err
	}
	return resp.JSON201, nil
}

// CreatePerson implements the MCP person creator through the daemon API.
func (b *PeopleBrowser) CreatePerson(
	ctx context.Context, input store.PersonCreateInput,
) (*store.Person, error) {
	person, err := b.engine.store.CreateStandalonePerson(ctx, input)
	if err != nil {
		return nil, err
	}
	return personFromGenerated(person), nil
}
