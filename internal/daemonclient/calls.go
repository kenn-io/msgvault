package daemonclient

import (
	"context"
	"encoding/json/v2"

	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) ImportCallObservation(ctx context.Context, data []byte) (*generated.CallImportResponse, error) {
	var body generated.CallImportRequest
	if err := json.Unmarshal(data, &body, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.ImportCallObservationResp, error) {
		return client.ImportCallObservationWithResponse(ctx, &generated.ImportCallObservationRequestOptions{Body: &body})
	})
	if err != nil {
		return nil, err
	}
	return resp.JSON200, nil
}

func (c *Client) ListCallObservations(ctx context.Context, q store.CallQuery) (*generated.CallEvidenceResponse, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.ListCallObservationsResp, error) {
		return client.ListCallObservationsWithResponse(ctx, &generated.ListCallObservationsRequestOptions{
			Query: &generated.ListCallObservationsQuery{
				PersonID: &q.PersonID, Limit: new(int64(q.Limit)), AfterID: &q.AfterID,
				After: q.After, Before: q.Before,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return resp.JSON200, nil
}

func (c *Client) SummarizeCalls(ctx context.Context, q store.CallQuery) (*generated.CallSummaryResponse, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.SummarizeCallsResp, error) {
		return client.SummarizeCallsWithResponse(ctx, &generated.SummarizeCallsRequestOptions{
			Query: &generated.SummarizeCallsQuery{
				PersonID: &q.PersonID, Limit: new(int64(q.Limit)), Offset: new(int64(q.Offset)),
				After: q.After, Before: q.Before,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return resp.JSON200, nil
}
