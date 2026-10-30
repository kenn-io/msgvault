package threads

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/store"
)

type ImportOptions struct {
	SourceID         int64
	SourceIdentifier string
	Request          SyncRequest
	Budget           *RequestBudget
	// Execution is optional. A caller that refreshed credentials under an
	// existing source execution supplies that lease; the importer never retakes it.
	Execution *store.SyncExecution
	Now       func() time.Time
	Progress  func(string)
}
type ImportSummary struct {
	SyncRunID         int64     `json:"sync_run_id"`
	Status            string    `json:"status"`
	Mode              string    `json:"mode"`
	HistoryComplete   bool      `json:"history_complete"`
	SweepBoundary     int64     `json:"sweep_boundary"`
	SweepPosition     int64     `json:"sweep_position"`
	OldestObserved    time.Time `json:"oldest_observed"`
	RequestedFloor    time.Time `json:"requested_floor"`
	PendingWalks      int       `json:"pending_walks"`
	PendingPages      int       `json:"pending_pages"`
	Unavailable       int64     `json:"unavailable"`
	Incomplete        int64     `json:"incomplete"`
	MessagesAdded     int64     `json:"messages_added"`
	MessagesUpdated   int64     `json:"messages_updated"`
	MessagesProcessed int64     `json:"messages_processed"`
}
type Importer struct {
	store            *store.Store
	api              API
	afterItem        func() error
	beforeCheckpoint func() error
}

func NewImporter(st *store.Store, api API) *Importer { return &Importer{store: st, api: api} }

