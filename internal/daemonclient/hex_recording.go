package daemonclient

import (
	"context"

	"go.kenn.io/msgvault/internal/hex"
	"go.kenn.io/msgvault/internal/meetingarchive"
)

type hexRecordingOptions struct{ upload hex.RecordingUpload }

func (o hexRecordingOptions) GetPathParams() (map[string]any, error) { return map[string]any{}, nil }
func (o hexRecordingOptions) GetQuery() (map[string]any, error) {
	u := o.upload
	return map[string]any{"track": u.Track, "identifier": u.Source.Identifier, "account_email": u.Source.AccountEmail, "message_id": u.MessageID, "sha256": u.Hash, "size": u.Size, "no_build_cache": u.NoBuildCache}, nil
}
func (o hexRecordingOptions) GetBody() any { return nil }
func (o hexRecordingOptions) GetHeader() (map[string]string, error) {
	return map[string]string{"Content-Type": o.upload.MIMEType}, nil
}

// ImportHexRecording streams the immutable private snapshot through the
// generated client's authenticated request construction. Busy/rate-limit
// retries rewind the snapshot; interrupted uploads are retried by a rescan.
func (c *Client) ImportHexRecording(ctx context.Context, upload hex.RecordingUpload, recording *meetingarchive.Recording) error {
	upload, err := upload.Normalize()
	if err != nil {
		return err
	}
	if recording == nil || recording.File == nil || upload.Hash != recording.Hash || upload.Size != recording.Size || upload.MIMEType != recording.MIMEType {
		return hex.ErrRemoteRecordValidation
	}
	return sendRecorderRecording[hex.RemoteResult](ctx, c, "/api/v1/import/hex/recording", hexRecordingOptions{upload: upload}, recording, hexRecorderErrors)
}
