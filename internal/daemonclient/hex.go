package daemonclient

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"

	"github.com/cenkalti/backoff/v7"
	"go.kenn.io/msgvault/internal/hex"
)

// hexRequestOptions retains the exact normalized body across gate retries.
type hexRequestOptions struct{ body jsontext.Value }

func (o hexRequestOptions) GetPathParams() (map[string]any, error) { return map[string]any{}, nil }
func (o hexRequestOptions) GetQuery() (map[string]any, error)      { return map[string]any{}, nil }
func (o hexRequestOptions) GetBody() any                           { return o.body }
func (o hexRequestOptions) GetHeader() (map[string]string, error) {
	return map[string]string{"Content-Type": "application/json"}, nil
}

// ImportHex uses the normal owner credentials and resends the same body
// while the daemon's operation gate is busy or its off-host rate limit
// answers 429, until ctx ends. A lost response is recovered by rescanning the
// same stable meeting key.
func (c *Client) ImportHex(ctx context.Context, req hex.RemoteRequest) (hex.RemoteResult, error) {
	normalized, err := req.Normalize()
	if err != nil {
		return hex.RemoteResult{}, err
	}
	body, err := json.Marshal(normalized)
	if err != nil {
		return hex.RemoteResult{}, hex.ErrRemoteMalformed
	}
	if int64(len(body)) > hex.MaxRemoteRequestBytes {
		return hex.RemoteResult{}, hex.ErrRemoteTooLarge
	}
	options := hexRequestOptions{body: body}
	waiter := &operationBusyWaiter{c: c}
	result, err := backoff.Retry(ctx, func() (hex.RemoteResult, error) {
		resp, err := c.DoGeneratedRequestWithContext(ctx, http.MethodPost, "/api/v1/import/hex", options)
		if err != nil {
			return hex.RemoteResult{}, backoff.Permanent(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusTooManyRequests {
			// A first sync can upload faster than the daemon admits off-host
			// requests; pausing keeps later meetings in the same scan.
			return hex.RemoteResult{}, backoff.RetryAfter(retryAfterDelay(resp.Header.Get("Retry-After")), HandleErrorResponse(resp))
		}
		result, err := decodeHexResponse(resp)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return hex.RemoteResult{}, backoff.Permanent(ctxErr)
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
		return hex.RemoteResult{}, ctx.Err()
	}
	return hex.RemoteResult{}, retryErr.LastErr
}

func decodeHexResponse(resp *http.Response) (hex.RemoteResult, error) {
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		switch resp.StatusCode {
		case http.StatusUnprocessableEntity:
			data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
			var body apiErrorBody
			if err == nil && len(data) <= 4096 && json.Unmarshal(data, &body) == nil && body.Error == "record_validation_failed" {
				return hex.RemoteResult{}, hex.ErrRemoteRecordValidation
			}
			return hex.RemoteResult{}, &APIError{Status: resp.StatusCode, Code: "validation_failed", Message: "remote daemon rejected Hex transfer; check source registration and recorder identity"}
		case http.StatusRequestEntityTooLarge:
			return hex.RemoteResult{}, hex.ErrRemoteTooLarge
		}
		return hex.RemoteResult{}, HandleErrorResponse(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil {
		return hex.RemoteResult{}, err
	}
	var result hex.RemoteResult
	if len(data) > 4096 || json.Unmarshal(data, &result) != nil {
		return result, errors.New("invalid Hex import response")
	}
	return result, nil
}
