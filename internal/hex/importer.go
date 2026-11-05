package hex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

type Importer struct{ store *store.Store }

func NewImporter(st *store.Store) *Importer { return &Importer{store: st} }

// ImportOptions selects one registered source and optional recorder-local audio.
type ImportOptions struct {
	Identifier        string
	AccountEmail      string
	MeetingsDir       string
	AttachmentsDir    string
	LockDir           string
	RemoteTarget      string
	Full              bool
	Recordings        bool
	RecordingMaxBytes int64
	Limit             int
	StartedAfter      time.Time
	Progress          func(string)
	SendRecording     func(context.Context, RecordingUpload, *meetingarchive.Recording) error
}
type ImportSummary struct {
	SourceID          int64
	MeetingsProcessed int64
	MeetingsAdded     int64
	MeetingsUpdated   int64
	SkippedInProgress int64
	Errors            int64
	Warnings          int64
	RecordingsStored  int64
	RecordingsSkipped int64
	Duration          time.Duration
}

func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (sum *ImportSummary, retErr error) {
	sum = &ImportSummary{}
	if imp == nil || imp.store == nil {
		return sum, meetingarchive.ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return sum, err
	}
	started := time.Now()
	defer func() { sum.Duration = time.Since(started) }()
	source, err := imp.store.GetSourceByTypeAndIdentifier(SourceType, strings.TrimSpace(opts.Identifier))
	if err != nil {
		return sum, err
	}
	sum.SourceID = source.ID
	if err := imp.store.AddAccountIdentityContext(ctx, source.ID, opts.AccountEmail, "account-email"); err != nil {
		return sum, err
	}
	syncID, err := imp.store.StartSyncContext(ctx, source.ID, SourceType)
	if err != nil {
		return sum, err
	}
	scoped := imp.store.ScopedToSync(source.ID, syncID)
	checkpoint := func() *store.Checkpoint {
		return &store.Checkpoint{MessagesProcessed: sum.MeetingsProcessed, MessagesAdded: sum.MeetingsAdded, MessagesUpdated: sum.MeetingsUpdated, ErrorsCount: sum.Errors}
	}
	defer func() {
		if retErr != nil {
			_ = scoped.FailSyncWithCheckpoint(syncID, "Hex sync failed", checkpoint())
		}
	}()
	reader, err := Open(ctx, opts.MeetingsDir)
	if err != nil {
		return sum, err
	}
	defer func() { _ = reader.Close() }()
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
		snapshot, retained, err := NewImporter(scoped).prepareArchive(ctx, m, source.ID, opts.Identifier, opts.AccountEmail)
		if err == nil {
			result := meetingarchive.Result{MessageID: retained}
			var writeErr error
			if retained != 0 {
				sum.Warnings++
			} else {
				result, writeErr = meetingarchive.New(scoped).Upsert(ctx, snapshot, meetingarchive.UpsertOptions{Force: opts.Full})
			}
			err = writeErr
			if result.Created {
				sum.MeetingsAdded++
			} else if result.Changed {
				sum.MeetingsUpdated++
			}
			if err == nil && opts.Recordings {
				changed, recordingErr := NewImporter(scoped).importRecordings(ctx, opts, m, result.MessageID, sum)
				err = recordingErr
				if changed && !result.Created && !result.Changed {
					sum.MeetingsUpdated++
				}
			}
		}
		if err == nil && opts.Progress != nil {
			opts.Progress("processed Hex meeting " + m.Manifest.ID)
		}
		if err != nil {
			sum.Errors++
			failures = append(failures, fmt.Errorf("Hex meeting %s: %w", m.Manifest.ID, err))
		}
		if sum.MeetingsProcessed%50 == 0 {
			if err := scoped.UpdateSyncCheckpoint(syncID, checkpoint()); err != nil {
				return sum, err
			}
		}
	}
	if len(failures) > 0 {
		return sum, errors.Join(failures...)
	}
	if err := scoped.UpdateSyncCheckpoint(syncID, checkpoint()); err != nil {
		return sum, err
	}
	return sum, scoped.CompleteSync(syncID, "")
}
