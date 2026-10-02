package pocket

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

type ImportOptions struct {
	Identifier, AccountEmail string
	Full                     bool
	Limit                    int
	StartedAfter             time.Time
	Progress                 func(string)
}

type ImportSummary struct {
	SourceID                                                  int64
	MeetingsProcessed, MeetingsAdded, MeetingsUpdated, Errors int64
	Duration                                                  time.Duration
}

type Importer struct {
	store  *store.Store
	source Source
}

func NewImporter(st *store.Store, source Source) *Importer {
	return &Importer{store: st, source: source}
}

func RegisterSource(st *store.Store, identifier string, account Account) (*store.Source, error) {
	account, err := normalizedAccount(account)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(identifier) == "" {
		return nil, errors.New("pocket identifier is required")
	}
	src, err := st.GetOrCreateSource(SourceType, identifier)
	if err != nil {
		return nil, err
	}
	if err = st.BindMeetingSourceIdentity(src.ID, account.Email, account.UserID); err != nil {
		return nil, err
	}
	if err = st.UpdateSourceDisplayName(src.ID, identifier); err != nil {
		return nil, err
	}
	if err = st.AddAccountIdentity(src.ID, account.Email, "account-email"); err != nil {
		return nil, err
	}
	return st.GetSourceByID(src.ID)
}

func ValidateOwner(src *store.Source, account Account) error {
	account, err := normalizedAccount(account)
	if err != nil {
		return err
	}
	var bound struct {
		Email  string `json:"account_email"`
		UserID string `json:"account_user_id"`
	}
	if src == nil || src.SourceType != SourceType || !src.SyncConfig.Valid || json.Unmarshal([]byte(src.SyncConfig.String), &bound) != nil || bound.Email != account.Email || bound.UserID != account.UserID {
		return errors.New("pocket source identity does not match the authenticated account; register a new identifier")
	}
	return nil
}

type traversalState struct {
	Version  int              `json:"schema_version"`
	Sequence int64            `json:"sequence"`
	Attempts map[string]int64 `json:"attempts"`
}

func (i *Importer) loadState(sourceID int64, full bool) (traversalState, error) {
	state := traversalState{Version: 1, Attempts: map[string]int64{}}
	var raw string
	run, err := i.store.GetLatestCheckpointedSyncByType(sourceID, SourceType)
	if err == nil {
		raw = run.CursorBefore.String
	} else if !errors.Is(err, store.ErrSyncRunNotFound) {
		return state, err
	} else {
		run, err = i.store.GetLastSuccessfulSyncByType(sourceID, SourceType)
		if err == nil {
			raw = run.CursorAfter.String
		} else if !errors.Is(err, store.ErrSyncRunNotFound) {
			return state, err
		}
	}
	if raw == "" {
		return state, nil
	}
	var saved traversalState
	valid := len(raw) <= maxEvidenceBytes && json.Unmarshal([]byte(raw), &saved) == nil && saved.Version == 1 && saved.Sequence >= 0 && saved.Attempts != nil
	for id, seq := range saved.Attempts {
		if strings.TrimSpace(id) == "" || seq < 1 || seq > saved.Sequence {
			valid = false
		}
	}
	if !valid {
		if full {
			return state, nil
		}
		return state, errors.New("invalid Pocket sync state; run sync-pocket --full to repair it")
	}
	return saved, nil
}

func (i *Importer) enumerate(ctx context.Context) ([]Recording, error) {
	var records []Recording
	seen := map[string]bool{}
	total := -1
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		got, err := i.source.ListRecordings(ctx, page)
		if err != nil {
			return nil, err
		}
		if got.Page != page || got.Total < 0 || (total >= 0 && got.Total != total) || (got.HasMore && len(got.Recordings) == 0) {
			return nil, sectionError("pagination")
		}
		total = got.Total
		for _, rec := range got.Recordings {
			if strings.TrimSpace(rec.ID) == "" || seen[rec.ID] {
				return nil, sectionError("recording IDs")
			}
			seen[rec.ID] = true
			records = append(records, rec)
		}
		if len(records) > total {
			return nil, sectionError("recording total")
		}
		if !got.HasMore {
			if len(records) != total {
				return nil, sectionError("recording total")
			}
			return records, nil
		}
		if page == math.MaxInt {
			return nil, sectionError("pagination")
		}
	}
}

func mergeRecordingMetadata(recording, metadata Recording) Recording {
	if strings.TrimSpace(recording.Title) == "" {
		recording.Title = metadata.Title
	}
	if strings.TrimSpace(recording.RecordedAt) == "" && strings.TrimSpace(metadata.RecordedAt) != "" {
		recording.RecordedAt = metadata.RecordedAt
		recording.StartedAt = metadata.StartedAt
	}
	if strings.TrimSpace(recording.CreatedAt) == "" {
		recording.CreatedAt = metadata.CreatedAt
	}
	if strings.TrimSpace(recording.UpdatedAt) == "" {
		recording.UpdatedAt = metadata.UpdatedAt
	}
	if recording.StartedAt.IsZero() {
		recording.StartedAt = metadata.StartedAt
	}
	if recording.Duration == nil && metadata.Duration != nil {
		duration := *metadata.Duration
		recording.Duration = &duration
	}
	if metadata.Owner != nil {
		if recording.Owner == nil {
			owner := *metadata.Owner
			recording.Owner = &owner
		} else {
			if strings.TrimSpace(recording.Owner.Email) == "" {
				recording.Owner.Email = metadata.Owner.Email
			}
			if strings.TrimSpace(recording.Owner.UserID) == "" {
				recording.Owner.UserID = metadata.Owner.UserID
			}
			if strings.TrimSpace(recording.Owner.Name) == "" {
				recording.Owner.Name = metadata.Owner.Name
			}
		}
	}
	return recording
}

