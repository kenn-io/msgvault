package plaud

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

// SourceType identifies Plaud accounts in the archive.
const SourceType = "plaud"

// ImportOptions controls one sync of a registered Plaud account.
type ImportOptions struct {
	Identifier   string
	AccountEmail string
	Full         bool
	Limit        int
	CreatedAfter *time.Time
	Progress     func(current, total int, title string)
}

// ImportSummary reports committed changes and errors from a sync.
type ImportSummary struct {
	SourceID          int64
	MeetingsProcessed int64
	MeetingsAdded     int64
	MeetingsUpdated   int64
	Errors            int64
	Duration          time.Duration
}

// Importer archives recording metadata, transcripts, and notes from Plaud.
type Importer struct {
	store  *store.Store
	client Source
	now    func() time.Time
}

func NewImporter(st *store.Store, src Source) *Importer {
	return &Importer{store: st, client: src, now: time.Now}
}

type syncState struct {
	Version     int                  `json:"version"`
	LastChecked map[string]time.Time `json:"last_checked"`
}

func normalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	a, err := mail.ParseAddress(email)
	if err != nil || a.Address != email {
		return "", errors.New("plaud account_email must be an explicit email address")
	}
	return email, nil
}

func ValidateOwner(src *store.Source, email string) error {
	email, err := normalizeEmail(email)
	if err != nil {
		return err
	}
	var v struct {
		Email string `json:"account_email"`
	}
	if src == nil || !src.SyncConfig.Valid || json.Unmarshal([]byte(src.SyncConfig.String), &v) != nil || v.Email != email {
		return errors.New("plaud source owner differs or is unconfirmed; use a new identifier for another account")
	}
	return nil
}

func RegisterSource(st *store.Store, identifier, email string) (*store.Source, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return nil, err
	}
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return nil, errors.New("plaud identifier is required")
	}
	src, err := st.GetOrCreateSource(SourceType, identifier)
	if err != nil {
		return nil, err
	}
	// The local identifier can outlive configuration edits. Reauthorization
	// must not reassign its archive to another account.
	if err := st.BindMeetingSourceOwner(src.ID, email); err != nil {
		return nil, err
	}
	if err := st.UpdateSourceDisplayName(src.ID, identifier); err != nil {
		return nil, err
	}
	if err := st.AddAccountIdentity(src.ID, email, "account-email"); err != nil {
		return nil, err
	}
	return st.GetSourceByTypeAndIdentifier(SourceType, identifier)
}

