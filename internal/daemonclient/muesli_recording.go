package daemonclient

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/cenkalti/backoff/v7"
	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/muesli"
)

type muesliRecordingOptions struct{ upload muesli.RecordingUpload }

func (o muesliRecordingOptions) GetPathParams() (map[string]any, error) { return map[string]any{}, nil }
func (o muesliRecordingOptions) GetQuery() (map[string]any, error) {
	u := o.upload
	return map[string]any{"identifier": u.Source.Identifier, "account_email": u.Source.AccountEmail, "message_id": u.MessageID, "sha256": u.Hash, "size": u.Size, "no_build_cache": u.NoBuildCache}, nil
}
func (o muesliRecordingOptions) GetBody() any { return nil }
func (o muesliRecordingOptions) GetHeader() (map[string]string, error) {
	return map[string]string{"Content-Type": o.upload.MIMEType}, nil
}

// ImportMuesliRecording streams the immutable private snapshot through the
// generated client's authenticated request construction. Busy/rate-limit
// retries rewind the snapshot; interrupted uploads are retried by a rescan.
func (c *Client) ImportMuesliRecording(ctx context.Context, upload muesli.RecordingUpload, recording *muesli.Recording) error {
	upload, err := upload.Normalize()
	if err != nil {
		return err
	}
	if recording == nil || recording.File == nil || upload.Hash != recording.Hash || upload.Size != recording.Size || upload.MIMEType != recording.MIMEType {
		return muesli.ErrRemoteRecordValidation
	}
	client, err := c.GeneratedClient()
	if err != nil {
		return err
	}
	waiter := &operationBusyWaiter{c: c}
	_, err = backoff.Retry(ctx, func() (struct{}, error) {
		req, err := client.APIClient().CreateRequest(ctx, runtime.RequestOptionsParameters{
			RequestURL: client.APIClient().GetBaseURL() + "/api/v1/import/muesli/recording", Method: http.MethodPost, Options: muesliRecordingOptions{upload: upload},
		})
		if err != nil {
			return struct{}{}, backoff.Permanent(err)
		}
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(io.NewSectionReader(recording.File, 0, recording.Size)), nil
		}
		req.Body, _ = req.GetBody()
		req.ContentLength = recording.Size
		resp, err := doRequestWithRootContext(c.requestContext(), httpClientWithoutTimeout(c.httpClient), req, maxErrorBodyBytes)
		if err != nil {
			return struct{}{}, backoff.Permanent(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusRequestEntityTooLarge {
			return struct{}{}, backoff.Permanent(muesli.ErrRecordingTooLarge)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return struct{}{}, backoff.RetryAfter(retryAfterDelay(resp.Header.Get("Retry-After")), HandleErrorResponse(resp))
		}
		_, err = decodeMuesliResponse(resp)
		if _, busy := errors.AsType[*OperationInProgressError](err); err != nil && !busy {
			return struct{}{}, backoff.Permanent(err)
		}
		return struct{}{}, err
	}, backoff.WithBackOff(backoff.NewConstantBackOff(operationBusyRetryDelay)), backoff.WithMaxTries(0), backoff.WithMaxElapsedTime(0), backoff.WithNotify(waiter.notify))
	if err == nil {
		return nil
	}
	retryErr := backoff.AsRetryError(err)
	if !errors.Is(retryErr.Cause, backoff.ErrPermanent) {
		return ctx.Err()
	}
	return retryErr.LastErr
}
