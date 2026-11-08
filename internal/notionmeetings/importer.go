package notionmeetings

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
)

const (
	syncStateVersion           = 5
	notionSnapshotVersion      = 2
	transcriptRetryCadence     = 6 * time.Hour
	transcriptRetryWindow      = 7 * 24 * time.Hour
	unknownTranscriptRetrySpan = 48 * time.Hour
)

// Source is the read-only Notion surface required by a sync run.
type Source interface {
	hydrationSource
	QueryMeetingNotes(ctx context.Context, limit int) (*QueryResult, error)
}

type Importer struct {
	limits discoveryLimits
	users  UserSource
	store  *store.Store
	client Source
	now    func() time.Time
}

func NewImporter(st *store.Store, client Source) *Importer {
	return &Importer{store: st, client: client, now: time.Now, limits: defaultDiscoveryLimits()}
}

// WithUserSource resolves attendees with a separate workspace users token.
func (imp *Importer) WithUserSource(users UserSource) *Importer {
	imp.users = users
	return imp
}

type ImportOptions struct {
	TokenType         string
	UserID            string
	RootPageIDs       []string
	RootDataSourceIDs []string
	MeetingBlockIDs   []string
	SearchAccessible  *bool
	Identifier        string
	AccountEmail      string
	Full              bool
	Limit             int
	CreatedAfter      time.Time
	Progress          func(string)
}

type ImportSummary struct {
	IdentityProvenance string
	ScopeCounts        map[string]int
	WorkRemaining      int
	CoverageReasons    map[string]int
	SourceID           int64
	MeetingsProcessed  int64
	MeetingsAdded      int64
	MeetingsUpdated    int64
	MaintenanceRetries int64
	Errors             int64
	PartialCoverage    bool
	Duration           time.Duration
}

type knownMeeting struct {
	LastAttemptAt    string            `json:"last_attempt_at,omitempty"`
	LastSuccessAt    string            `json:"last_success_at,omitempty"`
	FetchError       string            `json:"fetch_error,omitempty"`
	SectionFreshness map[string]string `json:"section_freshness,omitempty"`
	LastEditedTime   string            `json:"last_edited_time"`
	SnapshotSHA256   string            `json:"snapshot_sha256,omitempty"`
	SnapshotVersion  int               `json:"snapshot_version"`
}

type pendingTranscript struct {
	BlockID       string `json:"block_id"`
	NextAttemptAt string `json:"next_attempt_at"`
	RetryUntil    string `json:"retry_until"`
}

type Coverage struct {
	Reasons         map[string]int       `json:"reasons,omitempty"`
	Diagnostics     []coverageDiagnostic `json:"diagnostics,omitempty"`
	DiagnosticCount int                  `json:"diagnostic_count,omitempty"`
	HasMore         bool                 `json:"has_more"`
	Returned        int                  `json:"returned"`
	OldestEdit      string               `json:"oldest_edit,omitempty"`
	NewestEdit      string               `json:"newest_edit,omitempty"`
	Limited         bool                 `json:"limited,omitzero"`
}

type syncState struct {
	Fingerprint          string                  `json:"scope_fingerprint,omitempty"`
	Identity             Identity                `json:"identity"`
	Admissions           []string                `json:"admissions,omitempty"`
	Frontier             []scopeTask             `json:"frontier,omitempty"`
	Windows              []queryWindow           `json:"query_windows,omitempty"`
	Deferred             []deferredScope         `json:"deferred,omitempty"`
	Seen                 map[string]bool         `json:"visited,omitempty"`
	Active               bool                    `json:"active"`
	FreshRequested       bool                    `json:"fresh_requested"`
	SeedNext             int                     `json:"seed_next,omitempty"`
	NextLane             int                     `json:"next_lane,omitempty"`
	RefreshCursor        string                  `json:"refresh_cursor,omitempty"`
	RefreshKnownNext     bool                    `json:"refresh_known_next,omitempty"`
	Version              int                     `json:"version"`
	LastQueryAt          string                  `json:"last_query_at"`
	Known                map[string]knownMeeting `json:"known"`
	Pending              []pendingTranscript     `json:"pending,omitempty"`
	PendingBacklog       []pendingTranscript     `json:"pending_backlog,omitempty"`
	TranscriptRetryUntil map[string]string       `json:"transcript_retry_until,omitempty"`
	Coverage             Coverage                `json:"coverage"`
}

