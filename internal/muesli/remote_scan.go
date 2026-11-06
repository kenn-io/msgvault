package muesli

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/meetingimport"
)

type RemoteSender func(context.Context, RemoteRequest) (RemoteResult, error)

// ScanRemote holds a recorder-side process lock before reading a snapshot,
// through the last acknowledgement. Overlapping native hook/watch/manual
// processes therefore cannot upload an older snapshot after a newer one.
// Meetings whose payload the daemon already acknowledged are not uploaded
// again unless opts.Full is set.
func ScanRemote(ctx context.Context, opts ImportOptions, send RemoteSender) (sum *ImportSummary, retErr error) {
	started := time.Now()
	sum = &ImportSummary{}
	defer func() { sum.Duration = time.Since(started) }()
	canonical, lockDir, err := meetingimport.RecorderSyncPaths(opts.DBPath, opts.LockDir)
	if err != nil {
		return sum, err
	}
	lock, err := meetingimport.LockRecorderSource(ctx, canonical, lockDir)
	if err != nil {
		return sum, err
	}
	defer func() { _ = lock.Unlock() }()
	uploads, err := meetingimport.LoadRecorderUploads[int64](meetingimport.RecorderUploadsPath(lockDir, canonical, opts.Identifier, opts.RemoteTarget))
	if err != nil {
		return sum, err
	}
	defer func() { retErr = errors.Join(retErr, uploads.Save()) }()
	reader, err := Open(ctx, opts.DBPath)
	if err != nil {
		return sum, err
	}
	defer func() { _ = reader.Close() }()
	contacts := DisabledContacts()
	if opts.ContactsEnabled {
		contacts, err = OpenContacts(ctx, opts.ContactsPath)
		if err != nil {
			return sum, err
		}
	}
	sum.ContactsState = contacts.State()
	meetings, err := reader.listMeetings(ctx, opts.MeetingID)
	if err != nil {
		return sum, err
	}
	push := remotePush{opts: opts, send: send, uploads: uploads, sum: sum,
		contacts: contacts, shared: contacts.sharedAddresses(opts.PhoneCountryCode)}
	var recordErrors []error
	for _, meeting := range meetings {
		if err := ctx.Err(); err != nil {
			return sum, errors.Join(errors.Join(recordErrors...), err)
		}
		switch meeting.Eligibility() {
		case SkipDeleted:
			sum.SkippedDeleted++
			continue
		case SkipInProgress:
			sum.SkippedInProgress++
			continue
		case SkipEmpty:
			sum.SkippedEmpty++
			continue
		}
		if !opts.StartedAfter.IsZero() {
			when, err := time.Parse(time.RFC3339Nano, meeting.StartTime)
			if err == nil && when.Before(opts.StartedAfter) {
				continue
			}
		}
		if opts.Limit > 0 && sum.MeetingsProcessed >= int64(opts.Limit) {
			break
		}
		sum.MeetingsProcessed++
		if err := push.meeting(ctx, meeting); err != nil {
			sum.Errors++
			if !errors.Is(err, ErrRemoteValidation) && !errors.Is(err, ErrRemoteTooLarge) && !errors.Is(err, ErrRecordingTooLarge) {
				return sum, errors.Join(errors.Join(recordErrors...), err)
			}
			recordErrors = append(recordErrors, fmt.Errorf("muesli meeting %d: %w", meeting.ID, err))
		}
	}
	return sum, errors.Join(recordErrors...)
}

type remotePush struct {
	opts     ImportOptions
	send     RemoteSender
	uploads  *meetingimport.RecorderUploads[int64]
	sum      *ImportSummary
	contacts *Contacts
	shared   map[string]bool
}