// Import assumes the caller owns the source's credential lifecycle lock. The
// execution lease fences every archive mutation and is explicitly released here
// when acquired here. No API call occurs inside a database transaction.
func (i *Importer) Import(ctx context.Context, o ImportOptions) (sum *ImportSummary, retErr error) {
	if err := o.Request.Validate(); err != nil {
		return nil, err
	}
	if !o.Request.After.IsZero() && o.Request.After.Before(earliestHistoryTime()) {
		return nil, errors.New("Threads history floor precedes the earliest supported time")
	}
	if _, _, err := ParseSourceIdentifier(o.SourceIdentifier); err != nil {
		return nil, err
	}
	if o.Budget == nil {
		return nil, errors.New("Threads run budget is required")
	}
	ctx, cancel := o.Budget.RunContext(ctx)
	defer cancel()
	now := o.Now
	if now == nil {
		now = o.Budget.now
	}
	source, err := i.store.GetSourceByIDContext(ctx, o.SourceID)
	if err != nil {
		return nil, err
	}
	if source.SourceType != "threads" || source.Identifier != o.SourceIdentifier || source.HistoryOnly || source.MergedIntoSourceID != 0 {
		return nil, errors.New("Threads source binding is not writable")
	}
	execution := o.Execution
	if execution == nil {
		execution, err = i.store.AcquireSyncExecutionContext(ctx, o.SourceID)
		if err != nil {
			return nil, err
		}
		defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	}
	var cfg SourceSyncConfig
	if o.Request.PostID != "" {
		if err = json.Unmarshal([]byte(source.SyncConfig.String), &cfg); err != nil {
			return nil, errors.New("invalid Threads coverage config")
		}
		if err = cfg.validate(); err != nil {
			return nil, err
		}
		// Only production root selection can grant eligibility; an owner-authored
		// child in someone else's conversation is insufficient.
		if _, err = i.store.GetThreadsOwnRootContext(ctx, o.SourceID, o.Request.PostID); err != nil {
			return nil, errors.New("Threads targeted sync requires an archived verified own root")
		}
	} else {
		cfg, err = PrepareSourceSyncConfigContext(ctx, i.store, o.SourceID, o.Request, now())
		if err != nil {
			return nil, err
		}
	}
	query, err := o.Request.ResumeQuery(o.SourceIdentifier, cfg)
	if err != nil {
		return nil, err
	}
	baseline, checkpoint, err := i.store.GetThreadsSyncResumeContext(ctx, o.SourceID, query)
	if err != nil {
		return nil, err
	}
	state, err := ResumeSyncState(o.Request, baseline, checkpoint)
	if err != nil {
		return nil, err
	}
	if state == nil {
		fp, e := o.Request.Fingerprint(o.SourceIdentifier, cfg)
		if e != nil {
			return nil, e
		}
		upper, e := i.store.ThreadsOwnRootUpperKeyContext(ctx, o.SourceID)
		if e != nil {
			return nil, e
		}
		epoch, e := NewOAuthState()
		if e != nil {
			return nil, e
		}
		state, err = NewSyncState(epoch, fp, cfg, now(), upper)
		if err != nil {
			return nil, err
		}
	} else if state.Mode != store.ThreadsSyncPost && (checkpoint == nil || state.CompletedLanes != 63) {
		if checkpoint == nil {
			state.CompletedLanes = 0
		}
		if err = state.OwnPostsHead.PinHead(cfg.HistoryFloor, now()); o.Request.PostID == "" && err != nil {
			return nil, err
		}
		if err = state.OwnRepliesHead.PinHead(cfg.HistoryFloor, now()); o.Request.PostID == "" && err != nil {
			return nil, err
		}
		if state.Mode != store.ThreadsSyncPost {
			upper, e := i.store.ThreadsOwnRootUpperKeyContext(ctx, o.SourceID)
			if e != nil {
				return nil, e
			}
			if state.HotRoots.Done {
				state.HotRoots = RootPass{ThroughID: upper, HotSince: now().UTC().Add(-7 * 24 * time.Hour)}
			}
			if state.AllRoots.Done {
				state.AllRoots = RootPass{ThroughID: upper}
			}
		}
	} else if state.Mode == store.ThreadsSyncPost && baseline != nil && checkpoint == nil {
		state.TargetRoot.Done = false
		state.TargetRoot.Cursor = ""
		state.TargetRoot.Guard = WalkGuard{}
	}
	if state.Mode != store.ThreadsSyncPost {
		state.markCompleted()
	}
	sum = &ImportSummary{RequestedFloor: cfg.HistoryFloor, Status: store.SyncStatusRunning, Mode: state.Mode}
	runID, err := execution.StartSyncWithRequestContext(ctx, state.Mode, "", state.Fingerprint)
	if err != nil {
		return sum, err
	}
	sum.SyncRunID = runID
	scoped := i.store.ScopedToSync(o.SourceID, runID)
	writer := NewWriter(scoped, o.SourceID, o.SourceIdentifier)
	snapshot, err := state.Encode()
	if err != nil {
		return sum, err
	}
	cp := &store.Checkpoint{PageToken: snapshot}
	finished := false
	defer func() {
		if !finished {
			cause := retErr
			if cause == nil {
				cause = errors.New("Threads sync interrupted")
			}
			sum.Status = store.SyncStatusFailed
			if jobctx.YieldedToWaiter(ctx) && jobctx.ErrorAfterYield(ctx, cause) == nil && cp.ErrorsCount == 0 {
				sum.Status = store.SyncStatusCancelled
			}
			// Store detaches a bounded cleanup internally, retaining the original
			// cancellation cause for scheduler-handoff classification.
			if workloadPause(ctx) != nil && onlyCancellation(cause) {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				sum.Status = store.SyncStatusPaused
				retErr = scoped.PauseSyncWithCheckpointContext(cleanup, runID, cp)
			} else {
				retErr = errors.Join(retErr, scoped.InterruptSyncWithCheckpoint(ctx, runID, cause, cp))
			}
		}
		committed, e := DecodeSyncState(cp.PageToken)
		if e == nil {
			sum.fill(committed)
		}
	}()
	if err = scoped.UpdateSyncCheckpointContext(ctx, runID, cp); err != nil {
		return sum, err
	}
	// Replay may have committed an item before its page checkpoint. Repair its
	// deferred edges before reading the first resumed page.
	if err = writer.ResolveParents(ctx); err != nil {
		return sum, err
	}
	for !state.done() {
		lane := state.NextLane
		next, err := DecodeSyncState(snapshot)
		if err != nil {
			return sum, err
		}
		next.NextLane = (lane + 1) % 6
		processed, added, updated, e := i.step(ctx, scoped, writer, next, lane, o, cfg)
		if e != nil && !isPause(e) {
			sum.Status = store.SyncStatusFailed
			return sum, e
		}
		if e != nil && processed < 0 {
			sum.Status = store.SyncStatusPaused
			if err = scoped.PauseSyncWithCheckpointContext(ctx, runID, cp); err != nil {
				return sum, err
			}
			finished = true
			return sum, nil
		}
		if err = writer.ResolveParents(ctx); err != nil {
			return sum, err
		}
		next.markCompleted()
		encoded, err := next.Encode()
		if err != nil {
			return sum, err
		}
		nextCP := &store.Checkpoint{PageToken: encoded, MessagesProcessed: cp.MessagesProcessed + processed, MessagesAdded: cp.MessagesAdded + added, MessagesUpdated: cp.MessagesUpdated + updated, ErrorsCount: next.Coverage.Incomplete + next.Coverage.Unavailable}
		if i.beforeCheckpoint != nil {
			if err = i.beforeCheckpoint(); err != nil {
				return sum, err
			}
		}
		if err = scoped.UpdateSyncCheckpointContext(ctx, runID, nextCP); err != nil {
			return sum, err
		}
		state = next
		snapshot = encoded
		cp = nextCP
		sum.MessagesProcessed = cp.MessagesProcessed
		sum.MessagesAdded = cp.MessagesAdded
		sum.MessagesUpdated = cp.MessagesUpdated
		if o.Progress != nil {
			o.Progress(fmt.Sprintf("Archived %d Threads snapshots", sum.MessagesProcessed))
		}
		if e != nil {
			sum.Status = store.SyncStatusPaused
			if err = scoped.PauseSyncWithCheckpointContext(ctx, runID, cp); err != nil {
				return sum, err
			}
			finished = true
			return sum, nil
		}
	}
	if state.Mode == store.ThreadsSyncFull {
		state.Mode = store.ThreadsSyncBaseline
		state.Fingerprint = query.BaselineFingerprint
	}
	encoded, err := state.Encode()
	if err != nil {
		return sum, err
	}
	if o.Request.PostID != "" {
		err = scoped.CompleteSyncAndPreserveSourceCursorContext(ctx, runID, o.SourceID, encoded)
	} else {
		err = scoped.CompleteSyncAndUpdateSourceCursorContext(ctx, runID, o.SourceID, encoded)
	}
	if err != nil {
		return sum, err
	}
	finished = true
	sum.Status = store.SyncStatusCompleted
	sum.fill(state)
	return sum, nil
}

