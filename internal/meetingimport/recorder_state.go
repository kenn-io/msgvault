package meetingimport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/kit/atomicfile"
)

// RecorderUploads[K] remembers Acknowledged meeting payloads and recording hashes,
// so scheduled rescans only send new or changed data. It is a cache: losing it
// costs a full re-upload, never archive data.
type RecorderUploads[K comparable] struct {
	path       string
	Version    int               `json:"version"`
	Meetings   map[K]string      `json:"meetings"`
	Recordings map[string]string `json:"recordings"`
	Dirty      bool              `json:"-"`
}

const recorderUploadsVersion = 2

// RecorderUploadsPath scopes the record to one database, source, and daemon so
// pointing the recorder at another archive uploads everything again.
func RecorderUploadsPath(lockDir, canonicalDB, identifier, target string) string {
	hash := sha256.Sum256([]byte(canonicalDB + "\x00" + identifier + "\x00" + target))
	return filepath.Join(lockDir, hex.EncodeToString(hash[:])+".uploads.json")
}

func LoadRecorderUploads[K comparable](path string) (*RecorderUploads[K], error) {
	uploads := &RecorderUploads[K]{
		path: path, Version: recorderUploadsVersion,
		Meetings: map[K]string{}, Recordings: map[string]string{},
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return uploads, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read meeting upload record: %w", err)
	}
	var header struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(data, &header) == nil && header.Version > recorderUploadsVersion {
		return nil, fmt.Errorf("unsupported meeting upload record version %d", header.Version)
	}
	var stored RecorderUploads[K]
	if json.Unmarshal(data, &stored) != nil || stored.Meetings == nil {
		// An unreadable cache only means every meeting is offered again; the
		// next save replaces it.
		uploads.Dirty = true
		return uploads, nil //nolint:nilerr // A corrupt cache must not block sync.
	}
	if stored.Version == 1 {
		// Version 1 keyed recording acknowledgements only by meeting row ID.
		// A database reset can reuse that ID, so preserve meeting offers but
		// invalidate every ambiguous recording receipt.
		uploads.Meetings = stored.Meetings
		uploads.Dirty = true
		return uploads, nil
	}
	if stored.Version != recorderUploadsVersion {
		uploads.Dirty = true
		return uploads, nil
	}
	uploads.Meetings = stored.Meetings
	if stored.Recordings != nil {
		uploads.Recordings = stored.Recordings
	}
	return uploads, nil
}

func MeetingDigest(m any) (string, error) {
	encoded, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

func (u *RecorderUploads[K]) Acknowledged(id K, digest string) bool {
	return u.Meetings[id] == digest
}

func (u *RecorderUploads[K]) Record(id K, digest string) {
	if u.Meetings[id] != digest {
		u.Meetings[id] = digest
		u.Dirty = true
	}
}

func (u *RecorderUploads[K]) RecordingAcknowledged(identity, hash string) bool {
	return u.Recordings[identity] == hash
}

func (u *RecorderUploads[K]) RecordRecording(identity, hash string) {
	if u.Recordings[identity] != hash {
		u.Recordings[identity] = hash
		u.Dirty = true
	}
}

func (u *RecorderUploads[K]) ForgetRecording(identity string) {
	if _, ok := u.Recordings[identity]; ok {
		delete(u.Recordings, identity)
		u.Dirty = true
	}
}

func (u *RecorderUploads[K]) Save() error {
	if !u.Dirty {
		return nil
	}
	data, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("encode meeting upload record: %w", err)
	}
	if err := atomicfile.WriteFile(u.path, data, atomicfile.WithPrivate()); err != nil {
		return fmt.Errorf("save meeting upload record: %w", err)
	}
	u.Dirty = false
	return nil
}
