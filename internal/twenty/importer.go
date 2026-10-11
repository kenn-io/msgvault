package twenty

import (
	"context"
	"database/sql"
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
	// FullRescan reports that the run rescanned every recording, either by
	// request or to finish an earlier full rescan.
	FullRescan bool
	Duration   time.Duration
}

// overlap re-reads recordings updated shortly before the watermark. An edit
// that commits after a scan has passed its updatedAt would otherwise wait for
// a full rescan. Recordings already handled at the same updatedAt are skipped.
const overlap = 5 * time.Minute

// syncState is the JSON cursor persisted in sync_runs.cursor_after.
type syncState struct {
	Incremental scanPosition `json:"incremental"`
	// Full is the progress of a full rescan that a limit stopped. Later runs
	// resume it, still forcing archive refreshes, until it reaches the end.
	Full *scanPosition `json:"full,omitempty"`
}

// scanPosition is where a scan in updatedAt order resumes.
type scanPosition struct {
	// UpdatedAfter is the newest updatedAt among recordings already handled.
	UpdatedAfter string `json:"updated_after,omitempty"`
	// Recent maps the recordings handled within the overlap before
	// UpdatedAfter to the updatedAt they were handled at.
	Recent map[string]string `json:"recent,omitempty"`
}

// position tracks a scanPosition while a scan advances it.
type position struct {
	watermark time.Time
	recent    map[string]time.Time
}

// newPosition starts from a saved position. An unreadable time starts the
// scan from the beginning, which re-reads recordings but loses none.
func newPosition(saved scanPosition) position {
	p := position{recent: map[string]time.Time{}}
	if watermark, err := time.Parse(time.RFC3339Nano, saved.UpdatedAfter); err == nil {
		p.watermark = watermark
	}
	for id, value := range saved.Recent {
		if updated, err := time.Parse(time.RFC3339Nano, value); err == nil {
			p.recent[id] = updated
		}
	}
	return p
}

// since is the updatedAt lower bound for the next listing.
func (p *position) since() time.Time {
	if p.watermark.IsZero() {
		return time.Unix(0, 0).UTC()
	}
	return p.watermark.Add(-overlap)
}

func (p *position) handled(id string, updated time.Time) bool {
	at, ok := p.recent[id]
	return ok && at.Equal(updated)
}

func (p *position) record(id string, updated time.Time) {
	if updated.After(p.watermark) {
		p.watermark = updated
	}
	p.recent[id] = updated
}

func (p *position) saved() scanPosition {
	if p.watermark.IsZero() {
		return scanPosition{}
	}
	saved := scanPosition{UpdatedAfter: p.watermark.UTC().Format(time.RFC3339Nano), Recent: map[string]string{}}
	floor := p.since()
	for id, updated := range p.recent {
		if !updated.Before(floor) {
			saved.Recent[id] = updated.UTC().Format(time.RFC3339Nano)
		}
	}
	return saved
}

// importRun holds what one Import call shares across its recordings.
type importRun struct {
	opts     ImportOptions
	sum      *ImportSummary
	scoped   *store.Store
	archiver *meetingarchive.Archiver
	syncID   int64
	sourceID int64
}

func (r *importRun) progress(line string) {
	if r.opts.Progress != nil {
		r.opts.Progress(line)
	}
}

// Import lists recordings in updatedAt order from the last run's watermark,
// so late transcripts and summaries, which update the recording, are picked
// up without rescanning the catalog. Full rescans every recording; a full
// rescan that a limit stops is resumed by later runs until it completes.
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
	run := &importRun{opts: opts, sum: sum, scoped: imp.store.ScopedToSync(source.ID, syncID), syncID: syncID, sourceID: source.ID}
	run.archiver = meetingarchive.New(run.scoped)
	defer func() {
		sum.Duration = time.Since(started)
		if retErr != nil {
			_ = run.scoped.FailSyncWithCheckpoint(syncID, "Twenty sync failed", run.checkpoint())
		}
	}()
	if err := run.scoped.AddAccountIdentityContext(ctx, source.ID, opts.AccountEmail, "account-email"); err != nil {
		return sum, err
	}
	var state syncState
	if previous, err := imp.store.GetLastSuccessfulSync(source.ID); err == nil && previous.CursorAfter.Valid {
		// An unreadable cursor restarts discovery from the beginning.
		_ = json.Unmarshal([]byte(previous.CursorAfter.String), &state)
	} else if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return sum, err
	}
	// Recordings outside --after are listed but not archived, so that run
	// must not move either position past them.
	advance := opts.StartedAfter.IsZero()
	resume := advance && state.Full != nil
	run.opts.Full = opts.Full || resume
	sum.FullRescan = run.opts.Full
	pos := newPosition(state.Incremental)
	switch {
	case resume:
		pos = newPosition(*state.Full)
		run.progress("resuming the unfinished full rescan")
	case run.opts.Full:
		pos = newPosition(scanPosition{})
	}
	if err := imp.scan(ctx, run, &pos, advance); err != nil {
		return sum, err
	}
	if err := run.scoped.UpdateSyncCheckpoint(syncID, run.checkpoint()); err != nil {
		return sum, err
	}
	if advance {
		switch saved := pos.saved(); {
		case run.opts.Full && sum.PartialCoverage:
			state.Full = &saved
		case run.opts.Full:
			// A completed rescan has seen every recording, so its position
			// is also where incremental runs continue.
			state.Full, state.Incremental = nil, saved
		default:
			state.Incremental = saved
		}
	}
	cursorAfter, err := json.Marshal(state, json.Deterministic(true))
	if err != nil {
		return sum, err
	}
	if err := run.scoped.CompleteSync(syncID, string(cursorAfter)); err != nil {
		return sum, err
	}
	return sum, nil
}

