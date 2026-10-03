package daemonclient

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"time"

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

// ImportMuesli uses the normal owner credentials and cancellable gate retry.
// A lost response is recovered by rescanning the same stable meeting key.
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
	for {
		if err := ctx.Err(); err != nil {
			return muesli.RemoteResult{}, err
		}
		resp, err := c.DoGeneratedRequestWithContext(ctx, http.MethodPost, "/api/v1/import/muesli", options)
		if err != nil {
			return muesli.RemoteResult{}, err
		}
		result, err := decodeMuesliResponse(resp)
		_ = resp.Body.Close()
		if _, busy := errors.AsType[*OperationInProgressError](err); !busy {
			return result, err
		}
		waiter.notify(err, operationBusyRetryDelay)
		timer := time.NewTimer(operationBusyRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return muesli.RemoteResult{}, ctx.Err()
		case <-timer.C:
		}
	}
}
func decodeMuesliResponse(resp *http.Response) (muesli.RemoteResult, error) {
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		switch resp.StatusCode {
		case http.StatusUnprocessableEntity:
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
