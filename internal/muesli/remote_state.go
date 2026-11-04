package muesli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.kenn.io/msgvault/internal/meetingimport"
)

// ForgetRemoteUploads clears the record of acknowledged meetings, so the next
// scan offers every meeting to the daemon. Registration calls it because a
// re-registered source may no longer hold the earlier meetings.
func ForgetRemoteUploads(ctx context.Context, opts ImportOptions) error {
	canonical, lockDir, err := meetingimport.RecorderSyncPaths(opts.DBPath, opts.LockDir)
	if err != nil {
		return err
	}
	lock, err := meetingimport.LockRecorderSource(ctx, canonical, lockDir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	path := meetingimport.RecorderUploadsPath(lockDir, canonical, opts.Identifier, opts.RemoteTarget)
	if _, err := meetingimport.LoadRecorderUploads[int64](path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear Muesli upload record: %w", err)
	}
	return nil
}