func (r *importRun) checkpoint() *store.Checkpoint {
	return &store.Checkpoint{MessagesProcessed: r.sum.MeetingsProcessed, MessagesAdded: r.sum.MeetingsAdded, MessagesUpdated: r.sum.MeetingsUpdated}
}

// scan walks recordings from pos, advancing it unless the run is filtered
// by --after, and stops early once the limit is reached.
func (imp *Importer) scan(ctx context.Context, run *importRun, pos *position, advance bool) error {
	since := pos.since().Format(time.RFC3339Nano)
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := imp.source.ListRecordings(ctx, since, cursor, defaultPageSize)
		if err != nil {
			return err
		}
		if page == nil {
			return errors.New("twenty returned no recording page")
		}
		for index, recording := range page.Records {
			if err := ctx.Err(); err != nil {
				return err
			}
			if recording.ID == "" {
				return errors.New("twenty discovery returned a recording without an ID")
			}
			updated, updatedErr := time.Parse(time.RFC3339Nano, recording.UpdatedAt)
			if updatedErr == nil && pos.handled(recording.ID, updated) {
				continue
			}
			processed, err := imp.importRecording(ctx, run, recording)
			if err != nil {
				return err
			}
			if updatedErr == nil && advance {
				pos.record(recording.ID, updated)
			}
			if !processed {
				continue
			}
			run.progress(fmt.Sprintf("processed Twenty meeting %d", run.sum.MeetingsProcessed))
			if err := run.scoped.UpdateSyncCheckpoint(run.syncID, run.checkpoint()); err != nil {
				return err
			}
			// Recordings arrive in updatedAt order, so a limited run still
			// leaves a position the next run can resume from.
			if run.opts.Limit > 0 && run.sum.MeetingsProcessed >= int64(run.opts.Limit) {
				run.sum.PartialCoverage = index+1 < len(page.Records) || page.HasMore
				return nil
			}
		}
		if !page.HasMore {
			return nil
		}
		cursor = page.NextCursor
	}
}

// importRecording archives one recording and reports whether it counted as
// processed. Evidence that can't be archived is skipped and recorded on the
// sync run so one bad recording can't stop the source.
func (imp *Importer) importRecording(ctx context.Context, run *importRun, recording Recording) (bool, error) {
	skip := func(reason error) (bool, error) {
		run.sum.SkippedInvalid++
		run.progress(fmt.Sprintf("skipped Twenty recording %s: %v", recording.ID, reason))
		return false, run.scoped.RecordSyncRunItem(store.SyncRunItem{SyncRunID: run.syncID, SourceMessageID: "recording:" + recording.ID, Phase: "ingest", Status: store.SyncRunItemStatusSkipped, ErrorKind: "twenty_invalid_recording", ErrorMessage: reason.Error()})
	}
	if recording.TooLarge {
		return skip(errResponseTooLarge)
	}
	calendar := recording.Calendar
	if calendar == nil {
		archived, ok, err := imp.archivedCalendar(run.sourceID, recording)
		if err != nil {
			return false, err
		}
		if ok {
			calendar = &archived
		}
	}
	snapshot, eligible, err := archiveSnapshot(run.sourceID, run.opts.AccountEmail, recording, calendar)
	if err != nil {
		return skip(err)
	}
	if !eligible {
		run.sum.SkippedEmpty++
		return false, nil
	}
	if !run.opts.StartedAfter.IsZero() && snapshot.StartedAt.Before(run.opts.StartedAfter) {
		return false, nil
	}
	run.sum.MeetingsProcessed++
	result, err := run.archiver.Upsert(ctx, snapshot, meetingarchive.UpsertOptions{Force: run.opts.Full})
	if result.Created {
		run.sum.MeetingsAdded++
	} else if result.Changed {
		run.sum.MeetingsUpdated++
	}
	return true, err
}

// archivedCalendar returns the calendar evidence a recording was last
// archived with, so a deleted or unlinked event keeps the attendees already
// archived. A recording now linked to a different event that can't be read
// doesn't inherit the old event.
func (imp *Importer) archivedCalendar(sourceID int64, recording Recording) (Calendar, bool, error) {
	sourceMessageID := "recording:" + recording.ID
	existing, err := imp.store.MessageMetadataBatch(sourceID, []string{sourceMessageID})
	if err != nil {
		return Calendar{}, false, err
	}
	message, ok := existing[sourceMessageID]
	if !ok {
		return Calendar{}, false, nil
	}
	raw, err := imp.store.GetMessageRaw(message.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return Calendar{}, false, nil
	}
	if err != nil {
		return Calendar{}, false, err
	}
	var envelope struct {
		Calendar     jsontext.Value   `json:"calendar_event"`
		Participants []jsontext.Value `json:"participants"`
	}
	var event struct {
		ID string `json:"id"`
	}
	// Evidence without a readable event has no attendees to keep.
	readable := json.Unmarshal(raw, &envelope) == nil && len(envelope.Calendar) > 0 &&
		string(envelope.Calendar) != "null" && json.Unmarshal(envelope.Calendar, &event) == nil
	relinked := recording.CalendarEventID != "" && recording.CalendarEventID != event.ID
	if !readable || relinked {
		return Calendar{}, false, nil
	}
	return Calendar{Raw: envelope.Calendar, Participants: envelope.Participants}, true, nil
}
