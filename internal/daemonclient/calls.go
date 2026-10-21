package daemonclient

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/cenkalti/backoff/v7"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/store"
)

type CallImportResult struct {
	MessageID int64 `json:"message_id"`
	SourceID  int64 `json:"source_id"`
}
type CallEvidencePage struct {
	Items []store.CallObservationView `json:"items"`
}
type CallSummaryPage struct {
	Items []store.PersonCallSummary `json:"items"`
}

type callOptions struct {
	body  any
	query map[string]any
}

func (o callOptions) GetPathParams() (map[string]any, error) { return map[string]any{}, nil }
func (o callOptions) GetQuery() (map[string]any, error)      { return o.query, nil }
func (o callOptions) GetBody() any                           { return o.body }
func (o callOptions) GetHeader() (map[string]string, error) {
	return map[string]string{"Content-Type": "application/json"}, nil
}

func callRequest[T any](ctx context.Context, c *Client, method, path string, options callOptions) (T, error) {
	var zero T
	resp, err := c.DoGeneratedRequestWithContext(ctx, method, path, options)
	if err != nil {
		return zero, err
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeCallResponse[T](resp)
}

// callRequestWithRetry resends the same request while the daemon's operation
// gate is busy or its rate limiter asks the client to wait.
func callRequestWithRetry[T any](ctx context.Context, c *Client, method, path string, options callOptions) (T, error) {
	var zero T
	waiter := &operationBusyWaiter{c: c}
	result, err := backoff.Retry(ctx, func() (T, error) {
		resp, err := c.DoGeneratedRequestWithContext(ctx, method, path, options)
		if err != nil {
			return zero, backoff.Permanent(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusTooManyRequests {
			return zero, backoff.RetryAfter(retryAfterDelay(resp.Header.Get("Retry-After")), HandleErrorResponse(resp))
		}
		result, err := decodeCallResponse[T](resp)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, backoff.Permanent(ctxErr)
		}
		if _, busy := errors.AsType[*OperationInProgressError](err); err != nil && !busy {
			return result, backoff.Permanent(err)
		}
		return result, err
	}, backoff.WithBackOff(backoff.NewConstantBackOff(operationBusyRetryDelay)),
		backoff.WithMaxTries(0), backoff.WithMaxElapsedTime(0), backoff.WithNotify(waiter.notify))
	if err == nil {
		return result, nil
	}
	retryErr := backoff.AsRetryError(err)
	if !errors.Is(retryErr.Cause, backoff.ErrPermanent) {
		return zero, ctx.Err()
	}
	return zero, retryErr.LastErr
}

func decodeCallResponse[T any](resp *http.Response) (T, error) {
	var result T
	if resp.StatusCode != http.StatusOK {
		return result, HandleErrorResponse(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil {
		return result, err
	}
	if len(body) > 16<<20 {
		return result, errors.New("call history response exceeds 16 MiB")
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return result, fmt.Errorf("decode call history response: %w", err)
	}
	return result, nil
}

func (c *Client) ImportCallObservation(ctx context.Context, data []byte) (CallImportResult, error) {
	if _, err := callhistory.DecodeImport(bytes.NewReader(data)); err != nil {
		return CallImportResult{}, err
	}
	return callRequestWithRetry[CallImportResult](ctx, c, http.MethodPost, "/api/v1/import/calls", callOptions{body: jsontext.Value(data)})
}

func callQueryValues(q store.CallQuery, summary bool) map[string]any {
	values := map[string]any{
		"person_id": q.PersonID, "limit": q.Limit,
	}
	if summary {
		values["offset"] = q.Offset
	} else {
		values["after_id"] = q.AfterID
	}
	if q.After != nil {
		values["after"] = q.After.Format(time.RFC3339Nano)
	}
	if q.Before != nil {
		values["before"] = q.Before.Format(time.RFC3339Nano)
	}
	return values
}

func (c *Client) ListCallObservations(ctx context.Context, q store.CallQuery) (CallEvidencePage, error) {
	if q.Offset != 0 {
		return CallEvidencePage{}, fmt.Errorf("%w: evidence pages require after_id", callhistory.ErrInvalid)
	}
	return callRequestWithRetry[CallEvidencePage](ctx, c, http.MethodGet, "/api/v1/calls", callOptions{query: callQueryValues(q, false)})
}

func (c *Client) SummarizeCalls(ctx context.Context, q store.CallQuery) (CallSummaryPage, error) {
	if q.AfterID != 0 {
		return CallSummaryPage{}, fmt.Errorf("%w: summary pages require offset", callhistory.ErrInvalid)
	}
	return callRequest[CallSummaryPage](ctx, c, http.MethodGet, "/api/v1/calls/summary", callOptions{query: callQueryValues(q, true)})
}