func (state *syncState) marshal() (string, error) {
	s := *state
	sort.Slice(s.Pending, func(i, j int) bool { return s.Pending[i].BlockID < s.Pending[j].BlockID })
	if err := s.validateTransient(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(s, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("marshal Notion sync cursor: %w", err)
	}
	return string(encoded), nil
}

func (state *syncState) validate() error {
	s := *state
	if s.Version != 1 && s.Version != 2 && s.Version != 3 && s.Version != 4 && s.Version != syncStateVersion {
		return fmt.Errorf("unsupported Notion sync cursor version %d", s.Version)
	}
	allPending := append(append([]pendingTranscript(nil), s.Pending...), s.PendingBacklog...)
	seen := make(map[string]struct{}, len(allPending))
	for index, pending := range allPending {
		if strings.TrimSpace(pending.BlockID) == "" {
			return fmt.Errorf("pending[%d] has a blank block_id", index)
		}
		if _, duplicate := seen[pending.BlockID]; duplicate {
			return errors.New("pending contains duplicate block_id")
		}
		seen[pending.BlockID] = struct{}{}
		if _, err := time.Parse(time.RFC3339, pending.NextAttemptAt); err != nil {
			return errors.New("pending.next_attempt_at must be an RFC3339 timestamp")
		}
		if _, err := time.Parse(time.RFC3339, pending.RetryUntil); err != nil {
			return errors.New("pending.retry_until must be an RFC3339 timestamp")
		}
	}
	deadlineIDs := make([]string, 0, len(s.TranscriptRetryUntil))
	for id := range s.TranscriptRetryUntil {
		deadlineIDs = append(deadlineIDs, id)
	}
	sort.Strings(deadlineIDs)
	for _, id := range deadlineIDs {
		if strings.TrimSpace(id) == "" {
			return errors.New("transcript retry deadline has a blank block ID")
		}
		if _, err := time.Parse(time.RFC3339, s.TranscriptRetryUntil[id]); err != nil {
			return errors.New("transcript retry deadline must use RFC3339")
		}
	}
	if s.Version == 1 {
		// Legacy cursors predate the active pending queue limit.
		s.Pending = nil
	}
	return s.validateDiscovery()
}

func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (sum *ImportSummary, retErr error) {
	if imp == nil || imp.store == nil || imp.client == nil {
		return nil, errors.New("notion meeting importer is unavailable")
	}
	if err := validateDiscoveryOptions(opts); err != nil {
		return nil, err
	}
	limits := imp.limits
	if limits.Duration <= 0 {
		limits = defaultDiscoveryLimits()
	}
	ctx, cancel := context.WithTimeout(ctx, limits.Duration)
	defer cancel()
	ctx = WithAttemptBudget(ctx, limits.Attempts)
	started := imp.now().UTC()
	source, err := imp.store.GetSourceByTypeAndIdentifier(ctx, SourceType, strings.TrimSpace(opts.Identifier))
	if err != nil {
		return nil, err
	}
	sum = &ImportSummary{SourceID: source.ID, ScopeCounts: map[string]int{"page_roots": len(normalizedIDs(opts.RootPageIDs)), "data_source_roots": len(normalizedIDs(opts.RootDataSourceIDs)), "meeting_roots": len(normalizedIDs(opts.MeetingBlockIDs))}}
	if searchEnabled(opts) {
		sum.ScopeCounts["accessible_search"] = 1
	}
	state, err := imp.loadState(ctx, source.ID)
	if err != nil {
		return sum, err
	}
	// Rotate legacy overflow into bounded active slots after retaining deadlines.
	state.fitPendingBacklog(started, limits)
	lastValidCursor, err := state.marshal()
	if err != nil {
		return sum, err
	}
	syncID, err := imp.store.StartSyncContext(ctx, source.ID, SourceType)
	if err != nil {
		return sum, err
	}
	scoped := imp.store.ScopedToSync(source.ID, syncID)
	checkpoint := func() (*store.Checkpoint, error) {
		cursor, err := state.marshal()
		if err != nil {
			return nil, err
		}
		return &store.Checkpoint{PageToken: cursor, MessagesProcessed: sum.MeetingsProcessed, MessagesAdded: sum.MeetingsAdded, MessagesUpdated: sum.MeetingsUpdated, ErrorsCount: sum.Errors}, nil
	}
	save := func() error {
		cp, err := checkpoint()
		if err != nil {
			return err
		}
		if err := scoped.UpdateSyncCheckpointContext(ctx, syncID, cp); err != nil {
			return err
		}
		lastValidCursor = cp.PageToken
		return nil
	}
	defer func() {
		state.currentCoverage(searchEnabled(opts))
		sum.Duration = imp.now().UTC().Sub(started)
		sum.WorkRemaining = state.workRemaining()
		sum.CoverageReasons = state.Coverage.Reasons
		sum.PartialCoverage = sum.WorkRemaining > 0 || len(state.Coverage.Reasons) > 0
		if retErr != nil {
			state.reason(errorCategory(retErr), "run")
			cp, cpErr := checkpoint()
			if cpErr != nil {
				retErr = errors.Join(retErr, cpErr)
				cp = &store.Checkpoint{PageToken: lastValidCursor, MessagesProcessed: sum.MeetingsProcessed, MessagesAdded: sum.MeetingsAdded, MessagesUpdated: sum.MeetingsUpdated, ErrorsCount: sum.Errors}
			}
			// A complete provider cursor must survive cancellation and failed writes.
			retErr = errors.Join(retErr, scoped.InterruptSyncWithCheckpoint(ctx, syncID, retErr, cp))
		}
	}()
	api, ok := imp.client.(discoverySource)
	if !ok {
		return sum, errors.New("notion source does not support verified identity and broad discovery")
	}
	self, err := api.RetrieveSelf(ctx)
	if err != nil {
		sum.Errors++
		return sum, hydrationFailure("verify Notion credentials", err)
	}
	identity, err := ResolveIdentity(self, opts.TokenType, opts.UserID)
	if err != nil {
		sum.Errors++
		return sum, err
	}
	sum.IdentityProvenance = identity.Provenance
	fingerprint := scopeFingerprint(opts.Identifier, identity, opts)
	if fingerprint != state.Fingerprint {
		// Keep archive IDs, checksums and retry deadlines, but no continuation from
		// another selection/identity scope can certify this one.
		state.Admissions = nil
		state.Frontier = nil
		state.Windows = nil
		state.Deferred = nil
		state.Seen = map[string]bool{}
		state.Active = false
		state.SeedNext = 0
		state.Coverage = Coverage{Reasons: map[string]int{}}
	}
	state.Coverage.Reasons = map[string]int{}
	state.Identity = identity
	state.Fingerprint = fingerprint
	if opts.Full && state.Active {
		state.FreshRequested = true
	}
	seeds := configuredSeeds(opts)
	if state.SeedNext > len(seeds) {
		return sum, errors.New("invalid saved root position")
	}
	seedCandidate := func() (syncState, bool) {
		candidate := state.copyDiscovery()
		if candidate.SeedNext == 0 || candidate.SeedNext > len(seeds) {
			return candidate, false
		}
		task := seeds[candidate.SeedNext-1]
		if !candidate.Seen[task.key()] {
			candidate.Seen[task.key()] = true
			candidate.Frontier = append(candidate.Frontier, task)
		}
		candidate.SeedNext++
		if candidate.SeedNext > len(seeds) {
			candidate.SeedNext = 0
		}
		return candidate, candidate.validateTransientWith(limits) == nil
	}
	if state.Active {
		retry := len(state.Deferred) >= limits.Scopes
		if state.SeedNext > 0 {
			// Give a root that fits a turn before activating another due retry.
			_, fits := seedCandidate()
			retry = len(state.Frontier) == 0 && !fits
		}
		if retry {
			before := state.copyDiscovery()
			state.activateOneDeferred(started, limits)
			if state.validateTransientWith(limits) != nil {
				state = before
			}
		}
	}
	beginSweep := func() {
		before := state.copyDiscovery()
		state.startSweep(opts, started)
		state.activateDeferred(started, limits)
		if state.validateTransientWith(limits) == nil {
			return
		}
		// Retain the configured roots by position in the fingerprinted options,
		// instead of putting all of them in a nearly full checkpoint at once.
		state = before.copyDiscovery()
		compact := opts
		compact.RootPageIDs, compact.RootDataSourceIDs, compact.MeetingBlockIDs = nil, nil, nil
		noSearch := false
		compact.SearchAccessible = &noSearch
		state.startSweep(compact, started)
		if len(seeds) > 0 {
			state.SeedNext = 1
		}
		if state.validateTransientWith(limits) != nil {
			state.Coverage.Diagnostics = nil
		}
		if state.validateTransientWith(limits) != nil {
			// Even the compact continuation cannot fit. Keep the inactive
			// checkpoint, but let one due retry drain if it can be represented.
			state = before.copyDiscovery()
			state.activateOneDeferred(started, limits)
			state.Active = len(state.Frontier) > 0 || len(state.Windows) > 0
			if state.validateTransientWith(limits) != nil {
				state = before
			}
			return
		}
		if _, fits := seedCandidate(); !fits {
			compactState := state.copyDiscovery()
			state.activateOneDeferred(started, limits)
			if state.validateTransientWith(limits) != nil {
				state = compactState
			}
		}
	}
	sweepsStarted := 0
	if !state.Active {
		beginSweep()
		sweepsStarted++
	}
	state.Coverage.Limited = opts.Limit > 0
	if err := save(); err != nil {
		return sum, err
	}
	if strings.TrimSpace(opts.AccountEmail) != "" {
		if err := imp.store.AddAccountIdentityContext(ctx, source.ID, opts.AccountEmail, "account-email"); err != nil {
			return sum, fmt.Errorf("confirm Notion account identity: %w", err)
		}
	}
	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}
	run := &discoveryRun{state: &state, client: api, source: imp.client, limits: limits, now: started}
	due := state.duePendingIDs(started, limits.KnownRefresh)
	dueSet := make(map[string]bool, len(due))
	for _, id := range due {
		dueSet[id] = true
	}
	refresh := state.refreshIDs(limits.KnownRefresh, dueSet)
	processed := map[string]bool{}
	blockedAdmissions := map[string]bool{}
	admitted := 0
	hydrator := NewHydrator(imp.client).WithUserSource(imp.users)
	admissionStep := func(ctx context.Context) (bool, error) {
		index := -1
		for i, id := range state.Admissions {
			if blockedAdmissions[id] {
				continue
			}
			_, known := state.Known[id]
			if processed[id] || known || opts.Limit <= 0 || admitted < opts.Limit {
				index = i
				break
			}
		}
		if index < 0 {
			return false, nil
		}
		id := state.Admissions[index]
		beforeAdmission := state.copyDiscovery()
		if !processed[id] {
			_, known := state.Known[id]
			selected, err := imp.processMeeting(ctx, scoped, &state, sum, opts, id, progress, hydrator)
			if err != nil {
				if errors.Is(err, ErrRequestBudget) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					// Retain interrupted work behind independent admissions. Retirement
					// still requires a successful archive publication below.
					state.Admissions = append(append(state.Admissions[:index], state.Admissions[index+1:]...), id)
				}
				return false, err
			}
			if selected && !known {
				admitted++
			}
			processed[id] = true
		}
		state.Admissions = append(state.Admissions[:index], state.Admissions[index+1:]...)
		if len(state.Deferred) > len(beforeAdmission.Deferred) && state.validateTransientWith(limits) != nil {
			// A failed new meeting can add a deferred entry to a full cursor.
			// Keep the admission itself as replayable debt if it cannot fit.
			state = beforeAdmission
			state.Admissions = append(append(state.Admissions[:index], state.Admissions[index+1:]...), id)
			blockedAdmissions[id] = true
			delete(processed, id)
		}
		return true, nil
	}
	refreshStep := func(ctx context.Context) (bool, error) {
		for len(due) > 0 || len(refresh) > 0 {
			isDue := len(due) > 0 && (len(refresh) == 0 || !state.RefreshKnownNext)
			if len(due) > 0 && len(refresh) > 0 {
				state.RefreshKnownNext = isDue
			}
			var id string
			if isDue {
				id, due = due[0], due[1:]
			} else {
				id, refresh = refresh[0], refresh[1:]
			}
			if processed[id] {
				if !isDue {
					state.RefreshCursor = id
				}
				continue
			}
			waiting := false
			for _, queue := range [][]pendingTranscript{state.Pending, state.PendingBacklog} {
				for _, pending := range queue {
					if pending.BlockID == id && !pendingDue(pending, started) && !opts.Full && !run.visible[id] {
						waiting = true
						break
					}
				}
				if waiting {
					break
				}
			}
			if waiting {
				state.RefreshCursor = id
				continue
			}
			_, err := imp.processMeeting(ctx, scoped, &state, sum, opts, id, progress, hydrator)
			if err != nil {
				if errors.Is(err, ErrRequestBudget) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					if isDue {
						// Retain the deadline while giving other due retries a turn.
						state.deferTranscriptAttempt(id, started)
					} else {
						// Rotation records the attempt, not successful freshness.
						state.RefreshCursor = id
					}
				}
				return false, err
			}
			if !isDue {
				state.RefreshCursor = id
			}
			processed[id] = true
			sum.MaintenanceRetries++
			return true, nil
		}
		return false, nil
	}
	traversalStep := func(ctx context.Context) (bool, error) {
		if state.SeedNext > 0 {
			candidate, fits := seedCandidate()
			if fits {
				state = candidate
				return true, nil
			}
		}
		return run.stepTraversal(ctx)
	}
	steps := []func(context.Context) (bool, error){run.stepQuery, traversalStep, admissionStep, refreshStep}
	idle := 0
	for idle < len(steps) {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		lane := state.NextLane
		state.NextLane = (lane + 1) % len(steps)
		before := state.copyDiscovery()
		changed, stepErr := steps[lane](ctx)
		if lane >= 2 && changed && len(state.Pending) > 0 && state.validateTransientWith(limits) != nil {
			state.fitPendingBacklog(started, limits)
		}
		if lane < 2 && changed {
			if boundErr := state.validateTransientWith(limits); boundErr != nil {
				state = before.copyDiscovery()
				// Retain the predecessor page as deferred work instead of accepting an
				// unrepresentable continuation or blocking independent branches forever.
				if lane == 0 && len(state.Windows) > 0 {
					window := state.Windows[0]
					state.Windows = state.Windows[1:]
					state.deferWindow(window, "cursor_budget", started)
				}
				if lane == 1 && len(state.Frontier) > 0 {
					task := state.Frontier[0]
					state.Frontier = state.Frontier[1:]
					state.deferTask(task, "cursor_budget", started)
				}
				if state.validateTransient() != nil {
					state = before.copyDiscovery()
					var task *scopeTask
					var window *queryWindow
					if lane == 0 && len(state.Windows) > 0 {
						item := state.Windows[0]
						window = &item
						state.Windows = state.Windows[1:]
					}
					if lane == 1 && len(state.Frontier) > 0 {
						item := state.Frontier[0]
						task = &item
						state.Frontier = state.Frontier[1:]
					}
					if state.coalescePredecessor(task, window, started, limits) {
						goto complete
					}
					state = before.copyDiscovery()
					// Keep the predecessor replayable, but let independent work lead
					// the next run when no deferred slot is available.
					if lane == 0 && len(state.Windows) > 1 {
						state.Windows = append(state.Windows[1:], state.Windows[0])
					}
					if lane == 1 && len(state.Frontier) > 1 {
						state.Frontier = append(state.Frontier[1:], state.Frontier[0])
					}
					state.reason("cursor_budget", "run")
					if state.validateTransient() != nil {
						// Historical diagnostics are bounded and can be compacted to
						// retain replayable work within the cursor byte limit.
						state.Coverage.Diagnostics = nil
						if state.validateTransient() != nil {
							state = before.copyDiscovery()
							if lane == 0 && len(state.Windows) > 1 {
								state.Windows = append(state.Windows[1:], state.Windows[0])
							}
							if lane == 1 && len(state.Frontier) > 1 {
								state.Frontier = append(state.Frontier[1:], state.Frontier[0])
							}
						}
					}
				}
				goto complete
			}
		}
		if errors.Is(stepErr, ErrRequestBudget) {
			state.reason("request_budget", "run")
			goto complete
		}
		if stepErr != nil {
			if lane < 2 {
				sum.Errors++
			}
			return sum, hydrationFailure("advance Notion import", stepErr)
		}
		if changed {
			idle = 0
			if err := save(); err != nil {
				return sum, err
			}
			if lane < 2 {
				progress("Notion discovery checkpoint saved")
			}
		} else {
			idle++
		}
		if len(state.Frontier) == 0 && len(state.Windows) == 0 && state.SeedNext == 0 {
			state.Active = false
			if sweepsStarted == 0 && state.FreshRequested && run.requests < limits.Requests {
				beginSweep()
				sweepsStarted++
				idle = 0
			}
		}
	}

