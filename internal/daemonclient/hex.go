package daemonclient

import (
	"context"

	"go.kenn.io/msgvault/internal/hex"
)

var hexRecorderErrors = recorderErrors{"Hex", hex.ErrRemoteMalformed, hex.ErrRemoteTooLarge, hex.ErrRemoteRecordValidation}

// ImportHex sends validated recorder-local evidence with gate and rate-limit retries.
func (c *Client) ImportHex(ctx context.Context, req hex.RemoteRequest) (hex.RemoteResult, error) {
	normalized, err := req.Normalize()
	if err != nil {
		return hex.RemoteResult{}, err
	}
	return sendRecorder[hex.RemoteResult](ctx, c, "/api/v1/import/hex", normalized, hexRecorderErrors)
}
