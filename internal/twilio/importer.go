package twilio

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/callsync"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

type Source interface {
	ListRecordingsPage(ctx context.Context, after time.Time, pageSize int, cursor string) ([]Recording, string, error)
	GetCall(ctx context.Context, callID string) (Call, error)
	CallRecordings(ctx context.Context, callID string) ([]Recording, error)
	Transcripts(ctx context.Context, recordings []Recording) (Evidence, error)
	OpenRecording(ctx context.Context, recording Recording, maxBytes int64) (io.ReadCloser, error)
}

type Importer struct {
	store  *store.Store
	client Source
	now    func() time.Time
}

func NewImporter(st *store.Store, client Source) *Importer {
	return &Importer{store: st, client: client, now: time.Now}
}

type (
	ImportOptions = callsync.Options
	ImportSummary = callsync.Summary
)

var spec = callsync.Spec{SourceType: SourceType, Label: "Twilio", Command: "sync-twilio", RowPrefix: "twilio:"}

const recordingPageSize = 1000

// importRun is the Twilio side of one engine run.
type importRun struct {
	*callsync.Run

	imp *Importer
}

func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (*ImportSummary, error) {
	return callsync.Import(ctx, imp.store, spec, imp.now(), opts, func(run *callsync.Run) callsync.Provider[[]Recording] {
		return &importRun{Run: run, imp: imp}
	})
}

// ListPage groups one page of account-wide recordings by call. Each item's key
// names its recordings too, so a call whose recordings span pages is handled
// again for the later ones, which matters when its per-call list is gone.
func (r *importRun) ListPage(ctx context.Context, after time.Time, cursor string) ([]callsync.Item[[]Recording], string, error) {
	recordings, next, err := r.imp.client.ListRecordingsPage(ctx, after, recordingPageSize, cursor)
	if err != nil {
		return nil, "", err
	}
	var items []callsync.Item[[]Recording]
	index := map[string]int{}
	for _, recording := range recordings {
		if recording.CallSID == "" {
			r.Sum.Diagnostics = append(r.Sum.Diagnostics, "recording "+recording.SID+" skipped: no call SID")
			continue
		}
		i, ok := index[recording.CallSID]
		if !ok {
			i = len(items)
			index[recording.CallSID] = i
			items = append(items, callsync.Item[[]Recording]{ID: recording.CallSID})
		}
		items[i].Listed = append(items[i].Listed, recording)
	}
	for i := range items {
		items[i].Key = items[i].ID
		for _, recording := range items[i].Listed {
			items[i].Key += " " + recording.SID
		}
	}
	return items, next, nil
}

// Handle re-fetches and re-merges one listed call.
func (r *importRun) Handle(ctx context.Context, id string, listed []Recording) (callErr, err error) {
	return r.archiveCall(ctx, id, listed)
}

// callStart is the earliest of the call's start and its recordings' start or
// creation times, so a call whose details Twilio deleted keeps a date.
func callStart(a archivedCall) time.Time {
	start := ParseTime(a.Call.StartTime)
	for _, recording := range a.Recordings {
		for _, value := range []string{recording.StartTime, recording.DateCreated} {
			if at := ParseTime(value); !at.IsZero() && (start.IsZero() || at.Before(start)) {
				start = at
			}
		}
	}
	return start
}

func (r *importRun) createdInBound(recording Recording) bool {
	return r.Opts.CreatedAfter.IsZero() || !ParseTime(recording.DateCreated).Before(r.Opts.CreatedAfter)
}