complete:
	if run.requests >= limits.Requests && (len(state.Frontier) > 0 || len(state.Windows) > 0) {
		state.reason("discovery_budget", "run")
	}
	if len(state.Admissions) > 0 {
		state.reason("unfinished_progress", "pending")
	}
	if len(state.Frontier) == 0 && len(state.Windows) == 0 && state.SeedNext == 0 {
		state.Active = false
	}
	state.currentCoverage(searchEnabled(opts))
	if err := save(); err != nil {
		return sum, err
	}
	cursor, err := state.marshal()
	if err != nil {
		return sum, err
	}
	if err := scoped.CompleteSyncContext(ctx, syncID, cursor); err != nil {
		return sum, err
	}
	return sum, nil
}

func (imp *Importer) loadState(ctx context.Context, sourceID int64) (syncState, error) {
	state := newSyncState()
	previous, err := imp.store.GetLatestCheckpointedSyncByType(ctx, sourceID, SourceType)
	cursor := ""
	if err == nil {
		cursor = previous.CursorBefore.String
	} else if !errors.Is(err, store.ErrSyncRunNotFound) {
		return state, fmt.Errorf("restore sync progress: %w", err)
	}
	if cursor == "" {
		previous, err = imp.store.GetLastSuccessfulSyncByType(ctx, sourceID, SourceType)
		if err == nil {
			cursor = previous.CursorAfter.String
		} else if !errors.Is(err, store.ErrSyncRunNotFound) {
			return state, fmt.Errorf("load previous Notion cursor: %w", err)
		}
	}
	legacy := cursor == ""
	if cursor != "" {
		if err := json.Unmarshal([]byte(cursor), &state); err != nil {
			return state, fmt.Errorf("decode previous Notion sync cursor: %w", err)
		}
		if err := state.validate(); err != nil {
			return state, fmt.Errorf("validate previous Notion sync cursor: %w", err)
		}
		legacy = state.Version == 1
	}
	state.initialize()
	if legacy {
		archived, err := imp.store.SourceMessageMetadata(ctx, sourceID)
		if err != nil {
			return state, err
		}
		for id := range archived {
			if _, ok := state.Known[id]; !ok {
				state.Known[id] = knownMeeting{}
			}
		}
	}
	return state, nil
}

