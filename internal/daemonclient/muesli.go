package daemonclient

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v7"
	"go.kenn.io/msgvault/internal/muesli"
)

// muesliRequestOptions retains the exact normalized body across gate retries.
type muesliRequestOptions struct{ body jsontext.Value }

func (o muesliRequestOptions) GetPathParams() (map[string]any, error) { return map[string]any{}, nil }
func (o muesliRequestOptions) GetQuery() (map[string]any, error)      { return map[string]any{}, nil }
func (o muesliRequestOptions) GetBody() any                           { return o.body }
func (o muesliRequestOptions) GetHeader() (map[string]string, error) {
	return map[string]string{"Content-Type": "application/json"}, nil
}

// ImportMuesli uses the normal owner credentials and resends the same body
// while the daemon's operation gate is busy or its off-host rate limit
// answers 429, until ctx ends. A lost response is recovered by rescanning the
// same stable meeting key.
func (c *Client) ImportMuesli(ctx context.Context, req muesli.RemoteRequest) (muesli.RemoteResult, error) {
	normalized, err := req.Normalize()
	if err != nil {
		return muesli.RemoteResult{}, err
	}
	body, err := json.Marshal(normalized)
	if err != nil {
		return muesli.RemoteResult{}, muesli.ErrRemoteMalformed
	}
	if int64(len(body)) > muesli.MaxRemoteRequestBytes {
		return muesli.RemoteResult{}, muesli.ErrRemoteTooLarge
	}
	options := muesliRequestOptions{body: body}
	waiter := &operationBusyWaiter{c: c}
	result, err := backoff.Retry(ctx, func() (muesli.RemoteResult, error) {
		resp, err := c.DoGeneratedRequestWithContext(ctx, http.MethodPost, "/api/v1/import/muesli", options)
		if err != nil {
			return muesli.RemoteResult{}, backoff.Permanent(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusTooManyRequests {
			// A first sync can upload faster than the daemon admits off-host
			// requests; pausing keeps later meetings in the same scan.
			return muesli.RemoteResult{}, backoff.RetryAfter(retryAfterDelay(resp.Header.Get("Retry-After")), HandleErrorResponse(resp))
		}
		result, err := decodeMuesliResponse(resp)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return muesli.RemoteResult{}, backoff.Permanent(ctxErr)
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
		return muesli.RemoteResult{}, ctx.Err()
	}
	return muesli.RemoteResult{}, retryErr.LastErr
}

// retryAfterDelay reads a Retry-After seconds value, using the busy retry
// delay when the daemon omits it.
func retryAfterDelay(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return operationBusyRetryDelay
	}
	return time.Duration(seconds) * time.Second
}

func decodeMuesliResponse(resp *http.Response) (muesli.RemoteResult, error) {
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		switch resp.StatusCode {
		case http.StatusUnprocessableEntity:
			data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
			var body apiErrorBody
			if err == nil && len(data) <= 4096 && json.Unmarshal(data, &body) == nil && body.Error == "record_validation_failed" {
				return muesli.RemoteResult{}, muesli.ErrRemoteRecordValidation
			}
			return muesli.RemoteResult{}, &APIError{Status: resp.StatusCode, Code: "validation_failed", Message: "remote daemon rejected Muesli transfer; check source registration and recorder identity"}
		case http.StatusRequestEntityTooLarge:
			return muesli.RemoteResult{}, muesli.ErrRemoteTooLarge
		}
		return muesli.RemoteResult{}, HandleErrorResponse(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil {
		return muesli.RemoteResult{}, err
	}
	var result muesli.RemoteResult
	if len(data) > 4096 || json.Unmarshal(data, &result) != nil {
		return result, errors.New("invalid Muesli import response")
	}
	return result, nil
}
