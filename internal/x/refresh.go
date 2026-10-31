package x

import (
	"context"
	"errors"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

var ErrContentCleanupRequired = errors.New("X content cleanup is required before refresh can complete")

type RefreshOptions struct {
	SourceID  int64
	OwnerID   string
	BatchSize int
}

type RefreshSummary struct {
	Complete          bool      `json:"complete"`
	Overdue           bool      `json:"overdue"`
	Outstanding       int64     `json:"outstanding"`
	PendingRemoval    bool      `json:"pending_removal"`
	LastCompleteAt    time.Time `json:"last_complete_at,omitzero"`
	OldestUncheckedAt time.Time `json:"oldest_unchecked_at,omitzero"`
}

// SetContentCleanup installs the daemon's real derived-content cleanup before
// Import or Refresh. The callback receives work already queued by its scoped
// Store and must not acquire another source execution while this one is held.
func (imp *Importer) SetContentCleanup(fn func(context.Context, store.XRemovalWork) error) {
	imp.cleanup = fn
}

// Refresh owns one source execution for a resumable latest-version sweep.
// Authoritative pending work and timestamps survive interruption; no body is
// scanned to select work and no database transaction spans provider or file I/O.
func (imp *Importer) Refresh(ctx context.Context, opts RefreshOptions) (summary RefreshSummary, retErr error) {
	if imp == nil || imp.store == nil || opts.SourceID <= 0 || ValidateID(opts.OwnerID) != nil {
		return summary, errors.New("X refresh requires an existing numeric account")
	}
	if err := imp.policy.Validate(opts.OwnerID); err != nil {
		return summary, err
	}
	if opts.BatchSize == 0 {
		opts.BatchSize = imp.policy.RefreshBatchSize
	}
	if opts.BatchSize < 1 || opts.BatchSize > 100 {
		return summary, errors.New("X refresh batch must contain 1 through 100 IDs")
	}
	source, err := imp.store.GetSourceByIDContext(ctx, opts.SourceID)
	if err != nil {
		return summary, err
	}
	if source.SourceType != "x" || source.Identifier != opts.OwnerID || source.HistoryOnly || source.MergedIntoSourceID != 0 {
		return summary, errors.New("X refresh source does not match its active numeric owner")
	}
	execution, err := imp.store.AcquireSyncExecutionContext(ctx, source.ID)
	if err != nil {
		return summary, err
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	run, err := execution.StartSyncContext(ctx, "x", "")
	if err != nil {
		return summary, err
	}
	scoped := *imp
	scoped.store = imp.store.ScopedToSync(source.ID, run)
	imp = &scoped
	defer func() {
		final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if retErr != nil {
			retErr = errors.Join(retErr, imp.saveRefreshCoverage(final, opts, &summary, false))
			retErr = errors.Join(retErr, imp.store.InterruptSyncWithCheckpoint(final, run, retErr, nil))
		} else {
			retErr = imp.store.CompleteSyncContext(final, run, "")
		}
	}()
	return imp.refreshScoped(ctx, opts, nil)
}

func (imp *Importer) refreshScoped(ctx context.Context, opts RefreshOptions, api API) (summary RefreshSummary, retErr error) {
	state, err := imp.store.LoadXControlContext(ctx, opts.SourceID)
	if err != nil {
		return summary, err
	}
	if state.Refresh.SweepStartedAt.IsZero() {
		refresh := state.Refresh
		refresh.SweepStartedAt = imp.now().UTC()
		refresh.Cursor = ""
		_, err = imp.store.UpdateXControlContext(ctx, opts.SourceID, state.Revision, store.XControlPatch{OwnerID: opts.OwnerID, Refresh: &refresh})
		if err != nil {
			return summary, err
		}
		state.Refresh = refresh
	}
	before := state.Refresh.SweepStartedAt
	if api == nil {
		api = imp.readAPI(opts.SourceID, ReadScope{})
	}
	for {
		if err := imp.finishPendingContent(ctx, opts.SourceID); err != nil {
			return summary, err
		}
		ids, coverage, err := imp.store.XRefreshCandidatesContext(ctx, opts.SourceID, before, opts.BatchSize)
		if err != nil {
			return summary, err
		}
		summary.Outstanding, summary.OldestUncheckedAt, summary.PendingRemoval = coverage.Outstanding, coverage.OldestUncheckedAt, coverage.PendingRemoval
		if coverage.PendingRemoval {
			return summary, store.ErrXRemovalNeedsOwner
		}
		if len(ids) == 0 {
			if err := imp.saveRefreshCoverage(ctx, opts, &summary, true); err != nil {
				return summary, err
			}
			return summary, nil
		}
		if imp.api == nil || imp.credentials == nil {
			return summary, store.ErrXBudgetIdentity
		}
		page, err := api.Lookup(ctx, ids)
		if err != nil {
			return summary, err
		}
		if len(page.RequestedIDs) == 0 {
			return summary, ErrResponseShape
		}
		if err := imp.applyRefreshPage(ctx, opts.SourceID, opts.OwnerID, page); err != nil {
			return summary, err
		}
		next, _, err := imp.store.XRefreshCandidatesContext(ctx, opts.SourceID, before, opts.BatchSize)
		if err != nil {
			return summary, err
		}
		for _, id := range next {
			if slices.Contains(page.RequestedIDs, id) {
				return summary, ErrIncompleteCoverage
			}
		}
		state, err := imp.store.LoadXControlContext(ctx, opts.SourceID)
		if err != nil {
			return summary, err
		}
		refresh := state.Refresh
		refresh.Cursor = page.RequestedIDs[len(page.RequestedIDs)-1]
		if _, err := imp.store.UpdateXControlContext(ctx, opts.SourceID, state.Revision, store.XControlPatch{OwnerID: opts.OwnerID, Refresh: &refresh}); err != nil {
			return summary, err
		}
	}
}

func (imp *Importer) applyRefreshPage(ctx context.Context, sourceID int64, owner string, page Page) error {
	if err := imp.queueProtectedAuthors(ctx, sourceID, page.Users); err != nil {
		return err
	}
	if err := WritePage(ctx, imp.store, sourceID, owner, page, page.FetchedAt); err != nil {
		return err
	}
	if err := imp.queueUnavailableResults(ctx, sourceID, page); err != nil {
		return err
	}
	if err := imp.store.SettleXRequestContext(ctx, sourceID, page.ReservationID, store.XResourceUsage{Posts: page.PostCount, Users: page.UserCount, Persisted: true, Complete: len(page.Errors) == 0}); err != nil {
		return err
	}
	return imp.finishPendingContent(ctx, sourceID)
}

func (imp *Importer) finishPendingContent(ctx context.Context, sourceID int64) error {
	pending, err := imp.store.PendingXRemovalsContext(ctx, sourceID)
	if err != nil {
		return err
	}
	for _, item := range pending {
		work, err := imp.store.RemoveXPostContentContext(ctx, sourceID, item.CanonicalID, item.Reason)
		if err != nil {
			return err
		}
		if imp.cleanup == nil {
			return ErrContentCleanupRequired
		}
		if err := imp.cleanup(ctx, work); err != nil {
			return err
		}
	}
	pending, err = imp.store.PendingXRemovalsContext(ctx, sourceID)
	if err != nil {
		return err
	}
	if len(pending) != 0 {
		return ErrContentCleanupRequired
	}
	globalPending, err := imp.store.XCleanupPendingContext(ctx)
	if err != nil {
		return err
	}
	if globalPending {
		return store.ErrXRemovalNeedsOwner
	}
	return nil
}

func (imp *Importer) saveRefreshCoverage(ctx context.Context, opts RefreshOptions, summary *RefreshSummary, complete bool) error {
	state, err := imp.store.LoadXControlContext(ctx, opts.SourceID)
	if err != nil {
		return err
	}
	before := state.Refresh.SweepStartedAt
	if before.IsZero() {
		before = imp.now().UTC()
	}
	_, coverage, err := imp.store.XRefreshCandidatesContext(ctx, opts.SourceID, before, opts.BatchSize)
	if err != nil {
		return err
	}
	refresh := state.Refresh
	refresh.Outstanding, refresh.OldestUncheckedAt, refresh.PendingRemoval = coverage.Outstanding, coverage.OldestUncheckedAt, coverage.PendingRemoval
	finished := complete && coverage.Outstanding == 0 && !coverage.PendingRemoval
	if finished {
		refresh.LastCompleteAt = imp.now().UTC()
		refresh.SweepStartedAt = time.Time{}
		refresh.Cursor = ""
	}
	_, err = imp.store.UpdateXControlContext(ctx, opts.SourceID, state.Revision, store.XControlPatch{OwnerID: opts.OwnerID, Refresh: &refresh})
	// A rejected checkpoint cannot publish successful completion or its timestamp.
	summary.Complete = finished && err == nil
	if err != nil {
		refresh.LastCompleteAt = state.Refresh.LastCompleteAt
	}
	summary.Outstanding, summary.OldestUncheckedAt, summary.PendingRemoval, summary.LastCompleteAt = refresh.Outstanding, refresh.OldestUncheckedAt, refresh.PendingRemoval, refresh.LastCompleteAt
	summary.Overdue = refreshOverdue(imp.now(), imp.policy.RefreshSweepInterval, *summary)
	return err
}

// ReadRefreshStatus computes retention coverage from stored metadata and an
// explicit clock. Pausing collection, losing credentials or exhausting the
// allowance cannot hide outstanding work or mutate the archive through status.
func ReadRefreshStatus(ctx context.Context, st *store.Store, sourceID int64, now time.Time, interval time.Duration) (summary RefreshSummary, err error) {
	if st == nil || sourceID <= 0 || now.IsZero() || interval <= 0 {
		return summary, errors.New("invalid X refresh status request")
	}
	source, err := st.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return summary, err
	}
	if source.SourceType != "x" || ValidateID(source.Identifier) != nil {
		return summary, errors.New("X refresh status requires a numeric X source")
	}
	state, err := st.LoadXControlContext(ctx, sourceID)
	if err != nil {
		return summary, err
	}
	before := state.Refresh.SweepStartedAt
	if before.IsZero() {
		before = now.UTC().Add(-interval)
	}
	_, coverage, err := st.XRefreshCandidatesContext(ctx, sourceID, before, 1)
	if err != nil {
		return summary, err
	}
	summary.Outstanding = coverage.Outstanding
	summary.PendingRemoval = coverage.PendingRemoval
	summary.OldestUncheckedAt = coverage.OldestUncheckedAt
	summary.LastCompleteAt = state.Refresh.LastCompleteAt
	summary.Complete = !summary.LastCompleteAt.IsZero() && state.Refresh.SweepStartedAt.IsZero() && summary.Outstanding == 0 && !summary.PendingRemoval
	summary.Overdue = refreshOverdue(now, interval, summary)
	return summary, nil
}

func (imp *Importer) refreshDue(ctx context.Context, opts RefreshOptions, api API) error {
	if err := imp.finishPendingContent(ctx, opts.SourceID); err != nil {
		return err
	}
	state, err := imp.store.LoadXControlContext(ctx, opts.SourceID)
	if err != nil {
		return err
	}
	_, coverage, err := imp.store.XRefreshCandidatesContext(ctx, opts.SourceID, imp.now().UTC().Add(-imp.policy.RefreshSweepInterval), opts.BatchSize)
	if err != nil {
		return err
	}
	if state.Refresh.SweepStartedAt.IsZero() && coverage.Outstanding == 0 && !coverage.PendingRemoval {
		return nil
	}
	_, err = imp.refreshScoped(ctx, opts, api)
	if err != nil {
		var summary RefreshSummary
		final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = errors.Join(err, imp.saveRefreshCoverage(final, opts, &summary, false))
	}
	return err
}

func (imp *Importer) queueProtectedAuthors(ctx context.Context, sourceID int64, users []User) error {
	for _, user := range users {
		if user.Protected {
			if err := imp.store.QueueXAuthorRemovalContext(ctx, sourceID, user.ID); err != nil {
				return err
			}
		}
	}
	return nil
}
func (imp *Importer) queueUnavailableResults(ctx context.Context, sourceID int64, page Page) error {
	for _, item := range page.Errors {
		if item.Unavailable && item.ID != "" && (len(page.RequestedIDs) == 0 || slices.Contains(page.RequestedIDs, item.ID)) {
			if _, err := imp.store.RemoveXPostContentContext(ctx, sourceID, item.ID, "deleted"); err != nil {
				return err
			}
		}
	}
	return nil
}
func (imp *Importer) resolveCleanedWalk(ctx context.Context, summary *ImportSummary, owner, segment string, walk *store.XWalk) (*store.XWalk, error) {
	if walk.UnresolvedPage {
		return walk, nil
	}
	completed, err := imp.store.CompletedXRemovalsContext(ctx, summary.SourceID, walk.UnresolvedIDs)
	if err != nil || len(completed) == 0 {
		return walk, err
	}
	next, err := resolveWalkItems(walk, completed)
	if err != nil {
		return walk, err
	}
	if err := imp.saveWalk(ctx, summary, owner, segment, next); err != nil {
		return walk, err
	}
	return next, nil
}

func refreshOverdue(now time.Time, interval time.Duration, summary RefreshSummary) bool {
	if summary.Outstanding == 0 && !summary.PendingRemoval {
		return false
	}
	baseline := summary.LastCompleteAt
	if baseline.IsZero() {
		baseline = summary.OldestUncheckedAt
	}
	return baseline.IsZero() || !now.UTC().Before(baseline.Add(interval))
}