func (state *syncState) refreshIDs(limit int, excluded map[string]bool) []string {
	s := *state
	ids := make([]string, 0, len(s.Known))
	for id := range s.Known {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	start := sort.SearchStrings(ids, s.RefreshCursor)
	for start < len(ids) && ids[start] <= s.RefreshCursor {
		start++
	}
	rotated := append(append([]string{}, ids[start:]...), ids[:start]...)
	selected := make([]string, 0, min(len(rotated), limit))
	for _, id := range rotated {
		if !excluded[id] && len(selected) < limit {
			selected = append(selected, id)
		}
	}
	return selected
}

// Due retries have their own bounded selection, independent of archive size.
// Oldest attempts go first; overall request and time budgets still apply.
func (state *syncState) duePendingIDs(now time.Time, limit int) []string {
	s := *state
	pending := append([]pendingTranscript(nil), s.Pending...)
	sort.Slice(pending, func(i, j int) bool {
		left, _ := time.Parse(time.RFC3339, pending[i].NextAttemptAt)
		right, _ := time.Parse(time.RFC3339, pending[j].NextAttemptAt)
		if left.Equal(right) {
			return pending[i].BlockID < pending[j].BlockID
		}
		return left.Before(right)
	})
	ids := make([]string, 0, min(len(pending), limit))
	for _, item := range pending {
		if len(ids) == limit {
			break
		}
		if pendingDue(item, now) {
			ids = append(ids, item.BlockID)
		}
	}
	return ids
}

type archivedMeetingState struct {
	Evidence      rawEvidence
	HasEvidence   bool
	Body          string
	MessageID     int64
	ResolvedUsers []resolvedUser
}

func (imp *Importer) archivedState(ctx context.Context, sourceID int64, id string) (archivedMeetingState, error) {
	existing, err := imp.store.MessageExistsBatchContext(ctx, sourceID, []string{id})
	if err != nil {
		return archivedMeetingState{}, err
	}
	messageID, ok := existing[id]
	if !ok {
		return archivedMeetingState{}, nil
	}
	body, err := imp.store.GetMessageBodyText(ctx, messageID)
	if err != nil {
		return archivedMeetingState{}, err
	}
	archived := archivedMeetingState{Body: body, MessageID: messageID}
	metadata, err := imp.store.GetMessageMetadataContext(ctx, messageID)
	if err != nil {
		return archivedMeetingState{}, err
	}
	if metadata.Valid {
		var decoded meetingMetadata
		if json.Unmarshal([]byte(metadata.String), &decoded) == nil {
			archived.ResolvedUsers = decoded.ResolvedUsers
		}
	}
	raw, err := imp.store.GetMessageRawContext(ctx, messageID)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrInvalidMessageRaw) {
		return archived, nil
	}
	if err != nil {
		return archivedMeetingState{}, err
	}
	evidence, ok := decodeArchivedEvidence(raw, id)
	archived.Evidence = evidence
	archived.HasEvidence = ok
	if ok {
		archived.ResolvedUsers = evidence.ResolvedUsers
	}
	return archived, nil
}

