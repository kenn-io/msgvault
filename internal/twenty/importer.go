package twenty

import (
	"context"
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
	PartialCoverage   bool
	Duration          time.Duration
}

// Import rescans immutable-ID ordered discovery on every run. Provider edits
// and late transcripts need not change a discovery timestamp to converge.
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
	archiver := meetingarchive.New(scoped)
	seenIDs := map[string]bool{}
	seenCursors := map[string]bool{"": true}
	cursor := ""
scan:
	for {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		page, err := imp.source.ListRecordings(ctx, cursor, pageSize)
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
			if recording.ID == "" || seenIDs[recording.ID] {
				return sum, errors.New("twenty discovery returned a missing or duplicate recording ID")
			}
			seenIDs[recording.ID] = true
			var calendar *Calendar
			if recording.CalendarEventID != "" {
				calendar, err = imp.source.GetCalendar(ctx, recording.CalendarEventID)
				if errors.Is(err, errLinkedCalendarUnavailable) {
					calendar = nil
				} else if err != nil {
					return sum, err
				}
			}
			snapshot, eligible, err := archiveSnapshot(source.ID, opts.AccountEmail, recording, calendar)
			if err != nil {
				return sum, err
			}
			if !eligible {
				sum.SkippedEmpty++
				continue
			}
			if !opts.StartedAfter.IsZero() && snapshot.StartedAt.Before(opts.StartedAfter) {
				continue
			}
			sum.MeetingsProcessed++
			result, err := archiver.Upsert(ctx, snapshot, meetingarchive.UpsertOptions{Force: opts.Full})
			if result.Created {
				sum.MeetingsAdded++
			} else if result.Changed {
				sum.MeetingsUpdated++
			}
			if err != nil {
				return sum, err
			}
			if opts.Progress != nil {
				opts.Progress(fmt.Sprintf("processed Twenty meeting %d", sum.MeetingsProcessed))
			}
			if err := scoped.UpdateSyncCheckpoint(syncID, checkpoint()); err != nil {
				return sum, err
			}
			if opts.Limit > 0 && sum.MeetingsProcessed >= int64(opts.Limit) {
				sum.PartialCoverage = index+1 < len(page.Records) || page.HasMore
				break scan
			}
		}
		if !page.HasMore {
			break
		}
		if page.NextCursor == "" || seenCursors[page.NextCursor] {
			return sum, errors.New("twenty discovery cursor did not advance")
		}
		seenCursors[page.NextCursor] = true
		cursor = page.NextCursor
	}
	if err := scoped.UpdateSyncCheckpoint(syncID, checkpoint()); err != nil {
		return sum, err
	}
	state, err := json.Marshal(map[string]any{"version": 1, "scanned_at": time.Now().UTC().Format(time.RFC3339), "partial_coverage": sum.PartialCoverage}, json.Deterministic(true))
	if err != nil {
		return sum, err
	}
	if err := scoped.CompleteSync(syncID, string(state)); err != nil {
		return sum, err
	}
	return sum, nil
}
