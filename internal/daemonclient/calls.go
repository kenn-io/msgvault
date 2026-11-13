package daemonclient

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) ImportCallObservation(ctx context.Context, data []byte) (*generated.CallImportResponse, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.ImportCallObservationResp, error) {
		return client.ImportCallObservationWithResponse(ctx, &generated.ImportCallObservationRequestOptions{}, func(_ context.Context, req *http.Request) error {
			req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
			req.Body, _ = req.GetBody()
			req.ContentLength = int64(len(data))
			return nil
		})
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