func decodeArchivedEvidence(raw []byte, expectedID string) (rawEvidence, bool) {
	var evidence rawEvidence
	if json.Unmarshal(raw, &evidence) != nil {
		return rawEvidence{}, false
	}
	if evidence.SchemaVersion != 1 && evidence.SchemaVersion != notionSnapshotVersion {
		return rawEvidence{}, false
	}
	var discovery MeetingNote
	if json.Unmarshal(evidence.Discovery, &discovery) != nil ||
		discovery.ID != expectedID || discovery.Type != meetingNotesType {
		return rawEvidence{}, false
	}
	var meetingBlock Block
	if json.Unmarshal(evidence.MeetingBlock, &meetingBlock) != nil ||
		meetingBlock.ID != expectedID || meetingBlock.Type != meetingNotesType {
		return rawEvidence{}, false
	}
	if evidence.SchemaVersion == 1 {
		var markdown MarkdownPage
		if json.Unmarshal(evidence.PageMarkdown, &markdown) != nil || markdown.Object != "page_markdown" || strings.TrimSpace(markdown.ID) == "" {
			return rawEvidence{}, false
		}
	} else {
		content := meetingcontent.Decode(RawFormat, raw, nil)
		// Only the reconstructed admitted blocks can prove canonical text. An
		// invalid section or conflicting header must never be promoted as scoped.
		for _, reason := range []string{content.Summary.Reason, content.Notes.Reason, content.Transcript.Reason} {
			if reason != "" && reason != "incomplete_tree" {
				return rawEvidence{}, false
			}
		}
		if content.Summary.Text != evidence.Canonical.Summary || content.Notes.Text != evidence.Canonical.Notes || content.Transcript.Text != evidence.Canonical.Transcript {
			return rawEvidence{}, false
		}
		// A fresh tree belongs to the validated current header. Reject conflicting
		// provenance before preservation could bind it to an older source header.
		for _, tree := range []rawBlockTree{evidence.Summary, evidence.Notes, evidence.Transcript} {
			if len(tree.Root) > 0 && !tree.Preserved &&
				(tree.SourceLastEditedTime != discovery.LastEditedTime || len(tree.SourceMeetingBlock) > 0) {
				return rawEvidence{}, false
			}
		}
	}
	return evidence, true
}

