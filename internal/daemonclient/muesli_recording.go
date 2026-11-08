package daemonclient

import (
	"context"

	"go.kenn.io/msgvault/internal/muesli"
)

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
	return sendRecorderRecording[muesli.RemoteResult](ctx, c, "/api/v1/import/muesli/recording", recorderUploadOptions{mediaType: upload.MIMEType, query: map[string]any{"identifier": upload.Source.Identifier, "account_email": upload.Source.AccountEmail, "message_id": upload.MessageID, "sha256": upload.Hash, "size": upload.Size, "no_build_cache": upload.NoBuildCache}}, recording, muesliRecorderErrors)
}
