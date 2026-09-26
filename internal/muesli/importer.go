package muesli

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

const (
	syncStateVersion   = 1
	checkpointInterval = 50
)

// Importer archives the meetings in one Muesli database.
type Importer struct {
	store *store.Store
	now   func() time.Time
}

func NewImporter(st *store.Store) *Importer {
	return &Importer{store: st, now: time.Now}
}

type ImportOptions struct {
	Identifier   string
	AccountEmail string
	DBPath       string
	// Full rewrites every archived meeting even when its evidence matches,
	// which refreshes attribution after identity changes.
	Full bool
	// Limit caps the eligible meetings processed; 0 means all.
	Limit int
	// StartedAfter keeps meetings that start on or after this time.
	StartedAfter time.Time
	// ContactsEnabled resolves attendees through the Mac's Contacts stores
	// under ContactsPath. PhoneCountryCode converts national-format phones.
	ContactsEnabled  bool
	ContactsPath     string
	PhoneCountryCode string
	Progress         func(string)
}

type ImportSummary struct {
	SourceID          int64
	MeetingsProcessed int64
	MeetingsAdded     int64
	MeetingsUpdated   int64
	SkippedDeleted    int64
	SkippedInProgress int64
	SkippedEmpty      int64
	Errors            int64
	// ContactsState is how much of the Mac's Contacts the sync could read.
	ContactsState ContactsState
	Duration      time.Duration
}

// syncState is recorded for diagnostics only. Every run rescans the whole
// database because Muesli does not bump updated_at for participant or folder
// edits; the archiver skips meetings whose evidence is unchanged.
type syncState struct {
	Version   int    `json:"version"`
	ScannedAt string `json:"scanned_at"`
}

// Import runs one sync of the Muesli database at opts.DBPath into the
// registered source opts.Identifier.
func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (sum *ImportSummary, retErr error) {
	if imp == nil || imp.store == nil {
		return nil, errors.New("muesli importer is unavailable")
	}
	started := imp.now()
	identifier := strings.TrimSpace(opts.Identifier)
	source, err := imp.store.GetSourceByTypeAndIdentifier(SourceType, identifier)
	if err != nil {
		if errors.Is(err, store.ErrSourceNotFound) {
			return nil, fmt.Errorf("muesli source %q is not registered; run msgvault add-muesli %s first",
				identifier, identifier)
		}
		return nil, fmt.Errorf("look up Muesli source %q: %w", identifier, err)
	}
	sum = &ImportSummary{SourceID: source.ID}
	if strings.TrimSpace(opts.AccountEmail) != "" {
		if err := imp.store.AddAccountIdentityContext(ctx, source.ID, opts.AccountEmail, "account-email"); err != nil {
			return sum, fmt.Errorf("confirm Muesli account identity: %w", err)
		}
	}

	syncID, err := imp.store.StartSync(source.ID, SourceType)
	if err != nil {
		return sum, err
	}
	// A superseded or failed run must not keep writing, so every write below
	// goes through the sync-scoped store.
	scoped := imp.store.ScopedToSync(source.ID, syncID)
	checkpoint := func() *store.Checkpoint {
		return &store.Checkpoint{
			MessagesProcessed: sum.MeetingsProcessed,
			MessagesAdded:     sum.MeetingsAdded,
			MessagesUpdated:   sum.MeetingsUpdated,
			ErrorsCount:       sum.Errors,
		}
	}
	defer func() {
		sum.Duration = imp.now().Sub(started)
		if retErr != nil {
			_ = scoped.FailSyncWithCheckpoint(syncID, withoutPath(retErr.Error(), opts.DBPath), checkpoint())
		}
	}()

	reader, err := Open(ctx, opts.DBPath)
	if err != nil {
		sum.Errors++
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
		sum.Errors++
		return sum, err
	}
	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}
	archiver := meetingarchive.New(scoped)
	var meetingErrors []error
	for _, meeting := range meetings {
		if err := ctx.Err(); err != nil {
			return sum, err
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
			startedAt, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(meeting.StartTime))
			if parseErr == nil && startedAt.Before(opts.StartedAfter) {
				continue
			}
		}
		if opts.Limit > 0 && sum.MeetingsProcessed >= int64(opts.Limit) {
			break
		}
		sum.MeetingsProcessed++

		if err := imp.resolveParticipants(source.ID, &meeting, contacts, opts.PhoneCountryCode); err != nil {
			return sum, err
		}
		snapshot, err := meeting.ArchiveSnapshot(source.ID, identifier, opts.AccountEmail)
		if err != nil {
			sum.Errors++
			meetingErrors = append(meetingErrors, fmt.Errorf("muesli meeting %d: %w", meeting.ID, err))
			continue
		}
		result, err := archiver.Upsert(ctx, snapshot, meetingarchive.UpsertOptions{Force: opts.Full})
		// Upsert can commit and then fail conversation-stat maintenance, so
		// count the write before looking at the error.
		switch {
		case result.Created:
			sum.MeetingsAdded++
			progress(fmt.Sprintf("added Muesli meeting %d", meeting.ID))
		case result.Changed:
			sum.MeetingsUpdated++
			progress(fmt.Sprintf("updated Muesli meeting %d", meeting.ID))
		}
		if err != nil {
			sum.Errors++
			meetingErrors = append(meetingErrors, fmt.Errorf("muesli meeting %d: %w", meeting.ID, err))
			continue
		}
		if sum.MeetingsProcessed%checkpointInterval == 0 {
			if err := scoped.UpdateSyncCheckpoint(syncID, checkpoint()); err != nil {
				return sum, err
			}
		}
	}
	if len(meetingErrors) > 0 {
		return sum, errors.Join(meetingErrors...)
	}

	if err := scoped.UpdateSyncCheckpoint(syncID, checkpoint()); err != nil {
		return sum, err
	}
	cursor, err := json.Marshal(syncState{
		Version:   syncStateVersion,
		ScannedAt: imp.now().UTC().Format(time.RFC3339),
	}, json.Deterministic(true))
	if err != nil {
		return sum, fmt.Errorf("marshal Muesli sync cursor: %w", err)
	}
	if err := scoped.CompleteSync(syncID, string(cursor)); err != nil {
		return sum, err
	}
	return sum, nil
}

// withoutPath keeps the database location, which names the local user and
// folder layout, out of the sync history stored in the archive. The error
// returned to the caller still names it.
func withoutPath(message, dbPath string) string {
	if abs, err := filepath.Abs(dbPath); err == nil && abs != "" {
		message = strings.ReplaceAll(message, abs, "<db_path>")
	}
	if dbPath != "" {
		message = strings.ReplaceAll(message, dbPath, "<db_path>")
	}
	return message
}
