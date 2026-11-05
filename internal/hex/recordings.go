package hex

import (
	"context"
	"encoding/binary"
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
	r, err := meetingarchive.PrepareRecording(ctx, opts.MeetingsDir, filepath.Join(m.Manifest.ID, track+".wav"), opts.RecordingMaxBytes)
	if r != nil && !trackHasSamples(r) {
		_ = r.Close()
		return nil, nil
	}
	return r, err
}

// Hex creates valid empty WAVs for tracks that never opened. Inspect chunks
// in the immutable snapshot so these tracks wait for real samples.
func trackHasSamples(r *meetingarchive.Recording) bool {
	for pos, count := int64(12), 0; pos <= r.Size-8 && count < 10000; count++ {
		var header [8]byte
		if _, err := r.File.ReadAt(header[:], pos); err != nil {
			return false
		}
		size := int64(binary.LittleEndian.Uint32(header[4:]))
		if size > r.Size-pos-8 {
			return false
		}
		if string(header[:4]) == "data" {
			return size > 0
		}
		pos += 8 + size + size%2
	}
	return false
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