func (i *Importer) Import(ctx context.Context, opts ImportOptions) (summary *ImportSummary, err error) {
	summary = &ImportSummary{}
	started := time.Now()
	defer func() { summary.Duration = time.Since(started) }()
	if opts.Limit < 0 {
		return summary, errors.New("pocket limit must be nonnegative")
	}
	account, err := i.source.CurrentAccount(ctx)
	if err != nil {
		return summary, err
	}
	account, err = normalizedAccount(account)
	if err != nil {
		return summary, err
	}
	if account.Email != strings.ToLower(strings.TrimSpace(opts.AccountEmail)) {
		return summary, errors.New("pocket account email does not match configuration")
	}
	src, err := i.store.GetSourceByTypeAndIdentifier(SourceType, opts.Identifier)
	if err != nil {
		return summary, fmt.Errorf("register Pocket with add-pocket first: %w", err)
	}
	if err = ValidateOwner(src, account); err != nil {
		return summary, err
	}
	summary.SourceID = src.ID
	state, err := i.loadState(src.ID, opts.Full || !opts.StartedAfter.IsZero())
	if err != nil {
		return summary, err
	}
	runID, err := i.store.StartSyncContext(ctx, src.ID, SourceType)
	if err != nil {
		return summary, err
	}
	scoped := i.store.ScopedToSync(src.ID, runID)
	cp := &store.Checkpoint{}
	checkpoint := func() error {
		raw, e := json.Marshal(state, json.Deterministic(true))
		if e != nil {
			return e
		}
		if len(raw) > maxEvidenceBytes {
			return errors.New("pocket sync state exceeds size limit")
		}
		cp.PageToken = string(raw)
		cp.MessagesProcessed = summary.MeetingsProcessed
		cp.MessagesAdded = summary.MeetingsAdded
		cp.MessagesUpdated = summary.MeetingsUpdated
		cp.ErrorsCount = summary.Errors
		return scoped.UpdateSyncCheckpoint(runID, cp)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, scoped.FailSyncWithCheckpoint(runID, "Pocket sync failed; retry sync-pocket", cp))
		}
	}()
	// Save the previous valid traversal before discovery, so an incomplete
	// enumeration cannot replace attempt positions with a partial ID set.
	if err = checkpoint(); err != nil {
		return summary, err
	}
	records, err := i.enumerate(ctx)
	if err != nil {
		return summary, err
	}
	seen := make(map[string]bool, len(records))
	for _, rec := range records {
		seen[rec.ID] = true
	}
	for id := range state.Attempts {
		if !seen[id] {
			delete(state.Attempts, id)
		}
	}
	sort.Slice(records, func(a, b int) bool {
		left, right := records[a], records[b]
		if state.Attempts[left.ID] != state.Attempts[right.ID] {
			return state.Attempts[left.ID] < state.Attempts[right.ID]
		}
		if !left.StartedAt.Equal(right.StartedAt) {
			return left.StartedAt.After(right.StartedAt)
		}
		return left.ID < right.ID
	})
	archiver := meetingarchive.New(scoped)
	var failures []error
	for _, metadata := range records {
		if !opts.StartedAfter.IsZero() && (metadata.StartedAt.IsZero() || metadata.StartedAt.Before(opts.StartedAfter)) {
			continue
		}
		if opts.Limit > 0 && summary.MeetingsProcessed >= int64(opts.Limit) {
			break
		}
		if err = ctx.Err(); err != nil {
			return summary, err
		}
		if state.Sequence == math.MaxInt64 {
			return summary, errors.New("pocket attempt sequence exhausted")
		}
		state.Sequence++
		state.Attempts[metadata.ID] = state.Sequence
		summary.MeetingsProcessed++
		if err = checkpoint(); err != nil {
			return summary, err
		}
		rec, readErr := i.source.Recording(ctx, metadata.ID)
		var result meetingarchive.Result
		if readErr == nil && rec.ID != metadata.ID {
			readErr = sectionError("recording ID")
		}
		if readErr == nil {
			rec = mergeRecordingMetadata(rec, metadata)
		}
		if readErr == nil {
			readErr = validateRecordedOwner(rec, account)
		}
		if readErr == nil {
			var snapshot meetingarchive.Snapshot
			snapshot, readErr = buildSnapshot(scoped, src.ID, account, rec)
			if readErr == nil {
				result, readErr = archiver.Upsert(ctx, snapshot, meetingarchive.UpsertOptions{Force: opts.Full || !opts.StartedAfter.IsZero()})
			}
		}
		if result.Changed {
			if result.Created {
				summary.MeetingsAdded++
			} else {
				summary.MeetingsUpdated++
			}
		}
		if readErr != nil {
			summary.Errors++
			failures = append(failures, readErr)
		}
		if err = checkpoint(); err != nil {
			return summary, errors.Join(err, errors.Join(failures...))
		}
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf("Processed %d recordings", summary.MeetingsProcessed))
		}
	}
	if err = ctx.Err(); err != nil {
		return summary, errors.Join(err, errors.Join(failures...))
	}
	if len(failures) > 0 {
		return summary, fmt.Errorf("pocket recording sync failed: %w", errors.Join(failures...))
	}
	if err = checkpoint(); err != nil {
		return summary, err
	}
	err = scoped.CompleteSyncContext(ctx, runID, cp.PageToken)
	return summary, err
}

func validateRecordedOwner(rec Recording, account Account) error {
	if rec.Owner == nil {
		return nil
	}
	if rec.Owner.Email != "" && strings.ToLower(strings.TrimSpace(rec.Owner.Email)) != account.Email || rec.Owner.UserID != "" && rec.Owner.UserID != account.UserID {
		return errors.New("pocket recording belongs to another account")
	}
	return nil
}