// preserveArchivedSections never attributes legacy body or page Markdown to a
// meeting block. If attribution cannot be proved, the caller retains the whole
// old archive and reports the stale scope instead of writing a v2 snapshot.
func preserveArchivedSections(meeting *HydratedMeeting, archived archivedMeetingState) bool {
	if archived.MessageID == 0 {
		return false
	}
	old := archived.Evidence
	oldTexts := canonicalEvidence{
		Summary:    archivedBodySection(archived.Body, "Summary"),
		Notes:      archivedBodySection(archived.Body, "Notes"),
		Transcript: transcriptFromArchivedBody(archived.Body),
	}
	if archived.HasEvidence {
		oldTexts = old.Canonical
	}
	sections := []struct {
		name      string
		id        string
		tree      *blockTree
		text      *string
		prior     rawBlockTree
		priorText string
	}{
		{"summary", meeting.Discovery.MeetingNotes.Children.SummaryBlockID, &meeting.SummaryTree, &meeting.Summary, old.Summary, oldTexts.Summary},
		{"notes", meeting.Discovery.MeetingNotes.Children.NotesBlockID, &meeting.NotesTree, &meeting.Notes, old.Notes, oldTexts.Notes},
		{"transcript", meeting.Discovery.MeetingNotes.Children.TranscriptBlockID, &meeting.TranscriptTree, &meeting.Transcript, old.Transcript, oldTexts.Transcript},
	}
	for _, section := range sections {
		if section.name != "transcript" && section.id == "" && sectionReferencePresent(meeting.Discovery.Raw, section.name) {
			continue
		}
		hasPrior := strings.TrimSpace(section.priorText) != "" || (archived.HasEvidence &&
			((old.SchemaVersion == 2 && len(section.prior.Root) != 0 && len(section.prior.Blocks) != 0) ||
				(old.SchemaVersion == 1 && (len(section.prior.Root) != 0 || len(section.prior.Pages) != 0 || len(section.prior.Blocks) != 0))))
		if !hasPrior || (section.tree.Complete && !section.tree.Filtered && (section.name != "transcript" || strings.TrimSpace(*section.text) != "")) {
			continue
		}
		if !archived.HasEvidence || old.SchemaVersion != 2 {
			return true
		}
		tree, ok := restoreArchivedTree(section.prior)
		if !ok || renderTree(tree) != section.priorText {
			return true
		}
		tree.Preserved = true
		if len(tree.SourceMeetingBlock) == 0 {
			tree.SourceMeetingBlock = old.MeetingBlock
		}
		*section.tree = tree
		*section.text = section.priorText
		meeting.Warnings = append(meeting.Warnings, "provider returned incomplete "+section.name+"; preserved archived meeting-owned "+section.name)
	}
	return false
}

// An omitted field is not evidence that the provider removed a section.
func sectionReferencePresent(raw []byte, section string) bool {
	var block struct {
		MeetingNotes struct {
			Children map[string]any `json:"children"`
		} `json:"meeting_notes"`
	}
	if json.Unmarshal(raw, &block) != nil {
		return false
	}
	_, ok := block.MeetingNotes.Children[section+"_block_id"]
	return ok
}

