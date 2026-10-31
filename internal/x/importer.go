package x

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

var ErrIncompleteCoverage = errors.New("X collection has unresolved coverage")

var ErrArchiveRefreshRequired = errors.New("X refresh is due or incomplete; run sync-x without --archive before paid archive collection")

var errRunCostEstimate = errors.New("X run cost estimate cannot be reconciled")

// ImportOptions pins collection to an existing verified numeric account.
type ImportOptions struct {
	SourceID       int64
	OwnerID        string
	After          time.Time
	Archive        bool
	MaxSpendMicros *int64
	JobID          string
	NewJob         bool
}

// ImportSummary reports durable work and independent endpoint coverage.
type ImportSummary struct {
	Mode               string          `json:"mode"`
	SourceID           int64           `json:"source_id"`
	SyncRunID          int64           `json:"sync_run_id"`
	Status             string          `json:"status"`
	Code               string          `json:"code,omitempty"`
	MessagesProcessed  int64           `json:"messages_processed"`
	MessagesAdded      int64           `json:"messages_added"`
	MessagesUpdated    int64           `json:"messages_updated"`
	OwnComplete        bool            `json:"own_complete"`
	MentionsComplete   bool            `json:"mentions_complete"`
	OwnWatermark       string          `json:"own_watermark,omitempty"`
	MentionsWatermark  string          `json:"mentions_watermark,omitempty"`
	HistoryGapPossible bool            `json:"history_gap_possible"`
	CredentialError    string          `json:"credential_error,omitempty"`
	ModelError         string          `json:"model_error,omitempty"`
	RunEstimatedMicros int64           `json:"run_estimated_micros"`
	RunReservedMicros  int64           `json:"run_reserved_micros"`
	EstimatedMicros    int64           `json:"month_estimated_micros"`
	ReservedMicros     int64           `json:"month_reserved_micros"`
	Archive            *ArchiveSummary `json:"archive,omitempty"`
}

type collectionWalk struct {
	segment string
	walk    *store.XWalk
}

type Importer struct {
	store       *store.Store
	api         API
	policy      config.XAccountConfig
	credentials Credentials
	now         func() time.Time
	cleanup     func(context.Context, store.XRemovalWork) error
}

func NewImporter(st *store.Store, api API, policy config.XAccountConfig, credentials Credentials) *Importer {
	return &Importer{store: st, api: api, policy: policy, credentials: credentials, now: time.Now}
}

