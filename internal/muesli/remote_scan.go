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
	"slices"
	"time"

	"github.com/gofrs/flock"
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
	canonical, lockDir, err := remoteSyncPaths(opts.DBPath, opts.LockDir)
	if err != nil {
		return sum, err
	}
	lock, err := lockRemoteSource(ctx, canonical, lockDir)
	if err != nil {
		return sum, err
	}
	defer func() { _ = lock.Unlock() }()
	uploads, err := loadRemoteUploads(remoteUploadsPath(lockDir, canonical, opts.Identifier, opts.RemoteTarget))
	if err != nil {
		return sum, err
	}
	defer func() { retErr = errors.Join(retErr, uploads.save()) }()
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
			if !errors.Is(err, ErrRemoteValidation) && !errors.Is(err, ErrRemoteTooLarge) {
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
	uploads  *remoteUploads
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
	digest, err := meetingDigest(r.Meeting)
	if err != nil {
		return remoteInvalid("meeting", "cannot be encoded as JSON")
	}
	if !p.opts.Full && !hasContactReview(r.Meeting) && p.uploads.acknowledged(meeting.ID, digest) {
		return nil
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
	if err != nil {
		return err
	}
	p.uploads.record(meeting.ID, digest)
	return nil
}

func hasContactReview(m *RemoteMeeting) bool {
	return slices.ContainsFunc(m.Participants, func(p RemoteParticipant) bool {
		return len(p.ContactReviewPhones) > 0
	})
}

// remoteSyncPaths resolves the database to one canonical path, so every alias
// of the same file shares a lock and upload record.
func remoteSyncPaths(dbPath, lockDir string) (canonical, dir string, err error) {
	canonical, err = filepath.Abs(dbPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve Muesli database: %w", err)
	}
	canonical, err = filepath.EvalSymlinks(canonical)
	if err != nil {
		return "", "", fmt.Errorf("resolve Muesli database: %w", err)
	}
	if lockDir == "" {
		return "", "", errors.New("muesli remote sync requires a lock directory")
	}
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return "", "", fmt.Errorf("create Muesli sync lock directory: %w", err)
	}
	return canonical, lockDir, nil
}

func lockRemoteSource(ctx context.Context, canonical, lockDir string) (*flock.Flock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(canonical))
	lock := flock.New(filepath.Join(lockDir, hex.EncodeToString(hash[:])+".lock"), flock.SetPermissions(0o600))
	ok, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("acquire Muesli source lock: %w", err)
	}
	if !ok {
		return nil, errors.New("muesli source lock unavailable")
	}
	return lock, nil
}