func restoreArchivedTree(raw rawBlockTree) (blockTree, bool) {
	if len(raw.Root) == 0 || len(raw.Blocks) == 0 || len(raw.Blocks) > maxHydrationBlocks {
		return blockTree{}, false
	}
	var root Block
	if json.Unmarshal(raw.Root, &root) != nil || root.ID == "" || root.Object != "block" || root.Type == meetingNotesType {
		return blockTree{}, false
	}
	tree := blockTree{Root: &root, Complete: raw.Complete, Filtered: raw.Filtered, SourceLastEditedTime: raw.SourceLastEditedTime, SourceMeetingBlock: raw.SourceMeetingBlock}
	seen := map[string]bool{}
	for _, item := range raw.Blocks {
		var block Block
		if json.Unmarshal(item, &block) != nil || block.ID == "" || block.Object != "block" || block.Type == meetingNotesType || seen[block.ID] {
			return blockTree{}, false
		}
		seen[block.ID] = true
		tree.Blocks = append(tree.Blocks, block)
	}
	if tree.Blocks[0].ID != root.ID {
		return blockTree{}, false
	}
	return tree, true
}

func archivedBodySection(body, label string) string {
	_, text, ok := strings.Cut(body, "\n\n"+label+":\n")
	if !ok {
		return ""
	}
	for _, next := range []string{"Summary", "Notes", "Transcript"} {
		if prefix, _, found := strings.Cut(text, "\n\n"+next+":\n"); found {
			text = prefix
		}
	}
	return strings.TrimSpace(text)
}

func transcriptFromArchivedBody(body string) string {
	const marker = "\n\nTranscript:\n"
	_, transcript, ok := strings.Cut(body, marker)
	if !ok {
		return ""
	}
	return strings.TrimSpace(transcript)
}

func preserveArchivedAttendees(meeting *HydratedMeeting, archived []resolvedUser) {
	if meeting == nil || len(archived) == 0 {
		return
	}

	archivedByID := make(map[string]resolvedUser, len(archived))
	for _, user := range archived {
		id := strings.TrimSpace(user.ID)
		email := normalizedUserEmail(user.Email)
		if id == "" || email == "" || !user.EmailVerified {
			continue
		}
		user.ID = id
		user.Name = strings.TrimSpace(user.Name)
		user.Email = email
		archivedByID[id] = user
	}
	if len(archivedByID) == 0 {
		return
	}

	currentByID := make(map[string]resolvedUser, len(meeting.ResolvedUsers))
	for _, user := range meeting.ResolvedUsers {
		currentByID[user.ID] = user
	}
	unresolved := make(map[string]struct{}, len(meeting.UnresolvedAttendeeIDs))
	for _, id := range meeting.UnresolvedAttendeeIDs {
		unresolved[id] = struct{}{}
	}

	attendeeIDs := meeting.Discovery.MeetingNotes.CalendarEvent.Attendees
	labels := make([]string, 0, len(attendeeIDs))
	attendees := make([]meetingarchive.Person, 0, len(attendeeIDs))
	resolved := make([]resolvedUser, 0, len(attendeeIDs))
	seenEmails := make(map[string]struct{}, len(attendeeIDs))
	seenResolvedIDs := make(map[string]struct{}, len(attendeeIDs))
	restored := 0
	for index, id := range attendeeIDs {
		label := id
		if index < len(meeting.AttendeeLabels) && strings.TrimSpace(meeting.AttendeeLabels[index]) != "" {
			label = strings.TrimSpace(meeting.AttendeeLabels[index])
		}

		user, ok := currentByID[id]
		if !ok {
			if _, isUnresolved := unresolved[id]; isUnresolved &&
				(meeting.failedAttendeeIDs == nil || meeting.failedAttendeeIDs[id]) {
				user, ok = archivedByID[id]
				if ok {
					restored++
					delete(unresolved, id)
				}
			}
		}
		if !ok {
			labels = append(labels, label)
			continue
		}

		user.ID = strings.TrimSpace(user.ID)
		user.Name = strings.TrimSpace(user.Name)
		user.Email = normalizedUserEmail(user.Email)
		if user.Name != "" {
			label = user.Name
		}
		labels = append(labels, label)
		if user.Email == "" || !user.EmailVerified {
			continue
		}
		if _, seen := seenResolvedIDs[user.ID]; !seen {
			resolved = append(resolved, user)
			seenResolvedIDs[user.ID] = struct{}{}
		}
		if _, seen := seenEmails[user.Email]; seen {
			continue
		}
		attendees = append(attendees, meetingarchive.Person{
			Name: user.Name, Email: user.Email, Anchor: userAnchor(user.ID),
		})
		seenEmails[user.Email] = struct{}{}
	}
	if restored == 0 {
		return
	}

	remainingUnresolved := make([]string, 0, len(meeting.UnresolvedAttendeeIDs))
	for _, id := range meeting.UnresolvedAttendeeIDs {
		if _, remains := unresolved[id]; remains {
			remainingUnresolved = append(remainingUnresolved, id)
		}
	}
	meeting.AttendeeLabels = labels
	meeting.Attendees = attendees
	meeting.ResolvedUsers = resolved
	meeting.UnresolvedAttendeeIDs = remainingUnresolved
	meeting.Warnings = append(meeting.Warnings, "user directory lookup failed; preserved previously verified attendees from archived evidence")
}

