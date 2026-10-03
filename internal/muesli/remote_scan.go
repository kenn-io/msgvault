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
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/msgvault/internal/meetingimport"
)

type RemoteSender func(context.Context, RemoteRequest) (RemoteResult, error)

// ScanRemote holds a recorder-side process lock before reading a snapshot,
// through the last acknowledgement. Overlapping native hook/watch/manual
// processes therefore cannot upload an older snapshot after a newer one.
func ScanRemote(ctx context.Context, opts ImportOptions, send RemoteSender) (*ImportSummary, error) {
	started := time.Now()
	sum := &ImportSummary{}
	defer func() { sum.Duration = time.Since(started) }()
	lock, err := lockRemoteSource(ctx, opts.DBPath, opts.LockDir)
	if err != nil {
		return sum, err
	}
	defer func() { _ = lock.Unlock() }()
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
	meetings, err := reader.ListMeetings(ctx)
	if err != nil {
		return sum, err
	}
	shared := contacts.sharedAddresses(opts.PhoneCountryCode)
	var recordErrors []error
	for _, meeting := range meetings {
		if err := ctx.Err(); err != nil {
			return sum, errors.Join(errors.Join(recordErrors...), err)
		}
		if opts.MeetingID > 0 && meeting.ID != opts.MeetingID {
			continue
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
		var local *Importer
		if err := local.resolveParticipants(0, &meeting, contacts, opts.PhoneCountryCode, shared); err != nil {
			return sum, err
		}
		r := RemoteRequest{Action: "upsert", Source: meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}, Meeting: NewRemoteMeeting(meeting), Full: opts.Full}
		r, err := r.Normalize()
		if err == nil {
			var encoded []byte
			encoded, err = json.Marshal(r)
			if err != nil {
				// Local encoding failures belong to this record. Keep values out
				// of diagnostics and let later completed meetings continue.
				err = remoteInvalid("meeting", "cannot be encoded as JSON")
			}
			if err == nil && int64(len(encoded)) > MaxRemoteRequestBytes {
				err = ErrRemoteTooLarge
			}
		}
		var result RemoteResult
		if err == nil {
			result, err = send(ctx, r)
		}
		sum.SourceID = result.SourceID
		if result.Status == "created" {
			sum.MeetingsAdded++
		} else if result.Changed {
			sum.MeetingsUpdated++
		}
		if err != nil {
			sum.Errors++
			if errors.Is(err, ErrRemoteValidation) || errors.Is(err, ErrRemoteTooLarge) {
				recordErrors = append(recordErrors, err)
				continue
			}
			return sum, errors.Join(errors.Join(recordErrors...), err)
		}
	}
	return sum, errors.Join(recordErrors...)
}

func lockRemoteSource(ctx context.Context, dbPath, lockDir string) (*flock.Flock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	canonical, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve Muesli database: %w", err)
	}
	canonical, err = filepath.EvalSymlinks(canonical)
	if err != nil {
		return nil, fmt.Errorf("resolve Muesli database: %w", err)
	}
	if lockDir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("locate Muesli sync lock directory: %w", err)
		}
		lockDir = filepath.Join(cache, "msgvault", "muesli-sync")
	}
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("create Muesli sync lock directory: %w", err)
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
