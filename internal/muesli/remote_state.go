package muesli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/kit/atomicfile"
)

// remoteUploads remembers acknowledged meeting payloads and recording hashes,
// so scheduled rescans only send new or changed data. It is a cache: losing it
// costs a full re-upload, never archive data.
type remoteUploads struct {
	path       string
	Version    int               `json:"version"`
	Meetings   map[int64]string  `json:"meetings"`
	Recordings map[string]string `json:"recordings"`
	dirty      bool
}

const remoteUploadsVersion = 2

// remoteUploadsPath scopes the record to one database, source, and daemon so
// pointing the recorder at another archive uploads everything again.
func remoteUploadsPath(lockDir, canonicalDB, identifier, target string) string {
	hash := sha256.Sum256([]byte(canonicalDB + "\x00" + identifier + "\x00" + target))
	return filepath.Join(lockDir, hex.EncodeToString(hash[:])+".uploads.json")
}

func loadRemoteUploads(path string) (*remoteUploads, error) {
	uploads := &remoteUploads{
		path: path, Version: remoteUploadsVersion,
		Meetings: map[int64]string{}, Recordings: map[string]string{},
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return uploads, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Muesli upload record: %w", err)
	}
	var stored remoteUploads
	if json.Unmarshal(data, &stored) != nil || stored.Meetings == nil {
		// An unreadable cache only means every meeting is offered again; the
		// next save replaces it.
		uploads.dirty = true
		return uploads, nil //nolint:nilerr // A corrupt cache must not block sync.
	}
	if stored.Version == 1 {
		// Version 1 keyed recording acknowledgements only by Muesli row ID.
		// A database reset can reuse that ID, so preserve meeting offers but
		// invalidate every ambiguous recording receipt.
		uploads.Meetings = stored.Meetings
		uploads.dirty = true
		return uploads, nil
	}
	if stored.Version != remoteUploadsVersion {
		uploads.dirty = true
		return uploads, nil
	}
	uploads.Meetings = stored.Meetings
	if stored.Recordings != nil {
		uploads.Recordings = stored.Recordings
	}
	return uploads, nil
}

func meetingDigest(m *RemoteMeeting) (string, error) {
	encoded, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

func (u *remoteUploads) acknowledged(id int64, digest string) bool {
	return u.Meetings[id] == digest
}

func (u *remoteUploads) record(id int64, digest string) {
	if u.Meetings[id] != digest {
		u.Meetings[id] = digest
		u.dirty = true
	}
}

func (u *remoteUploads) recordingAcknowledged(identity, hash string) bool {
	return u.Recordings[identity] == hash
}

func (u *remoteUploads) recordRecording(identity, hash string) {
	if u.Recordings[identity] != hash {
		u.Recordings[identity] = hash
		u.dirty = true
	}
}

func (u *remoteUploads) forgetRecording(identity string) {
	if _, ok := u.Recordings[identity]; ok {
		delete(u.Recordings, identity)
		u.dirty = true
	}
}

func (u *remoteUploads) save() error {
	if !u.dirty {
		return nil
	}
	data, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("encode Muesli upload record: %w", err)
	}
	if err := atomicfile.WriteFile(u.path, data, atomicfile.WithPrivate()); err != nil {
		return fmt.Errorf("save Muesli upload record: %w", err)
	}
	u.dirty = false
	return nil
}

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