func preserveArchivedAttendeeRelationships(
	meeting *HydratedMeeting,
	archived []store.MessageRecipient,
	archivedUsers []resolvedUser,
) {
	if meeting == nil || len(archived) == 0 {
		return
	}
	currentByID := make(map[string]int, len(meeting.ResolvedUsers))
	emailUsers := make(map[string]map[int]struct{}, len(meeting.ResolvedUsers))
	addEmailUser := func(email string, userIndex int) {
		email = normalizedUserEmail(email)
		if email == "" {
			return
		}
		users := emailUsers[email]
		if users == nil {
			users = make(map[int]struct{})
			emailUsers[email] = users
		}
		users[userIndex] = struct{}{}
	}
	addAlias := func(userIndex int, email string) {
		email = normalizedUserEmail(email)
		user := &meeting.ResolvedUsers[userIndex]
		if email == "" || email == normalizedUserEmail(user.Email) {
			return
		}
		for _, alias := range user.EmailAliases {
			if strings.EqualFold(strings.TrimSpace(alias), email) {
				return
			}
		}
		user.EmailAliases = append(user.EmailAliases, email)
	}
	for index := range meeting.ResolvedUsers {
		user := &meeting.ResolvedUsers[index]
		id := strings.TrimSpace(user.ID)
		email := normalizedUserEmail(user.Email)
		if id == "" || email == "" || !user.EmailVerified {
			continue
		}
		user.ID = id
		user.Email = email
		currentByID[id] = index
		addEmailUser(email, index)
		for _, alias := range user.EmailAliases {
			addEmailUser(alias, index)
		}
	}
	for _, user := range archivedUsers {
		index, current := currentByID[strings.TrimSpace(user.ID)]
		if !current || !user.EmailVerified {
			continue
		}
		for _, email := range append([]string{user.Email}, user.EmailAliases...) {
			addEmailUser(email, index)
			addAlias(index, email)
		}
	}
	if len(emailUsers) == 0 {
		return
	}
	currentParticipants := make(map[int64]map[int]struct{}, len(emailUsers))
	for _, recipient := range archived {
		email := normalizedUserEmail(recipient.EmailAddress)
		users := emailUsers[email]
		if len(users) == 0 {
			continue
		}
		participantUsers := currentParticipants[recipient.ParticipantID]
		if participantUsers == nil {
			participantUsers = make(map[int]struct{})
			currentParticipants[recipient.ParticipantID] = participantUsers
		}
		for index := range users {
			participantUsers[index] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(meeting.Attendees)+len(archived))
	for _, attendee := range meeting.Attendees {
		email := normalizedUserEmail(attendee.Email)
		if email != "" {
			seen[email] = struct{}{}
		}
	}
	restored := 0
	for _, recipient := range archived {
		users := currentParticipants[recipient.ParticipantID]
		if len(users) == 0 {
			continue
		}
		email := normalizedUserEmail(recipient.EmailAddress)
		if email == "" {
			continue
		}
		for index := range users {
			addAlias(index, email)
		}
		if _, ok := seen[email]; ok {
			continue
		}
		meeting.Attendees = append(meeting.Attendees, meetingarchive.Person{
			Name: strings.TrimSpace(recipient.DisplayName), Email: email,
		})
		seen[email] = struct{}{}
		restored++
	}
	if restored > 0 {
		meeting.Warnings = append(meeting.Warnings,
			"user directory lookup failed; preserved previously verified attendees from archived relationships")
	}
}

func candidateTime(meeting MeetingNote) time.Time {
	for _, value := range []string{
		meeting.MeetingNotes.CalendarEvent.StartTime,
		meeting.MeetingNotes.Recording.StartTime,
		meeting.CreatedTime,
	} {
		if parsed := parseNotionTime(value); !parsed.IsZero() {
			return parsed
		}
	}
	return time.Time{}
}

func schedulePending(meeting *HydratedMeeting, now time.Time, previous pendingTranscript, hasPrevious bool) (pendingTranscript, bool) {
	_, endedAt := meeting.times()
	retryUntil := time.Time{}
	if hasPrevious {
		retryUntil, _ = time.Parse(time.RFC3339, previous.RetryUntil)
	}
	if !endedAt.IsZero() {
		deadline := endedAt.Add(transcriptRetryWindow)
		if deadline.After(retryUntil) {
			retryUntil = deadline
		}
	} else if retryUntil.IsZero() {
		retryUntil = now.Add(unknownTranscriptRetrySpan)
	}
	if !retryUntil.After(now) {
		return pendingTranscript{}, false
	}
	next := now.Add(transcriptRetryCadence)
	if next.After(retryUntil) {
		next = retryUntil
	}
	return pendingTranscript{
		BlockID:       meeting.Discovery.ID,
		NextAttemptAt: next.UTC().Format(time.RFC3339),
		RetryUntil:    retryUntil.UTC().Format(time.RFC3339),
	}, true
}

func pendingDue(pending pendingTranscript, now time.Time) bool {
	next, err := time.Parse(time.RFC3339, pending.NextAttemptAt)
	return err != nil || !next.After(now)
}

func pendingExpired(pending pendingTranscript, now time.Time) bool {
	until, err := time.Parse(time.RFC3339, pending.RetryUntil)
	return err == nil && !until.After(now)
}

func terminalStatus(status string) bool {
	normalized := strings.ToLower(strings.TrimSpace(status))
	return strings.Contains(normalized, "fail") || strings.Contains(normalized, "delete")
}

func checksumBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
