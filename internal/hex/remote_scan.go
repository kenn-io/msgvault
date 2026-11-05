package hex

import (
	"context"
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingimport"
)

type RemoteSender func(context.Context, RemoteRequest) (RemoteResult, error)

func ForgetRemoteUploads(ctx context.Context, opts ImportOptions) error {
	return meetingimport.ForgetRecorderUploads[string](ctx, opts.MeetingsDir, opts.LockDir, opts.Identifier, opts.RemoteTarget)
}

// ScanRemote serializes a complete scan through its final acknowledgement.
// Receipts are a cache; --full and re-registration reoffer every record.
func ScanRemote(ctx context.Context, opts ImportOptions, send RemoteSender) (sum *ImportSummary, retErr error) {
	sum = &ImportSummary{}
	started := time.Now()
	defer func() { sum.Duration = time.Since(started) }()
	reader, err := Open(ctx, opts.MeetingsDir)
	if err != nil {
		return sum, err
	}
	defer func() { _ = reader.Close() }()
	if reader.root == nil {
		return sum, nil
	}
	canonical, dir, err := meetingimport.RecorderSyncPaths(opts.MeetingsDir, opts.LockDir)
	if err != nil {
		return sum, err
	}
	lock, err := meetingimport.LockRecorderSource(ctx, canonical, dir)
	if err != nil {
		return sum, err
	}
	defer func() { _ = lock.Unlock() }()
	receipts, err := meetingimport.LoadRecorderUploads[string](meetingimport.RecorderUploadsPath(dir, canonical, opts.Identifier, opts.RemoteTarget))
	if err != nil {
		return sum, err
	}
	defer func() { retErr = errors.Join(retErr, receipts.Save()) }()
	scan, err := reader.ListMeetings(ctx)
	if err != nil {
		return sum, err
	}
	sum.Warnings = scan.Warnings
	sum.SkippedInProgress = scan.Deferred
	var failures []error
	for _, m := range scan.Meetings {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		if time.UnixMilli(m.Manifest.CreatedAtMS).Before(opts.StartedAfter) {
			continue
		}
		if opts.Limit > 0 && sum.MeetingsProcessed >= int64(opts.Limit) {
			break
		}
		sum.MeetingsProcessed++
		err := pushMeeting(ctx, opts, m, send, receipts, sum)
		if err != nil {
			sum.Errors++
			if !errors.Is(err, ErrRemoteRecordValidation) && !errors.Is(err, ErrRemoteTooLarge) && !errors.Is(err, meetingarchive.ErrRecordingTooLarge) {
				return sum, errors.Join(errors.Join(failures...), err)
			}
			failures = append(failures, err)
		}
	}
	return sum, errors.Join(failures...)
}
func pushMeeting(ctx context.Context, opts ImportOptions, m Meeting, send RemoteSender, receipts *meetingimport.RecorderUploads[string], sum *ImportSummary) error {
	request, err := (RemoteRequest{Action: "upsert", Source: meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}, Meeting: &m, Full: opts.Full}).Normalize()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return ErrRemoteRecordValidation
	}
	if int64(len(encoded)) > MaxRemoteRequestBytes {
		return ErrRemoteTooLarge
	}
	digest, err := meetingimport.MeetingDigest(m)
	if err != nil {
		return err
	}
	recordings := map[string]*meetingarchive.Recording{}
	var failures []error
	pending := false
	for _, track := range []string{"system", "microphone"} {
		key := m.Manifest.ID + ":" + track
		if opts.Full {
			receipts.ForgetRecording(key)
		}
		if !opts.Recordings {
			continue
		}
		r, err := prepareTrack(ctx, opts, m, track)
		if err != nil {
			failures = append(failures, err)
		}
		if r == nil {
			sum.RecordingsSkipped++
			continue
		}
		defer func() { _ = r.Close() }()
		recordings[track] = r
		if !receipts.RecordingAcknowledged(key, r.Hash) {
			pending = true
		}
	}
	if !opts.Full && receipts.Acknowledged(m.Manifest.ID, digest) && !pending {
		return errors.Join(failures...)
	}
	result, err := send(ctx, request)
	sum.Warnings += result.Warnings
	if result.SourceID != 0 {
		sum.SourceID = result.SourceID
	}
	if result.Status == "created" {
		sum.MeetingsAdded++
	} else if result.Changed {
		sum.MeetingsUpdated++
	}
	if err != nil || result.Status == "created" {
		for _, track := range []string{"system", "microphone"} {
			receipts.ForgetRecording(m.Manifest.ID + ":" + track)
		}
	}
	if err != nil {
		return err
	}
	for _, track := range []string{"system", "microphone"} {
		r := recordings[track]
		if r == nil || receipts.RecordingAcknowledged(m.Manifest.ID+":"+track, r.Hash) {
			continue
		}
		if opts.SendRecording == nil {
			return errors.New("hex recording transport unavailable")
		}
		err := opts.SendRecording(ctx, RecordingUpload{Source: request.Source, MessageID: result.MessageID, Track: track, Hash: r.Hash, Size: r.Size, MIMEType: r.MIMEType}, r)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		receipts.RecordRecording(m.Manifest.ID+":"+track, r.Hash)
		sum.RecordingsStored++
	}
	// A successful retention is also an acknowledgement of this payload.
	receipts.Record(m.Manifest.ID, digest)
	return errors.Join(failures...)
}