// archiveCall returns callErr for provider failures other than a refusal
// (transient, malformed or mismatched answers), which fail the call and the
// next sync retries, and err for store failures that must stop the run.
// When the per-call list is gone, the account-wide listing still carries each
// listed recording's current status.
func (r *importRun) archiveCall(ctx context.Context, id string, discovered []Recording) (callErr, err error) {
	prior, err := loadArchive(ctx, r.St, r.SourceID, id)
	if err != nil {
		return nil, err
	}
	recordings := mergeRecordings(prior.Recordings, discovered)
	var diagnostics []string
	call, callErr := r.imp.client.GetCall(ctx, id)
	if callErr != nil {
		// Calls expire independently of retained recordings and transcripts.
		call = prior.Call
		call.SID = id
		if call.AccountSID == "" && len(recordings) > 0 {
			call.AccountSID = recordings[0].AccountSID
		}
		if refused(callErr) {
			diagnostics = append(diagnostics, "call_metadata_unavailable")
			callErr = nil
		}
	}
	var fresh []Recording
	if callErr == nil {
		fresh, callErr = r.imp.client.CallRecordings(ctx, id)
		if refused(callErr) {
			diagnostics = append(diagnostics, "call_recordings_unavailable")
			callErr = nil
		}
	}
	if !r.Opts.CreatedAfter.IsZero() {
		// --after bounds new evidence; recordings already archived stay.
		fresh = slices.DeleteFunc(fresh, func(recording Recording) bool {
			return ParseTime(recording.DateCreated).Before(r.Opts.CreatedAfter)
		})
	}
	recordings = mergeRecordings(recordings, fresh)
	var evidence Evidence
	var transcriptErr error
	if callErr == nil {
		evidence, transcriptErr = r.imp.client.Transcripts(ctx, recordings)
	}
	evidence.Diagnostics = append(evidence.Diagnostics, diagnostics...)
	archive := mergeArchive(prior, call, recordings, evidence)
	snapshot, err := archive.snapshot(r.SourceID, r.Opts.AccountEmail)
	if err != nil {
		return nil, err
	}
	result, err := meetingarchive.New(r.St).Upsert(ctx, snapshot, meetingarchive.UpsertOptions{Force: r.Opts.Full})
	if result.Changed {
		if result.Created {
			r.Sum.MeetingsAdded++
		} else {
			r.Sum.MeetingsUpdated++
		}
	}
	if err != nil {
		return nil, err
	}
	if callErr != nil {
		// Retain discovered recordings before queuing a retry: the next run
		// may no longer be able to list them through either discovery path.
		return callErr, nil
	}
	r.Sum.Diagnostics = append(r.Sum.Diagnostics, evidence.Diagnostics...)
	mediaErr, err := r.persistRecordings(ctx, result.MessageID, archive.Recordings, result.Changed)
	if err != nil {
		return nil, err
	}
	return errors.Join(transcriptErr, mediaErr), nil
}

// refused reports an API answer a later run would get again, such as a
// deleted call (404) or a refused one (401/403), so the call archives without
// what failed. A malformed answer stays a call error.
func refused(err error) bool {
	apiErr, ok := errors.AsType[*APIError](err)
	return ok && !apiErr.Retryable()
}

// persistRecordings returns mediaErr for downloads worth retrying and err for
// store failures. Each row write updates the message's attachment counts. A
// recording Twilio hasn't finished stays pending until a relist finds it
// completed, or the engine ages it out of the window.
func (r *importRun) persistRecordings(ctx context.Context, messageID int64, recordings []Recording, counted bool) (mediaErr, err error) {
	if len(recordings) == 0 {
		return nil, nil
	}
	rows, err := r.St.MessageProviderAttachments(messageID, spec.RowPrefix)
	if err != nil {
		return nil, err
	}
	policySkip := r.Opts.MediaPolicy.Evaluate(attachmentpolicy.Conversation{Type: "meeting", ParticipantCount: 2}, 0)
	var mediaErrs []error
	for _, recording := range recordings {
		key := "twilio:" + recording.SID
		row, exists := rows[key]
		if exists && (row.ContentHash != "" || !r.Opts.Full && !callsync.Waiting(row)) || !r.createdInBound(recording) {
			continue
		}
		metadata, err := json.Marshal(recording)
		if err != nil {
			return nil, err
		}
		write := store.AttachmentWrite{
			Filename: recording.SID + ".wav", MIMEType: "audio/wav", SourceAttachmentID: key, SourcePartKey: key,
			Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
			MediaType: "audio", Metadata: string(metadata), State: attachmentpolicy.StatePending,
			DurationMS: int64(durationSeconds(recording.Duration) * 1000),
		}
		switch {
		case policySkip != "":
			write.State, write.SkipReason = attachmentpolicy.StateSkipped, policySkip
		case recording.Encrypted() || recording.Status != "completed":
			if recording.Encrypted() || recording.Status == "deleted" || recording.Status == "absent" {
				write.State = attachmentpolicy.StateUnavailable
				write.SkipReason = attachmentpolicy.SkipSourceUnavailable
			} else {
				callsync.NotReady(&write)
			}
		default:
			alreadyFailed := exists && row.State == attachmentpolicy.StateFailed
			providerErr, storeErr := r.Fetch(ctx, recording.SID, func(maxBytes int64) (io.ReadCloser, error) {
				return r.imp.client.OpenRecording(ctx, recording, maxBytes)
			}, alreadyFailed, &write)
			if storeErr != nil {
				return nil, storeErr
			}
			if providerErr != nil {
				mediaErrs = append(mediaErrs, fmt.Errorf("recording %s: %w", recording.SID, providerErr))
			}
		}
		// Rewriting an identical row would only invalidate caches; a new row
		// or a changed one updates the meeting, which the cache refresh after
		// a failed run depends on.
		if exists && row.State == write.State && row.SkipReason == write.SkipReason && row.ContentHash == write.ContentHash && int64(row.Size) == write.Size {
			continue
		}
		if !counted {
			r.Sum.MeetingsUpdated++
			counted = true
		}
		if err := r.St.UpsertAttachmentRecordWithStats(ctx, messageID, write, true); err != nil {
			return nil, err
		}
	}
	return errors.Join(mediaErrs...), nil
}
