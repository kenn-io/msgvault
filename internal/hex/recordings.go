package hex

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingimport"
)

type RecordingUpload struct {
	Source       meetingimport.Source
	MessageID    int64
	Track        string
	Hash         string
	Size         int64
	MIMEType     string
	NoBuildCache bool
}

func (u RecordingUpload) Normalize() (RecordingUpload, error) {
	source, err := meetingimport.NormalizeRecorderSource(u.Source)
	if err != nil {
		return u, ErrRemoteValidation
	}
	u.Source = source
	_, hashErr := hex.DecodeString(u.Hash)
	if (u.Track != "system" && u.Track != "microphone") || u.MessageID <= 0 || u.Size <= 0 || u.Size == int64(^uint64(0)>>1) || len(u.Hash) != 64 || u.Hash != strings.ToLower(u.Hash) || hashErr != nil || u.MIMEType != "audio/wav" {
		return u, ErrRemoteRecordValidation
	}
	return u, nil
}
func StageRecordingUpload(ctx context.Context, u RecordingUpload, input io.Reader, maxBytes int64) (*meetingarchive.Recording, error) {
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
func (imp *Importer) ImportRecording(ctx context.Context, u RecordingUpload, dir string, input io.Reader, maxBytes int64) (bool, error) {
	u, err := u.Normalize()
	if err != nil {
		return false, err
	}
	if maxBytes <= 0 {
		maxBytes = meetingarchive.DefaultRecordingMaxBytes
	}
	if u.Size > maxBytes {
		return false, meetingarchive.ErrRecordingTooLarge
	}
	source, err := imp.registeredSource(ctx, u.Source, false)
	if err != nil {
		return false, err
	}
	message, err := imp.store.GetMessageContext(ctx, u.MessageID)
	if err != nil || message.SourceID != source.ID || message.MessageType != "meeting_transcript" {
		return false, ErrRemoteRecordValidation
	}
	changed, err := meetingarchive.StoreRecordingStream(ctx, imp.store, "hex:"+u.Track, fmt.Sprintf("meeting-%d-%s.wav", u.MessageID, u.Track), u.MessageID, dir, u.Hash, u.Size, u.MIMEType, input)
	if errors.Is(err, meetingarchive.ErrRecordingValidation) {
		err = ErrRemoteRecordValidation
	}
	return changed, err
}
func prepareTrack(ctx context.Context, opts ImportOptions, m Meeting, track string) (*meetingarchive.Recording, error) {
	return meetingarchive.PrepareRecording(ctx, opts.MeetingsDir, filepath.Join(m.Manifest.ID, track+".wav"), opts.RecordingMaxBytes)
}
func (imp *Importer) importRecordings(ctx context.Context, opts ImportOptions, m Meeting, messageID int64, sum *ImportSummary) (bool, error) {
	var failures []error
	changedAny := false
	for _, track := range []string{"system", "microphone"} {
		r, err := prepareTrack(ctx, opts, m, track)
		if r == nil {
			sum.RecordingsSkipped++
		} else {
			changed, writeErr := meetingarchive.StoreRecording(ctx, imp.store, "hex:"+track, fmt.Sprintf("meeting-%d-%s.wav", messageID, track), messageID, opts.AttachmentsDir, r)
			err = writeErr
			_ = r.Close()
			if err == nil {
				sum.RecordingsStored++
				if changed {
					changedAny = true
				}
			}
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	return changedAny, errors.Join(failures...)
}
