package daemonclient

import (
	"context"
	"encoding/json/v2"

	"go.kenn.io/msgvault/internal/vector"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) EmbeddingStatus(ctx context.Context, sourceID int64) (*vector.EmbeddingStatus, error) {
	options := &generated.GetEmbeddingStatusRequestOptions{}
	if sourceID > 0 {
		options.Query = &generated.GetEmbeddingStatusQuery{SourceID: &sourceID}
	}
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetEmbeddingStatusResp, error) {
		return client.GetEmbeddingStatusWithResponse(ctx, options)
	})
	if err != nil {
		return nil, err
	}
	var status vector.EmbeddingStatus
	if err := json.Unmarshal(response.Body, &status); err != nil {
		return nil, err
	}
	return &status, nil
}