// Once every lane has completed a pass in this epoch, drain any newer
// intervals already opened before publishing. Reopening completed lanes while
// a long walk remains lets heads and old roots keep polling without making a
// one-attempt resume oscillate forever between newly reopened empty walks.
func (s *SyncState) markCompleted() {
	for lane, done := range []bool{s.OwnPostsHead.Done, s.OwnRepliesHead.Done, s.OwnPostsHistory.Done, s.OwnRepliesHistory.Done, s.HotRoots.Done, s.AllRoots.Done} {
		if done {
			s.CompletedLanes |= 1 << lane
		}
	}
}
func (s *SyncState) done() bool {
	if s.Mode == store.ThreadsSyncPost {
		return s.TargetRoot.Done
	}
	return s.OwnPostsHead.Done && s.OwnRepliesHead.Done && s.OwnPostsHistory.Done && s.OwnRepliesHistory.Done && s.HotRoots.Done && s.AllRoots.Done
}
func (s *ImportSummary) fill(state *SyncState) {
	s.HistoryComplete = state.Mode != store.ThreadsSyncPost && state.OwnPostsHistory.Done && state.OwnRepliesHistory.Done
	s.SweepBoundary = state.AllRoots.ThroughID
	s.SweepPosition = state.AllRoots.AfterID
	s.OldestObserved = state.Coverage.OldestObserved
	s.Unavailable = state.Coverage.Unavailable
	s.Incomplete = state.Coverage.Incomplete
	s.PendingWalks = 0
	if state.Mode == store.ThreadsSyncPost {
		if !state.TargetRoot.Done {
			s.PendingWalks++
		}
	} else {
		for _, done := range []bool{state.OwnPostsHead.Done, state.OwnRepliesHead.Done, state.OwnPostsHistory.Done, state.OwnRepliesHistory.Done, state.HotRoots.Done, state.AllRoots.Done} {
			if !done {
				s.PendingWalks++
			}
		}
	}
	// Exact remote page counts are unknowable. This is a lower bound: each
	// unfinished walk needs at least one more page attempt.
	s.PendingPages = s.PendingWalks
}

