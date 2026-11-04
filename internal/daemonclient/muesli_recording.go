package daemonclient

import (
	"context"

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
	return sendRecorderRecording[muesli.RemoteResult](ctx, c, "/api/v1/import/muesli/recording", muesliRecordingOptions{upload: upload}, recording, muesliRecorderErrors)
}
