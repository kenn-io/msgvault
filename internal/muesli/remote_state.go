package muesli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.kenn.io/msgvault/internal/meetingimport"
)

type remoteUploads struct {
	*meetingimport.RecorderUploads[int64]
}

const remoteUploadsVersion = 2

func remoteUploadsPath(lockDir, canonicalDB, identifier, target string) string {
	return meetingimport.RecorderUploadsPath(lockDir, canonicalDB, identifier, target)
}
func loadRemoteUploads(path string) (*remoteUploads, error) {
	u, err := meetingimport.LoadRecorderUploads[int64](path)
	return &remoteUploads{u}, err
}
func meetingDigest(m *RemoteMeeting) (string, error)               { return meetingimport.MeetingDigest(m) }
func (u *remoteUploads) acknowledged(id int64, digest string) bool { return u.Acknowledged(id, digest) }
func (u *remoteUploads) record(id int64, digest string)            { u.Record(id, digest) }
func (u *remoteUploads) recordingAcknowledged(id, hash string) bool {
	return u.RecordingAcknowledged(id, hash)
}
func (u *remoteUploads) recordRecording(id, hash string) { u.RecordRecording(id, hash) }
func (u *remoteUploads) forgetRecording(id string)       { u.ForgetRecording(id) }
func (u *remoteUploads) save() error                     { return u.Save() }

// ForgetRemoteUploads clears the record of acknowledged meetings, so the next
// scan offers every meeting to the daemon. Registration calls it because a
// re-registered source may no longer hold the earlier meetings.
func ForgetRemoteUploads(ctx context.Context, opts ImportOptions) error {
	canonical, lockDir, err := remoteSyncPaths(opts.DBPath, opts.LockDir)
	if err != nil {
		return err
	}
	lock, err := lockRemoteSource(ctx, canonical, lockDir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	path := remoteUploadsPath(lockDir, canonical, opts.Identifier, opts.RemoteTarget)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear Muesli upload record: %w", err)
	}
	return nil
}
