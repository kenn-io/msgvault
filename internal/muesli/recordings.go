package muesli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingimport"
)

const DefaultRecordingMaxBytes = meetingarchive.DefaultRecordingMaxBytes
const recordingPartKey = "muesli:recording"

var ErrRecordingTooLarge = meetingarchive.ErrRecordingTooLarge
var ErrRecordingChanged = meetingarchive.ErrRecordingChanged

type Recording = meetingarchive.Recording

func PrepareRecording(ctx context.Context, dbPath, savedPath string, maxBytes int64) (*Recording, error) {
	if resolved, err := filepath.EvalSymlinks(dbPath); err == nil {
		dbPath = resolved
	} else if dir, err := filepath.EvalSymlinks(filepath.Dir(dbPath)); err == nil {
		dbPath = filepath.Join(dir, filepath.Base(dbPath))
	}
	return meetingarchive.PrepareRecording(ctx, filepath.Join(filepath.Dir(dbPath), "meeting-recordings"), savedPath, maxBytes)
}

// RecordingUpload names an archived message and immutable audio claims. It
// carries no filesystem path and uses the registered recorder's identity.
type RecordingUpload struct {
	Source       meetingimport.Source
	MessageID    int64
	Hash         string
	Size         int64
	MIMEType     string
	NoBuildCache bool
}

func (u RecordingUpload) Normalize() (RecordingUpload, error) {
	normalized, err := (RemoteRequest{Action: "register", Source: u.Source}).Normalize()
	if err != nil {
		return u, err
	}
	u.Source = normalized.Source
	if u.MessageID <= 0 || u.Size <= 0 || u.Size == int64(^uint64(0)>>1) || !remoteHex(u.Hash, 64) || (u.MIMEType != "audio/wav" && u.MIMEType != "audio/mp4") {
		return u, ErrRemoteRecordValidation
	}
	return u, nil
}

func (u RecordingUpload) Validate() error {
	_, err := u.Normalize()
	return err
}

func StageRecordingUpload(ctx context.Context, u RecordingUpload, input io.Reader, maxBytes int64) (*Recording, error) {
	u, err := u.Normalize()
	if err != nil {
		return nil, err
	}
	r, err := meetingarchive.StageRecordingUpload(ctx, u.Hash, u.Size, u.MIMEType, input, maxBytes)
	if errors.Is(err, meetingarchive.ErrRecordingValidation) {
		err = ErrRemoteRecordValidation
	}
	return r, err
}

// ImportRecording validates source membership before reading any upload bytes.
func (imp *Importer) ImportRecording(ctx context.Context, u RecordingUpload, dir string, input io.Reader, maxBytes int64) (bool, error) {
	u, err := u.Normalize()
	if err != nil {
		return false, err
	}
	if maxBytes <= 0 {
		maxBytes = DefaultRecordingMaxBytes
	}
	if u.Size > maxBytes {
		return false, ErrRecordingTooLarge
	}
	source, err := imp.store.GetSourceByTypeAndIdentifier(ctx, SourceType, u.Source.Identifier)
	if err != nil {
		return false, err
	}
	_, matches, err := meetingimport.RecorderIdentityMatch(ctx, imp.store, source.ID, u.Source.AccountEmail)
	if err != nil {
		return false, err
	}
	if !matches {
		return false, ErrRemoteValidation
	}
	message, err := imp.store.GetMessageContext(ctx, u.MessageID)
	if err != nil || message.SourceID != source.ID || message.MessageType != "meeting_transcript" {
		return false, ErrRemoteRecordValidation
	}
	return imp.storeRecordingStream(ctx, u.MessageID, dir, u.Hash, u.Size, u.MIMEType, input)
}

func (imp *Importer) storeRecording(ctx context.Context, messageID int64, dir string, r *Recording) (bool, error) {
	return meetingarchive.StoreRecording(ctx, imp.store, recordingPartKey, recordingFilename(messageID, r.MIMEType), messageID, dir, r)
}
func (imp *Importer) storeRecordingStream(ctx context.Context, messageID int64, dir, hash string, size int64, mime string, input io.Reader) (bool, error) {
	changed, err := meetingarchive.StoreRecordingStream(ctx, imp.store, recordingPartKey, recordingFilename(messageID, mime), messageID, dir, hash, size, mime, input)
	if errors.Is(err, meetingarchive.ErrRecordingValidation) {
		err = ErrRemoteRecordValidation
	}
	return changed, err
}

func recordingFilename(id int64, mime string) string {
	extension := ".m4a"
	if mime == "audio/wav" {
		extension = ".wav"
	}
	return fmt.Sprintf("meeting-%d%s", id, extension)
}
