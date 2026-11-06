package muesli

import (
	"context"

	"go.kenn.io/msgvault/internal/meetingimport"
)

// ForgetRemoteUploads reoffers supported receipts after re-registration.
func ForgetRemoteUploads(ctx context.Context, opts ImportOptions) error {
	return meetingimport.ForgetRecorderUploads[int64](ctx, opts.DBPath, opts.LockDir, opts.Identifier, opts.RemoteTarget)
}
