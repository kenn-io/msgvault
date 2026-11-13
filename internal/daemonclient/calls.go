package daemonclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/cenkalti/backoff/v7"

	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) ImportCallObservation(ctx context.Context, data []byte) (*generated.CallImportResponse, error) {
	resp, err := retryCallRateLimit(ctx, func() (*generated.ImportCallObservationResp, string, error) {
		var retryAfter string
		resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.ImportCallObservationResp, error) {
			resp, err := client.ImportCallObservationWithResponse(ctx, &generated.ImportCallObservationRequestOptions{}, func(_ context.Context, req *http.Request) error {
				req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
				req.Body, _ = req.GetBody()
				req.ContentLength = int64(len(data))
				return nil
			})
			if resp != nil && resp.HTTPResponse != nil {
				retryAfter = resp.HTTPResponse.Header.Get("Retry-After")
			}
			return resp, err
		})
		return resp, retryAfter, err
	})
	if err != nil {
		return nil, err
	}
	return resp.JSON200, nil
}

func (c *Client) ListCallObservations(ctx context.Context, q store.CallQuery) (*generated.CallEvidenceResponse, error) {
	resp, err := retryCallRateLimit(ctx, func() (*generated.ListCallObservationsResp, string, error) {
		var retryAfter string
		resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.ListCallObservationsResp, error) {
			resp, err := client.ListCallObservationsWithResponse(ctx, &generated.ListCallObservationsRequestOptions{
				Query: &generated.ListCallObservationsQuery{
					PersonID: &q.PersonID, Limit: new(int64(q.Limit)), AfterID: &q.AfterID,
					After: q.After, Before: q.Before,
				},
			})
			if resp != nil && resp.HTTPResponse != nil {
				retryAfter = resp.HTTPResponse.Header.Get("Retry-After")
			}
			return resp, err
		})
		return resp, retryAfter, err
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

// retryCallRateLimit preserves the generated client and retries the same call
// when the daemon asks the caller to wait. APIResponse handles a busy gate.
func retryCallRateLimit[T any](ctx context.Context, request func() (T, string, error)) (T, error) {
	var zero T
	result, err := backoff.Retry(ctx, func() (T, error) {
		result, retryAfter, err := request()
		if err == nil {
			return result, nil
		}
		apiErr, ok := errors.AsType[*APIError](err)
		if !ok || apiErr.Status != http.StatusTooManyRequests {
			return zero, backoff.Permanent(err)
		}
		return zero, backoff.RetryAfter(retryAfterDelay(retryAfter), err)
	}, backoff.WithBackOff(backoff.NewConstantBackOff(operationBusyRetryDelay)), backoff.WithMaxTries(0), backoff.WithMaxElapsedTime(0))
	if err == nil {
		return result, nil
	}
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	return zero, backoff.AsRetryError(err).LastErr
}
