package daemonclient

import (
	"context"
	"net/http"

	"go.kenn.io/msgvault/internal/muesli"
)

var muesliRecorderErrors = recorderErrors{"Muesli", muesli.ErrRemoteMalformed, muesli.ErrRemoteTooLarge, muesli.ErrRemoteRecordValidation}

// ImportMuesli sends validated recorder-local evidence with gate and rate-limit retries.
func (c *Client) ImportMuesli(ctx context.Context, req muesli.RemoteRequest) (muesli.RemoteResult, error) {
	normalized, err := req.Normalize()
	if err != nil {
		return muesli.RemoteResult{}, err
	}
	return sendRecorder[muesli.RemoteResult](ctx, c, "/api/v1/import/muesli", normalized, muesliRecorderErrors)
}
func decodeMuesliResponse(resp *http.Response) (muesli.RemoteResult, error) {
	return decodeRecorderResponse[muesli.RemoteResult](resp, muesliRecorderErrors)
}