func (i *Importer) step(ctx context.Context, st *store.Store, w *Writer, s *SyncState, lane int, o ImportOptions, cfg SourceSyncConfig) (processed, added, updated int64, retErr error) {
	var feed *FeedWalk
	var pass *RootPass
	if s.Mode == store.ThreadsSyncPost {
		pass = &s.TargetRoot
	} else {
		switch lane {
		case 0:
			feed = &s.OwnPostsHead
		case 1:
			feed = &s.OwnRepliesHead
		case 2:
			feed = &s.OwnPostsHistory
		case 3:
			feed = &s.OwnRepliesHistory
		case 4:
			pass = &s.HotRoots
		case 5:
			pass = &s.AllRoots
		}
	}
	var page Page
	var err error
	var guard *WalkGuard
	var before string
	origin := ItemOrigin{}
	if feed != nil {
		if feed.Done {
			return 0, 0, 0, nil
		}
		if feed.Since.Unix() >= feed.Until.Unix() {
			feed.Done = true
			return 0, 0, 0, nil
		}
		before = feed.Cursor
		guard = &feed.Guard
		q := ListQuery{Since: feed.Since.Truncate(time.Second), Until: feed.Until, After: before}
		if lane == 0 || lane == 2 {
			origin.Kind = "own_posts"
			page, err = i.api.OwnPosts(ctx, q)
		} else {
			origin.Kind = "own_replies"
			page, err = i.api.OwnReplies(ctx, q)
		}
	} else {
		if pass.Done {
			return 0, 0, 0, nil
		}
		if pass.ActiveRoot == "" {
			roots, e := st.ListThreadsOwnRootsContext(ctx, o.SourceID, pass.AfterID, pass.ThroughID, pass.HotSince, 1)
			if e != nil {
				return 0, 0, 0, e
			}
			if len(roots) == 0 {
				pass.Done = true
				return 0, 0, 0, nil
			}
			pass.ActiveRoot = roots[0].MediaID
			pass.ActiveKey = roots[0].MessageID
		}
		origin = ItemOrigin{Kind: "conversation", RootID: pass.ActiveRoot}
		before = pass.Cursor
		guard = &pass.Guard
		page, err = i.api.Conversation(ctx, pass.ActiveRoot, before)
	}
	if err != nil && !isPause(err) {
		var api *APIError
		if errors.As(err, &api) && before != "" && api.CursorExpired {
			if e := guard.restart(); e != nil {
				s.Coverage.Incomplete++
				return 0, 0, 0, &PauseError{Reason: "cursor_gap"}
			}
			if feed != nil {
				feed.Cursor = ""
			} else {
				pass.Cursor = ""
			}
			return 0, 0, 0, nil
		}
		if pass != nil && errors.As(err, &api) && (api.StatusCode == 404 || api.StatusCode == 410 || (api.Code == 100 && api.Subcode == 33)) {
			s.Coverage.Unavailable++
			finishRoot(pass, s.Mode == store.ThreadsSyncPost)
			return 0, 0, 0, nil
		}
		return 0, 0, 0, err
	}
	if isPause(err) && page.Items == nil {
		return -1, 0, 0, err
	}
	guardBefore := *guard
	if e := guard.observe(before, page); e != nil {
		return 0, 0, 0, e
	}
	incomplete := false
	for _, m := range page.Items {
		if feed != nil && (m.Timestamp.Before(cfg.HistoryFloor) || m.Timestamp.Before(feed.Since) || m.Timestamp.After(feed.Until)) {
			continue
		}
		if origin.Kind == "own_replies" && m.RootPost == nil {
			s.Coverage.Incomplete++
			incomplete = true
			continue
		}
		_, exists, e := st.ThreadsArchiveMessageMetadataContext(ctx, o.SourceID, m.ID)
		if e != nil {
			return 0, 0, 0, e
		}
		id, e := w.Apply(ctx, m, origin)
		if e != nil {
			return 0, 0, 0, e
		}
		if id == 0 {
			continue
		}
		if i.afterItem != nil {
			if e = i.afterItem(); e != nil {
				return 0, 0, 0, e
			}
		}
		processed++
		if exists {
			updated++
		} else {
			added++
		}
		if s.Coverage.OldestObserved.IsZero() || m.Timestamp.Before(s.Coverage.OldestObserved) {
			s.Coverage.OldestObserved = m.Timestamp
		}
	}
	if incomplete {
		*guard = guardBefore
		return processed, added, updated, &PauseError{Reason: "incomplete_reply_root"}
	}
	if feed != nil {
		feed.Cursor = page.After
		if len(page.Items) == 0 {
			feed.Done = true
			feed.CompletedThrough = feed.Until
		}
	} else {
		pass.Cursor = page.After
		if len(page.Items) == 0 {
			finishRoot(pass, s.Mode == store.ThreadsSyncPost)
		}
	}
	return processed, added, updated, err
}
func finishRoot(pass *RootPass, targeted bool) {
	if targeted {
		pass.Done = true
		pass.Cursor = ""
		return
	}
	pass.AfterID = pass.ActiveKey
	pass.ActiveKey = 0
	pass.ActiveRoot = ""
	pass.Cursor = ""
	pass.Guard = WalkGuard{}
}
