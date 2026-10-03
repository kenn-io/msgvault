package twenty

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

type Importer struct {
	store  *store.Store
	source Source
}

func NewImporter(st *store.Store, source Source) *Importer {
	return &Importer{store: st, source: source}
}

type ImportOptions struct {
	Identifier   string
	AccountEmail string
	Full         bool
	Limit        int
	StartedAfter time.Time
	Progress     func(string)
}
type ImportSummary struct {
	SourceID          int64
	MeetingsProcessed int64
	MeetingsAdded     int64
	MeetingsUpdated   int64
	SkippedEmpty      int64
	SkippedInvalid    int64
	PartialCoverage   bool
	Duration          time.Duration
}

// syncState is the JSON cursor persisted in sync_runs.cursor_after.
type syncState struct {
	// UpdatedAfter is the newest updatedAt among recordings already handled.
	// The next run lists recordings updated at or after it.
	UpdatedAfter string `json:"updated_after,omitempty"`
}

// Import lists recordings in updatedAt order from the last run's watermark,
// so late transcripts and summaries, which update the recording, are picked
// up without rescanning the catalog. Full rescans every recording.
func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (sum *ImportSummary, retErr error) {
	if imp == nil || imp.store == nil || imp.source == nil {
		return nil, errors.New("twenty importer is unavailable")
	}
	if opts.Limit < 0 {
		return nil, errors.New("twenty limit must be nonnegative")
	}
	opts.Identifier = strings.TrimSpace(opts.Identifier)
	opts.AccountEmail = strings.ToLower(strings.TrimSpace(opts.AccountEmail))
	parsed, err := mail.ParseAddress(opts.AccountEmail)
	if opts.Identifier == "" || err != nil || parsed.Name != "" || parsed.Address != opts.AccountEmail || strings.ContainsAny(opts.AccountEmail, " \t\r\n") {
		return nil, errors.New("twenty requires an identifier and actual account email")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source, err := imp.store.GetSourceByTypeAndIdentifier(SourceType, opts.Identifier)
	if err != nil {
		if errors.Is(err, store.ErrSourceNotFound) {
			return nil, fmt.Errorf("twenty source %q is not registered; run msgvault add-twenty first", opts.Identifier)
		}
		return nil, err
	}
	started := time.Now()
	sum = &ImportSummary{SourceID: source.ID}
	syncID, err := imp.store.StartSync(source.ID, SourceType)
	if err != nil {
		return sum, err
	}
	scoped := imp.store.ScopedToSync(source.ID, syncID)
	checkpoint := func() *store.Checkpoint {
		return &store.Checkpoint{MessagesProcessed: sum.MeetingsProcessed, MessagesAdded: sum.MeetingsAdded, MessagesUpdated: sum.MeetingsUpdated}
	}
	defer func() {
		sum.Duration = time.Since(started)
		if retErr != nil {
			_ = scoped.FailSyncWithCheckpoint(syncID, "Twenty sync failed", checkpoint())
		}
	}()
	if err := scoped.AddAccountIdentityContext(ctx, source.ID, opts.AccountEmail, "account-email"); err != nil {
		return sum, err
	}
	var state syncState
	if previous, err := imp.store.GetLastSuccessfulSync(source.ID); err == nil && previous.CursorAfter.Valid {
		_ = json.Unmarshal([]byte(previous.CursorAfter.String), &state)
	} else if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return sum, err
	}
	watermark, _ := time.Parse(time.RFC3339Nano, state.UpdatedAfter)
	since := time.Unix(0, 0).UTC()
	if !opts.Full && !watermark.IsZero() {
		since = watermark
	}
	// Recordings outside --after are listed but not archived, so that run
	// must not move the watermark past them.
	advance := opts.StartedAfter.IsZero()
	archiver := meetingarchive.New(scoped)
	cursor := ""
scan:
	for {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		page, err := imp.source.ListRecordings(ctx, since.Format(time.RFC3339Nano), cursor, pageSize)
		if err != nil {
			return sum, err
		}
		if page == nil {
			return sum, errors.New("twenty returned no recording page")
		}
		for index, recording := range page.Records {
			if err := ctx.Err(); err != nil {
				return sum, err
			}
			if recording.ID == "" {
				return sum, errors.New("twenty discovery returned a recording without an ID")
			}
			processed, err := imp.importRecording(ctx, archiver, scoped, syncID, source.ID, opts, recording, sum)
			if err != nil {
				return sum, err
			}
			if updated, err := time.Parse(time.RFC3339Nano, recording.UpdatedAt); err == nil && advance && updated.After(watermark) {
				watermark = updated
			}
			if !processed {
				continue
			}
			if opts.Progress != nil {
				opts.Progress(fmt.Sprintf("processed Twenty meeting %d", sum.MeetingsProcessed))
			}
			if err := scoped.UpdateSyncCheckpoint(syncID, checkpoint()); err != nil {
				return sum, err
			}
			// Recordings arrive in updatedAt order, so a limited run still
			// leaves a watermark the next run can resume from.
			if opts.Limit > 0 && sum.MeetingsProcessed >= int64(opts.Limit) {
				sum.PartialCoverage = index+1 < len(page.Records) || page.HasMore
				break scan
			}
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	if err := scoped.UpdateSyncCheckpoint(syncID, checkpoint()); err != nil {
		return sum, err
	}
	if !watermark.IsZero() {
		state.UpdatedAfter = watermark.UTC().Format(time.RFC3339Nano)
	}
	cursorAfter, err := json.Marshal(state, json.Deterministic(true))
	if err != nil {
		return sum, err
	}
	if err := scoped.CompleteSync(syncID, string(cursorAfter)); err != nil {
		return sum, err
	}
	return sum, nil
}

// importRecording archives one recording and reports whether it counted as
// processed. Evidence that can't be archived is skipped and recorded on the
// sync run so one bad recording can't stop the source.
func (imp *Importer) importRecording(ctx context.Context, archiver *meetingarchive.Archiver, scoped *store.Store, syncID, sourceID int64, opts ImportOptions, recording Recording, sum *ImportSummary) (bool, error) {
	skip := func(reason error) (bool, error) {
		sum.SkippedInvalid++
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf("skipped Twenty recording %s: %v", recording.ID, reason))
		}
		return false, scoped.RecordSyncRunItem(store.SyncRunItem{SyncRunID: syncID, SourceMessageID: "recording:" + recording.ID, Phase: "ingest", Status: store.SyncRunItemStatusSkipped, ErrorKind: "twenty_invalid_recording", ErrorMessage: reason.Error()})
	}
	if recording.TooLarge {
		return skip(errResponseTooLarge)
	}
	calendar := recording.Calendar
	if calendar == nil {
		archived, ok, err := imp.archivedCalendar(sourceID, recording.ID)
		if err != nil {
			return false, err
		}
		if ok {
			calendar = &archived
		}
	}
	snapshot, eligible, err := archiveSnapshot(sourceID, opts.AccountEmail, recording, calendar)
	if err != nil {
		return skip(err)
	}
	if !eligible {
		sum.SkippedEmpty++
		return false, nil
	}
	if !opts.StartedAfter.IsZero() && snapshot.StartedAt.Before(opts.StartedAfter) {
		return false, nil
	}
	sum.MeetingsProcessed++
	result, err := archiver.Upsert(ctx, snapshot, meetingarchive.UpsertOptions{Force: opts.Full})
	if result.Created {
		sum.MeetingsAdded++
	} else if result.Changed {
		sum.MeetingsUpdated++
	}
	return true, err
}

// archivedCalendar returns the calendar evidence a recording was last archived
// with. A deleted or unlinked event keeps the attendees already archived.
func (imp *Importer) archivedCalendar(sourceID int64, recordingID string) (Calendar, bool, error) {
	existing, err := imp.store.MessageMetadataBatch(sourceID, []string{"recording:" + recordingID})
	if err != nil {
		return Calendar{}, false, err
	}
	message, ok := existing["recording:"+recordingID]
	if !ok {
		return Calendar{}, false, nil
	}
	raw, err := imp.store.GetMessageRaw(message.ID)
	if err != nil {
		return Calendar{}, false, err
	}
	var envelope struct {
		Calendar     jsontext.Value   `json:"calendar_event"`
		Participants []jsontext.Value `json:"participants"`
	}
	_ = json.Unmarshal(raw, &envelope)
	if len(envelope.Calendar) == 0 || string(envelope.Calendar) == "null" {
		return Calendar{}, false, nil
	}
	return Calendar{Raw: envelope.Calendar, Participants: envelope.Participants}, true, nil
}