// Import holds source execution across reads, while every Store operation is
// short. The raw API is wrapped exactly once; checkpoints are diagnostic only.
func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (summary *ImportSummary, retErr error) {
	if imp == nil || imp.store == nil || imp.api == nil || imp.credentials == nil || opts.SourceID <= 0 || ValidateID(opts.OwnerID) != nil {
		return nil, errors.New("X import requires a source, verified owner, API and credentials")
	}
	if err := imp.policy.Validate(opts.OwnerID); err != nil {
		return nil, err
	}
	if !opts.Archive && (opts.MaxSpendMicros != nil || opts.JobID != "" || opts.NewJob) {
		return nil, errors.New("X archive job options require archive mode")
	}
	source, err := imp.store.GetSourceByIDContext(ctx, opts.SourceID)
	if err != nil {
		return nil, err
	}
	if source.SourceType != "x" || source.Identifier != opts.OwnerID || source.MergedIntoSourceID != 0 {
		return nil, errors.New("X import source does not match its numeric owner")
	}
	summary = &ImportSummary{SourceID: source.ID, Status: "partial", Mode: "collection"}
	if opts.Archive {
		summary.Mode = "archive"
	}
	defer func() {
		if retErr != nil {
			summary.Status = "partial"
			summary.Code = importErrorCode(retErr)
		}
	}()
	execution, err := imp.store.AcquireSyncExecutionContext(ctx, source.ID)
	if err != nil {
		return summary, err
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	state, err := imp.store.LoadXControlContext(ctx, source.ID)
	if err != nil {
		return summary, err
	}
	spendBefore := slices.Clone(state.SpendBuckets)
	var walks []collectionWalk
	scope := ReadScope{}
	if opts.Archive {
		job, err := imp.store.PrepareXArchiveJobContext(ctx, source.ID, store.XArchiveJobSpec{OwnerID: opts.OwnerID, After: opts.After, Before: imp.now(), CapMicros: opts.MaxSpendMicros, JobID: opts.JobID, NewJob: opts.NewJob, Preview: opts.MaxSpendMicros == nil})
		if err != nil {
			return summary, err
		}
		summary.Archive, err = imp.archiveSummary(job, opts.MaxSpendMicros == nil)
		if err != nil {
			return summary, err
		}
		if summary.Archive.Preview {
			imp.describeControl(summary, state)
			summary.Status = "preview"
			return summary, nil
		}
		// Optional historical expansion must not consume the monthly allowance
		// needed by overdue maintenance. Do not silently add refresh spending to
		// an archive job, or change preview/zero-cap resource-request semantics.
		if *opts.MaxSpendMicros > 0 && !job.Complete {
			_, coverage, err := imp.store.XRefreshCandidatesContext(ctx, source.ID, imp.now().UTC().Add(-imp.policy.RefreshSweepInterval), 1)
			if err != nil {
				return summary, err
			}
			if !state.Refresh.SweepStartedAt.IsZero() || coverage.Outstanding != 0 || coverage.PendingRemoval {
				imp.describeControl(summary, state)
				return summary, ErrArchiveRefreshRequired
			}
		}
		scope.JobID = job.ID
		walks = []collectionWalk{{"archive-own", job.Own}, {"archive-mentions", job.Mentions}}
	} else {
		own, err := prepareTimelineWalk(opts.OwnerID, "timeline", state.Own, opts.After, imp.now())
		if err != nil {
			return summary, err
		}
		mentions, err := prepareTimelineWalk(opts.OwnerID, "mentions", state.Mentions, opts.After, imp.now())
		if err != nil {
			return summary, err
		}
		walks = []collectionWalk{{"own", own}, {"mentions", mentions}}
	}
	runID, err := execution.StartSyncContext(ctx, "x", "")
	if err != nil {
		return summary, err
	}
	summary.SyncRunID = runID
	scoped := *imp
	scoped.store = imp.store.ScopedToSync(source.ID, runID)
	imp = &scoped
	completed := false
	defer func() {
		finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		current, readErr := imp.store.LoadXControlContext(finalCtx, source.ID)
		if readErr == nil {
			imp.describeControl(summary, current)
			spent, reserved, costErr := runCostDelta(spendBefore, current.SpendBuckets)
			if costErr != nil {
				retErr = errors.Join(retErr, costErr)
			} else {
				summary.RunEstimatedMicros, summary.RunReservedMicros = spent, reserved
			}
		} else {
			retErr = errors.Join(retErr, readErr)
		}
		if retErr != nil {
			summary.Status = "partial"
			summary.Code = importErrorCode(retErr)
			if !completed {
				retErr = errors.Join(retErr, imp.store.InterruptSyncWithCheckpoint(finalCtx, runID, retErr, imp.diagnosticCheckpoint(summary, current)))
			}
		}
	}()
	if err := imp.finishPendingContent(ctx, source.ID); err != nil {
		return summary, err
	}
	for _, entry := range walks {
		if err := imp.saveWalk(ctx, summary, opts.OwnerID, entry.segment, entry.walk); err != nil {
			return summary, err
		}
	}
	admitted := imp.readAPI(source.ID, scope)
	if !opts.Archive {
		if err := imp.refreshDue(ctx, RefreshOptions{SourceID: source.ID, OwnerID: opts.OwnerID, BatchSize: imp.policy.RefreshBatchSize}, admitted); err != nil {
			return summary, err
		}
	}
	var failures []error
	for _, entry := range walks {
		if err := imp.collectWalk(ctx, admitted, summary, opts.OwnerID, entry.segment, entry.walk); err != nil {
			failures = append(failures, fmt.Errorf("X %s walk: %w", entry.segment, err))
			var rejected *APIError
			if errors.As(err, &rejected) && rejected.StatusCode == 402 {
				// Credits belong to the account. Trying its other endpoint
				// cannot recover coverage after the provider has stopped it.
				break
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	if len(failures) > 0 {
		return summary, errors.Join(failures...)
	}
	if !opts.Archive {
		current, err := imp.store.LoadXControlContext(ctx, source.ID)
		if err != nil {
			return summary, err
		}
		overlap, err := prepareOverlapWalk(opts.OwnerID, current.Overlap, imp.now())
		if err != nil {
			return summary, err
		}
		for {
			if err := imp.saveWalk(ctx, summary, opts.OwnerID, "overlap", overlap); err != nil {
				return summary, err
			}
			if !overlap.Complete {
				if err := imp.collectWalk(ctx, admitted, summary, opts.OwnerID, "overlap", overlap); err != nil {
					return summary, err
				}
				current, err = imp.store.LoadXControlContext(ctx, source.ID)
				if err != nil {
					return summary, err
				}
				overlap = current.Overlap
			}
			if overlap.Endpoint == "mentions" {
				break
			}
			overlap, err = nextOverlapEndpoint(overlap)
			if err != nil {
				return summary, err
			}
		}
	}
	current, err := imp.store.LoadXControlContext(ctx, source.ID)
	if err != nil {
		return summary, err
	}
	if err := imp.store.CompleteXSyncContext(ctx, runID, store.XControlPatch{OwnerID: opts.OwnerID, ExpectedRevision: current.Revision}); err != nil {
		return summary, err
	}
	completed = true
	summary.Status = "complete"
	return summary, nil
}

func (imp *Importer) collectWalk(ctx context.Context, api API, summary *ImportSummary, owner, segment string, walk *store.XWalk) error {
	replayed := false
	for !walk.Complete {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkExhausted(walk) || len(walk.UnresolvedIDs) > store.XMaxUnresolvedIDs-100 {
			resolved, err := imp.resolveWalkObligations(ctx, api, summary, owner, segment, walk)
			if err != nil {
				return err
			}
			walk = resolved
			if walk.Complete {
				return nil
			}
			if walkExhausted(walk) {
				return ErrIncompleteCoverage
			}
		}
		if len(walk.SeenTokens) >= 1000 {
			return errors.New("X page checkpoint capacity reached")
		}
		var page Page
		var err error
		if walk.Endpoint == store.XRequestSearch {
			page, err = api.SearchAll(ctx, SearchRequest{Query: walk.Query, After: walk.After, Before: walk.Before, Token: walk.Token, MaxResults: 100})
		} else {
			page, err = api.Timeline(ctx, timelineWalkRequest(walk, 100))
		}
		if err != nil {
			var failure *APIError
			if walk.Token != "" && !replayed && errors.As(err, &failure) && failure.Code == "invalid_pagination_token" {
				replayed = true
				walk = replayWalk(walk)
				if err := imp.saveWalk(ctx, summary, owner, segment, walk); err != nil {
					return err
				}
				continue
			}
			return err
		}
		if err := imp.persistPage(ctx, summary, owner, walk, page); err != nil {
			return err
		}
		progress := page
		progress.Posts = postsInWalkScope(page.Posts, walk, owner)
		progress.Errors = slices.Clone(page.Errors)
		for _, post := range page.Posts {
			if postUnavailable(post, page.Users) {
				progress.Errors = append(progress.Errors, ItemError{ID: post.ID, Code: "unavailable", Unavailable: true})
			}
		}
		next, err := advanceWalk(walk, progress)
		if err != nil {
			return err
		}
		if err := imp.saveWalk(ctx, summary, owner, segment, next); err != nil {
			return err
		}
		if err := imp.finishPendingContent(ctx, summary.SourceID); err != nil {
			return err
		}
		walk, err = imp.resolveCleanedWalk(ctx, summary, owner, segment, next)
		if err != nil {
			return err
		}
		if walk.UnresolvedPage {
			return ErrUnresolvedPage
		}
	}
	return nil
}

func (imp *Importer) persistPage(ctx context.Context, summary *ImportSummary, owner string, walk *store.XWalk, page Page) error {
	if err := imp.queueProtectedAuthors(ctx, summary.SourceID, page.Users); err != nil {
		return err
	}
	filtered := page
	filtered.Posts = nil
	filtered.Users = nil
	authors := make(map[string]bool)
	for _, post := range postsInWalkScope(page.Posts, walk, owner) {
		filtered.Posts = append(filtered.Posts, post)
		authors[post.AuthorID] = true
	}
	for _, user := range page.Users {
		if authors[user.ID] {
			filtered.Users = append(filtered.Users, user)
		}
	}
	effects, err := WritePageWithSummary(ctx, imp.store, summary.SourceID, owner, filtered, page.FetchedAt)
	summary.MessagesAdded += effects.Added
	summary.MessagesUpdated += effects.Updated
	if err != nil {
		return err
	}
	if err := imp.queueUnavailableResults(ctx, summary.SourceID, page); err != nil {
		return err
	}
	summary.MessagesProcessed += int64(len(page.Posts))
	return imp.store.SettleXRequestContext(ctx, summary.SourceID, page.ReservationID, store.XResourceUsage{Posts: page.PostCount, Users: page.UserCount, Persisted: true, Complete: len(page.Errors) == 0})
}

func (imp *Importer) resolveWalkObligations(ctx context.Context, api API, summary *ImportSummary, owner, segment string, walk *store.XWalk) (*store.XWalk, error) {
	if walk.UnresolvedPage {
		return walk, ErrUnresolvedPage
	}
	var err error
	walk, err = imp.resolveCleanedWalk(ctx, summary, owner, segment, walk)
	if err != nil {
		return walk, err
	}
	remaining := slices.Clone(walk.UnresolvedIDs)
	for len(remaining) > 0 {
		batch := remaining[:min(len(remaining), 100)]
		page, err := api.Lookup(ctx, batch)
		if err != nil {
			return walk, err
		}
		if len(page.RequestedIDs) == 0 {
			return walk, ErrResponseShape
		}
		if err := imp.persistPage(ctx, summary, owner, walk, page); err != nil {
			return walk, err
		}
		if err := imp.finishPendingContent(ctx, summary.SourceID); err != nil {
			return walk, err
		}
		resolved, err := imp.store.CompletedXRemovalsContext(ctx, summary.SourceID, page.RequestedIDs)
		if err != nil {
			return walk, err
		}
		for _, id := range page.RequestedIDs {
			if slices.Contains(resolved, id) {
				continue
			}
			for _, post := range page.Posts {
				if post.ID != id || postUnavailable(post, page.Users) {
					continue
				}
				target, targetErr := imp.store.ResolveXPostContext(ctx, summary.SourceID, id)
				if targetErr == nil && (target.Availability == "protected" || target.Availability == "withheld" || target.Availability == "removed" || target.Availability == "deleted") {
					continue
				}
				if targetErr != nil && !errors.Is(targetErr, store.ErrXPostNotFound) {
					return walk, targetErr
				}
				resolved = append(resolved, id)
				break
			}
		}
		next, err := resolveWalkItems(walk, resolved, postsInWalkScope(page.Posts, walk, owner)...)
		if err != nil {
			return walk, err
		}
		if err := imp.saveWalk(ctx, summary, owner, segment, next); err != nil {
			return walk, err
		}
		walk = next
		remaining = slices.DeleteFunc(remaining, func(id string) bool { return slices.Contains(page.RequestedIDs, id) })
	}
	if len(walk.UnresolvedIDs) > 0 {
		return walk, ErrIncompleteCoverage
	}
	return walk, nil
}

func postsInWalkScope(posts []Post, walk *store.XWalk, owner string) []Post {
	var accepted []Post
	for _, post := range posts {
		if !postInWalk(post, walk) {
			continue
		}
		// A frozen handle query is not proof of authorship. The returned
		// numeric author keeps own-post collection bound to its account.
		if walk.Endpoint == store.XRequestSearch && strings.HasPrefix(walk.Query, "from:") && post.AuthorID != owner {
			continue
		}
		accepted = append(accepted, post)
	}
	return accepted
}

func postUnavailable(post Post, users []User) bool {
	if post.Withheld {
		return true
	}
	for _, user := range users {
		if user.ID == post.AuthorID && user.Protected {
			return true
		}
	}
	return false
}

func (imp *Importer) saveWalk(ctx context.Context, summary *ImportSummary, owner, segment string, walk *store.XWalk) error {
	state, err := imp.store.LoadXControlContext(ctx, summary.SourceID)
	if err != nil {
		return err
	}
	patch := store.XControlPatch{OwnerID: owner}
	switch segment {
	case "own":
		patch.Own = walk
	case "mentions":
		patch.Mentions = walk
	case "overlap":
		patch.Overlap = walk
	case "archive-own", "archive-mentions":
		if summary.Archive == nil || summary.Archive.JobID == "" {
			return store.ErrXArchiveScope
		}
		found := false
		for i := range state.Jobs {
			if state.Jobs[i].ID == summary.Archive.JobID {
				if segment == "archive-own" {
					state.Jobs[i].Own = walk
				} else {
					state.Jobs[i].Mentions = walk
				}
				state.Jobs[i].Complete = state.Jobs[i].Own.Complete && state.Jobs[i].Mentions.Complete
				found = true
				break
			}
		}
		if !found {
			return store.ErrXArchiveScope
		}
		patch.Jobs = &state.Jobs
	default:
		return errors.New("unknown X walk segment")
	}
	current, err := imp.store.UpdateXControlContext(ctx, summary.SourceID, state.Revision, patch)
	if err != nil {
		return err
	}
	return imp.store.UpdateSyncCheckpointContext(ctx, summary.SyncRunID, imp.diagnosticCheckpoint(summary, current))
}

func (imp *Importer) diagnosticCheckpoint(summary *ImportSummary, state store.XControlState) *store.Checkpoint {
	diagnostic := struct {
		Version         int          `json:"version"`
		ControlRevision int64        `json:"control_revision"`
		Own             *store.XWalk `json:"own,omitempty"`
		Mentions        *store.XWalk `json:"mentions,omitempty"`
		Overlap         *store.XWalk `json:"overlap,omitempty"`
	}{1, state.Revision, state.Own, state.Mentions, state.Overlap}
	data, err := json.Marshal(diagnostic)
	if err != nil {
		return nil
	}
	return &store.Checkpoint{PageToken: string(data), MessagesProcessed: summary.MessagesProcessed, MessagesAdded: summary.MessagesAdded, MessagesUpdated: summary.MessagesUpdated}
}

func (imp *Importer) describeControl(summary *ImportSummary, state store.XControlState) {
	if summary.Archive != nil && summary.Archive.JobID != "" {
		for _, job := range state.Jobs {
			if job.ID == summary.Archive.JobID {
				describeArchiveJob(summary.Archive, job)
				break
			}
		}
	}
	if state.Own != nil {
		summary.OwnComplete = state.Own.Complete
		summary.OwnWatermark = state.Own.Watermark
		summary.HistoryGapPossible = state.Own.HistoryGapPossible
	}
	if state.Mentions != nil {
		summary.MentionsComplete = state.Mentions.Complete
		summary.MentionsWatermark = state.Mentions.Watermark
		summary.HistoryGapPossible = summary.HistoryGapPossible || state.Mentions.HistoryGapPossible
	}
	summary.CredentialError = state.CredentialError
	summary.ModelError = state.ModelError
	for _, bucket := range state.SpendBuckets {
		if bucket.Month == imp.now().UTC().Format("2006-01") {
			summary.EstimatedMicros = bucket.SpentMicros
			summary.ReservedMicros = bucket.ReservedMicros
		}
	}
}

func (imp *Importer) readAPI(sourceID int64, scope ReadScope) API {
	admitted := NewAdmittedAPI(imp.store, sourceID, imp.api, imp.policy, imp.credentials, scope).(*admittedAPI)
	admitted.now = imp.now
	admitted.beforeLookup = func(ctx context.Context, ids []string) error {
		return imp.store.MarkXPostsUnverifiedContext(ctx, sourceID, ids)
	}
	return admitted
}

func importErrorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "interrupted"
	case errors.Is(err, errRunCostEstimate):
		return "cost_estimate_unavailable"
	case errors.Is(err, ErrArchiveRefreshRequired):
		return "refresh_required"
	case errors.Is(err, store.ErrXArchiveScope):
		return "archive_scope"
	case errors.Is(err, store.ErrXBudgetExceeded):
		return "budget_exhausted"
	case errors.Is(err, store.ErrXModelError), errors.Is(err, ErrReadModelExceeded):
		return "read_model_error"
	case errors.Is(err, store.ErrXBudgetIdentity):
		return "credential_unavailable"
	default:
		var rejected *APIError
		if errors.As(err, &rejected) {
			return rejected.Code
		}
		return "incomplete_coverage"
	}
}

func runCostDelta(before, after []store.XSpendBucket) (spent, reserved int64, err error) {
	// Source execution excludes other admissions and settlements for this
	// account. Existing unknown reservations remain charged; only this run's
	// new requests can change these counters. Keep each original UTC month.
	previous := make(map[string]store.XSpendBucket, len(before))
	for _, bucket := range before {
		if _, exists := previous[bucket.Month]; exists || bucket.SpentMicros < 0 || bucket.ReservedMicros < 0 {
			return 0, 0, errRunCostEstimate
		}
		previous[bucket.Month] = bucket
	}
	seen := make(map[string]bool, len(after))
	for _, bucket := range after {
		old := previous[bucket.Month]
		if seen[bucket.Month] || bucket.SpentMicros < old.SpentMicros || bucket.ReservedMicros < old.ReservedMicros {
			return 0, 0, errRunCostEstimate
		}
		spentDelta, reservedDelta := bucket.SpentMicros-old.SpentMicros, bucket.ReservedMicros-old.ReservedMicros
		if spentDelta > math.MaxInt64-spent || reservedDelta > math.MaxInt64-reserved {
			return 0, 0, errRunCostEstimate
		}
		spent += spentDelta
		reserved += reservedDelta
		seen[bucket.Month] = true
		delete(previous, bucket.Month)
	}
	if len(previous) != 0 {
		return 0, 0, errRunCostEstimate
	}
	return spent, reserved, nil
}
