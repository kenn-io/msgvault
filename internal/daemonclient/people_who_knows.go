package daemonclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"go.kenn.io/msgvault/internal/whoknows"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const WhoKnowsMinAPISchemaVersion = "3.24.0"

// FindWhoKnows forwards the request to the daemon, which owns retrieval and ranking.
func (b *PeopleBrowser) FindWhoKnows(ctx context.Context, topic string, p whoknows.Params) (*whoknows.Response, error) {
	response, err := APIResponse(b.engine.store, func(client *apiclient.Client) (*generated.FindPeopleWhoKnowResp, error) {
		return client.FindPeopleWhoKnowWithResponse(ctx, WhoKnowsRequestOptions(topic, p))
	})
	if err != nil {
		return nil, err
	}
	if len(response.Body) == 0 {
		return nil, errors.New("empty who-knows response")
	}
	var result whoknows.Response
	if err := json.Unmarshal(response.Body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
