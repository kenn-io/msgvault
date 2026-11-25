package daemonclient

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v7"
	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingimport"
)

type recorderErrors struct {
	name                     string
	malformed, large, record error
}
type recorderRequestOptions struct{ body jsontext.Value }

func (o recorderRequestOptions) GetPathParams() (map[string]any, error) { return map[string]any{}, nil }
func (o recorderRequestOptions) GetQuery() (map[string]any, error)      { return map[string]any{}, nil }
func (o recorderRequestOptions) GetBody() any                           { return o.body }
func (o recorderRequestOptions) GetHeader() (map[string]string, error) {
	return map[string]string{"Content-Type": "application/json"}, nil
}

// retryRecorder applies the same gate, rate-limit and cancellation contract to JSON and audio.
func retryRecorder[T any](ctx context.Context, c *Client, send func() (*http.Response, error), decode func(*http.Response) (T, error)) (T, error) {
	var zero T
	waiter := &operationBusyWaiter{c: c}
	result, err := backoff.Retry(ctx, func() (T, error) {
		resp, err := send()
		if err != nil {
			return zero, backoff.Permanent(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusTooManyRequests {
			return zero, backoff.RetryAfter(retryAfterDelay(resp.Header.Get("Retry-After")), HandleErrorResponse(resp))
		}
		result, err := decode(resp)
		if ctx.Err() != nil {
			return zero, backoff.Permanent(ctx.Err())
		}
		if _, busy := errors.AsType[*OperationInProgressError](err); err != nil && !busy {
			return result, backoff.Permanent(err)
		}
		return result, err
	}, backoff.WithBackOff(backoff.NewConstantBackOff(operationBusyRetryDelay)), backoff.WithMaxTries(0), backoff.WithMaxElapsedTime(0), backoff.WithNotify(waiter.notify))
	if err == nil {
		return result, nil
	}
	failure := backoff.AsRetryError(err)
	if !errors.Is(failure.Cause, backoff.ErrPermanent) {
		return zero, ctx.Err()
	}
	return zero, failure.LastErr
}
func sendRecorder[T any](ctx context.Context, c *Client, path string, body any, e recorderErrors) (T, error) {
	var zero T
	encoded, err := json.Marshal(body)
	if err != nil {
		return zero, e.malformed
	}
	if int64(len(encoded)) > meetingimport.MaxRequestBytes {
		return zero, e.large
	}
	return retryRecorder(ctx, c, func() (*http.Response, error) {
		return c.DoGeneratedRequestWithContext(ctx, http.MethodPost, path, recorderRequestOptions{body: encoded})
	}, func(resp *http.Response) (T, error) { return decodeRecorderResponse[T](resp, e) })
}
func sendRecorderRecording[T any](ctx context.Context, c *Client, path string, options runtime.RequestOptions, recording *meetingarchive.Recording, e recorderErrors) error {
	client, err := c.GeneratedClient()
	if err != nil {
		return err
	}
	_, err = retryRecorder(ctx, c, func() (*http.Response, error) {
		req, err := client.APIClient().CreateRequest(ctx, runtime.RequestOptionsParameters{RequestURL: client.APIClient().GetBaseURL() + path, Method: http.MethodPost, Options: options})
		if err != nil {
			return nil, err
		}
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(io.NewSectionReader(recording.File, 0, recording.Size)), nil
		}
		req.Body, _ = req.GetBody()
		req.ContentLength = recording.Size
		return doRequestWithRootContext(c.requestContext(), httpClientWithoutTimeout(c.httpClient), req, maxErrorBodyBytes)
	}, func(resp *http.Response) (struct{}, error) {
		if resp.StatusCode == http.StatusRequestEntityTooLarge {
			return struct{}{}, meetingarchive.ErrRecordingTooLarge
		}
		_, err := decodeRecorderResponse[T](resp, e)
		return struct{}{}, err
	})
	return err
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

func decodeRecorderResponse[T any](resp *http.Response, e recorderErrors) (T, error) {
	var zero T
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		switch resp.StatusCode {
		case http.StatusUnprocessableEntity:
			data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
			var body apiErrorBody
			if err == nil && len(data) <= 4096 && json.Unmarshal(data, &body) == nil && body.Error == "record_validation_failed" {
				return zero, e.record
			}
			return zero, &APIError{Status: resp.StatusCode, Code: "validation_failed", Message: "remote daemon rejected " + e.name + " transfer; check source registration and recorder identity"}
		case http.StatusRequestEntityTooLarge:
			return zero, e.large
		}
		return zero, HandleErrorResponse(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil {
		return zero, err
	}
	var result T
	if len(data) > 4096 || json.Unmarshal(data, &result) != nil {
		return result, fmt.Errorf("invalid %s import response", e.name)
	}
	return result, nil
}

type recorderUploadOptions struct {
	query     map[string]any
	mediaType string
}

func (o recorderUploadOptions) GetPathParams() (map[string]any, error) { return map[string]any{}, nil }
func (o recorderUploadOptions) GetQuery() (map[string]any, error)      { return o.query, nil }
func (o recorderUploadOptions) GetBody() any                           { return nil }
func (o recorderUploadOptions) GetHeader() (map[string]string, error) {
	return map[string]string{"Content-Type": o.mediaType}, nil
}