func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (sum *ImportSummary, retErr error) {
	if opts.Limit < 0 {
		return nil, errors.New("plaud limit cannot be negative")
	}
	src, err := imp.store.GetSourceByTypeAndIdentifier(SourceType, opts.Identifier)
	if err != nil {
		return nil, fmt.Errorf("plaud source is not registered; run msgvault add-plaud %s: %w", opts.Identifier, err)
	}
	if err := ValidateOwner(src, opts.AccountEmail); err != nil {
		return nil, err
	}
	email, err := imp.client.CurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := ValidateOwner(src, email); err != nil {
		return nil, fmt.Errorf("confirm live plaud account: %w", err)
	}
	started := imp.now().UTC()
	sum = &ImportSummary{SourceID: src.ID}
	run, err := imp.store.StartSync(src.ID, SourceType)
	if err != nil {
		return sum, err
	}
	scoped := imp.store.ScopedToSync(src.ID, run)
	checkpoint := func() *store.Checkpoint {
		return &store.Checkpoint{
			MessagesProcessed: sum.MeetingsProcessed,
			MessagesAdded:     sum.MeetingsAdded,
			MessagesUpdated:   sum.MeetingsUpdated,
			ErrorsCount:       sum.Errors,
		}
	}
	defer func() {
		sum.Duration = imp.now().UTC().Sub(started)
		if retErr != nil {
			retErr = errors.Join(retErr, scoped.FailSyncWithCheckpoint(run, retErr.Error(), checkpoint()))
		}
	}()
	// Read the current rotation state after acquiring this source's sync run.
	current, err := scoped.GetSourceByID(src.ID)
	if err != nil {
		return sum, err
	}
	state := syncState{Version: 1, LastChecked: map[string]time.Time{}}
	if current.SyncCursor.String != "" {
		if json.Unmarshal([]byte(current.SyncCursor.String), &state) != nil ||
			state.Version != 1 || state.LastChecked == nil {
			return sum, errors.New("invalid plaud sync state")
		}
	}
	files, err := imp.enumerate(ctx)
	if err != nil {
		sum.Errors++
		return sum, err
	}
	sort.Slice(files, func(i, j int) bool {
		a, b := state.LastChecked[files[i].ID], state.LastChecked[files[j].ID]
		if !a.Equal(b) {
			return a.Before(b)
		}
		fileDateI, fileDateJ := recordingDate(files[i]), recordingDate(files[j])
		if !fileDateI.Equal(fileDateJ) {
			return fileDateI.After(fileDateJ)
		}
		return files[i].ID < files[j].ID
	})
	candidates := make([]File, 0, len(files))
	for _, f := range files {
		date := recordingDate(f)
		if opts.CreatedAfter != nil && (date.IsZero() || date.Before(*opts.CreatedAfter)) {
			continue
		}
		candidates = append(candidates, f)
	}
	if opts.Limit > 0 && len(candidates) > opts.Limit {
		candidates = candidates[:opts.Limit]
	}
	archiver := meetingarchive.New(scoped)
	var failures []error
	for index, f := range candidates {
		if err := ctx.Err(); err != nil {
			sum.Errors++
			failures = append(failures, err)
			break
		}
		sum.MeetingsProcessed++
		// A failed recording must not hold every later limited run in place.
		state.LastChecked[f.ID] = started
		rec, err := imp.client.Recording(ctx, f.ID)
		if err == nil && rec.File.ID != f.ID {
			err = fmt.Errorf("%w: detail file identity mismatch", ErrContract)
		}
		if err != nil {
			sum.Errors++
			failures = append(failures, fmt.Errorf("plaud recording %s: %w", f.ID, err))
			continue
		}
		e := normalized(rec)
		existing, err := scoped.MessageExistsBatch(src.ID, []string{f.ID})
		if err == nil && existing[f.ID] != 0 {
			var raw []byte
			raw, err = scoped.GetMessageRaw(existing[f.ID])
			if err == nil {
				var old evidence
				if json.Unmarshal(raw, &old) != nil || old.Version != 1 || old.FileID != f.ID {
					err = errors.New("invalid archived plaud evidence")
				} else {
					e.preserve(old)
				}
			}
		}
		if err != nil {
			sum.Errors++
			failures = append(failures, err)
			continue
		}
		snapshot, err := e.snapshot(src.ID, email)
		if err == nil {
			var result meetingarchive.Result
			result, err = archiver.Upsert(ctx, snapshot, meetingarchive.UpsertOptions{Force: opts.Full})
			if result.Created {
				sum.MeetingsAdded++
			} else if result.Changed {
				sum.MeetingsUpdated++
			}
		}
		if err != nil {
			sum.Errors++
			failures = append(failures, err)
			continue
		}
		if opts.Progress != nil {
			opts.Progress(index+1, len(candidates), f.Name)
		}
	}
	raw, err := json.Marshal(state, json.Deterministic(true))
	if err != nil {
		return sum, errors.Join(errors.Join(failures...), err)
	}
	// Rotation is current source state, not a snapshot to retain in every run.
	if err := scoped.UpdateSourceSyncState(src.ID, string(raw)); err != nil {
		return sum, errors.Join(errors.Join(failures...), err)
	}
	if len(failures) > 0 {
		return sum, errors.Join(failures...)
	}
	if err := ctx.Err(); err != nil {
		sum.Errors++
		return sum, err
	}
	if err := scoped.UpdateSyncCheckpoint(run, checkpoint()); err != nil {
		return sum, err
	}
	return sum, scoped.CompleteSync(run, "")
}

func recordingDate(file File) time.Time {
	if !file.StartedAt.IsZero() {
		return file.StartedAt
	}
	return file.CreatedAt
}

func (imp *Importer) enumerate(ctx context.Context) ([]File, error) {
	var files []File
	seen := map[string]bool{}
	total := -1
	for page := 1; ; page++ {
		p, err := imp.client.ListFiles(ctx, page, 100)
		if err != nil {
			return nil, err
		}
		if p.Complete != nil && !*p.Complete {
			return nil, fmt.Errorf("%w: incomplete recording list", ErrContract)
		}
		if p.Total != nil {
			if *p.Total < 0 || total >= 0 && total != *p.Total {
				return nil, fmt.Errorf("%w: inconsistent recording total", ErrContract)
			}
			total = *p.Total
		}
		for _, f := range p.Files {
			if strings.TrimSpace(f.ID) == "" || seen[f.ID] {
				return nil, fmt.Errorf("%w: blank or repeated file id", ErrContract)
			}
			seen[f.ID] = true
			files = append(files, f)
		}
		if total >= 0 && len(files) > total {
			return nil, fmt.Errorf("%w: recording count exceeds total", ErrContract)
		}
		terminal := len(p.Files) == 0 || p.HasMore != nil && !*p.HasMore
		if len(p.Files) == 0 && p.HasMore != nil && *p.HasMore {
			return nil, fmt.Errorf("%w: empty nonterminal recording page", ErrContract)
		}
		if terminal {
			if total >= 0 && len(files) != total {
				return nil, fmt.Errorf("%w: recording list ended before total", ErrContract)
			}
			return files, nil
		}
	}
}