// meeting uploads one eligible meeting unless the daemon already acknowledged
// the same payload. Meetings with review phones are always offered, because a
// suggested phone identity may reach the archive after the last upload.
func (p remotePush) meeting(ctx context.Context, meeting Meeting) error {
	var noArchive *Importer
	if err := noArchive.resolveParticipants(0, &meeting, p.contacts, p.opts.PhoneCountryCode, p.shared); err != nil {
		return err
	}
	r := RemoteRequest{Action: "upsert", Source: meetingimport.Source{Identifier: p.opts.Identifier, AccountEmail: p.opts.AccountEmail}, Meeting: NewRemoteMeeting(meeting), Full: p.opts.Full}
	r, err := r.Normalize()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		// Local encoding failures belong to this record. Keep values out of
		// diagnostics and let later completed meetings continue.
		return remoteInvalid("meeting", "cannot be encoded as JSON")
	}
	if int64(len(encoded)) > MaxRemoteRequestBytes {
		return ErrRemoteTooLarge
	}
	digest, err := meetingimport.MeetingDigest(r.Meeting)
	if err != nil {
		return remoteInvalid("meeting", "cannot be encoded as JSON")
	}
	var recording *Recording
	var recordingErr error
	if p.opts.Recordings {
		recording, recordingErr = PrepareRecording(ctx, p.opts.DBPath, meeting.SavedRecordingPath, p.opts.RecordingMaxBytes)
		if recording != nil {
			defer func() { _ = recording.Close() }()
			digest += ":" + recording.Hash
		} else if meeting.SavedRecordingPath != "" {
			p.sum.RecordingsSkipped++
		}
	}
	recordingAcknowledged := false
	recordingIdentity, identityErr := meeting.SourceMessageID()
	if identityErr != nil {
		if p.opts.Recordings {
			return remoteInvalid("meeting", "cannot identify recording acknowledgement")
		}
		recordingIdentity = ""
	}
	if p.opts.Full && recordingIdentity != "" {
		// A full resync may be rebuilding an archive that no longer contains
		// previously acknowledged audio. Invalidate the receipt even when the
		// local recording is currently unavailable.
		p.uploads.ForgetRecording(recordingIdentity)
	}
	if recording != nil {
		recordingAcknowledged = !p.opts.Full && p.uploads.RecordingAcknowledged(recordingIdentity, recording.Hash)
		if recordingAcknowledged {
			p.uploads.RecordRecording(recordingIdentity, recording.Hash)
		}
	}
	if !p.opts.Full && !hasContactReview(r.Meeting) && p.uploads.Acknowledged(meeting.ID, digest) &&
		(recording == nil || recordingAcknowledged) {
		return recordingErr
	}
	result, err := p.send(ctx, r)
	if result.SourceID != 0 {
		p.sum.SourceID = result.SourceID
	}
	if result.Status == "created" {
		p.sum.MeetingsAdded++
	} else if result.Changed {
		p.sum.MeetingsUpdated++
	}
	if result.Status == "created" && recordingIdentity != "" {
		p.uploads.ForgetRecording(recordingIdentity)
		recordingAcknowledged = false
	}
	if err != nil {
		// A lost response can hide a successful create. Do not let an old
		// recording receipt suppress recovery on the next successful scan.
		if recordingIdentity != "" {
			p.uploads.ForgetRecording(recordingIdentity)
		}
		return err
	}
	if recording != nil && !recordingAcknowledged {
		if p.opts.SendRecording == nil {
			return errors.New("muesli recording transport is unavailable")
		}
		upload := RecordingUpload{Source: r.Source, MessageID: result.MessageID, Hash: recording.Hash, Size: recording.Size, MIMEType: recording.MIMEType}
		if err := p.opts.SendRecording(ctx, upload, recording); err != nil {
			return err
		}
		p.uploads.RecordRecording(recordingIdentity, recording.Hash)
		p.sum.RecordingsStored++
	}
	p.uploads.Record(meeting.ID, digest)
	return recordingErr
}

func hasContactReview(m *RemoteMeeting) bool {
	return slices.ContainsFunc(m.Participants, func(p RemoteParticipant) bool {
		return len(p.ContactReviewPhones) > 0
	})
}
