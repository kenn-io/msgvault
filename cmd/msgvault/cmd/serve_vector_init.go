package cmd

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/visual"
)

// setupVectorFeaturesForRun is a test seam for the build-tag-selected
// setupVectorFeatures implementation.
var setupVectorFeaturesForRun = setupVectorFeatures

// resolveActiveGeneration is a test seam for the staleness check
// startVectorInit runs at init completion. It defaults to the same
// vector.ResolveActiveForFingerprint the hybrid engine calls at query time,
// so init-time detection and query-time 503s share one comparison.
var resolveActiveGeneration = vector.ResolveActiveForFingerprint

// vectorInitHandle tracks the background vector init goroutine so shutdown
// can wait for it and close the opened backend.
type vectorInitHandle struct {
	done chan struct{}
	mu   sync.Mutex
	vf   *vectorFeatures
}

// WaitContext blocks until the init goroutine finishes or ctx is done.
// Returns true if the goroutine finished, false if ctx ended first. When both
// h.done and ctx are ready it deterministically prefers h.done so a completed
// init still reports true (and its backend gets closed) even if the shutdown
// budget expired in the same instant.
func (h *vectorInitHandle) WaitContext(ctx context.Context) bool {
	select {
	case <-h.done:
		return true
	default:
	}
	select {
	case <-h.done:
		return true
	case <-ctx.Done():
		// ctx won the race: re-check h.done in case it was also ready, so a
		// finished init is never misreported as timed out.
		select {
		case <-h.done:
			return true
		default:
			return false
		}
	}
}

// WaitTimeout blocks until the init goroutine finishes or d elapses.
// Returns false on timeout.
func (h *vectorInitHandle) WaitTimeout(d time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return h.WaitContext(ctx)
}

// CloseFeatures closes the vector backend if the init goroutine opened one.
// Only call after WaitTimeout reports the goroutine finished.
func (h *vectorInitHandle) CloseFeatures() {
	h.mu.Lock()
	vf := h.vf
	h.vf = nil
	h.mu.Unlock()
	if vf != nil && vf.Close != nil {
		if err := vf.Close(); err != nil {
			logger.Warn("closing vectors.db failed", "error", err)
		}
	}
}

// startVectorInit runs the expensive vector backend setup (open, schema
// migrations, embed_gen backfill) in the background so the daemon API can
// serve archive requests immediately. The tracker (idle tracker + operation
// gate) serializes the init's msgvault.db writes against scheduled syncs
// and keeps a background daemon from idle-stopping mid-migration. On
// success the components are installed into apiServer and the embed job is
// registered; on failure the daemon keeps serving with vector endpoints
// reporting the error.
func startVectorInit(
	ctx context.Context,
	s *store.Store,
	dbPath string,
	tracker scheduler.WorkTracker,
	apiServer *api.Server,
	sched *scheduler.Scheduler,
	openers ...visual.StreamOpener,
) *vectorInitHandle {
	h := &vectorInitHandle{done: make(chan struct{})}
	if !cfg.Vector.AnyLaneEnabled() {
		close(h.done)
		return h
	}
	go func() {
		defer close(h.done)
		logger.Info("daemon startup step",
			"step", "init_vector_backend",
			"detail", "running in background; may run vector schema migrations and embed_gen backfill on large archives")
		if tracker != nil {
			release, ok := tracker.BeginWorkContext(ctx)
			if !ok {
				logger.Info("vector init aborted", "reason", "daemon shutting down")
				return
			}
			defer release()
		}
		vf, err := setupVectorFeaturesForRun(ctx, s, dbPath, false, openers...)
		if err != nil {
			if ctx.Err() != nil {
				logger.Info("vector init cancelled during daemon shutdown")
				return
			}
			logger.Error("vector init failed; vector search unavailable until fixed",
				"error", err)
			apiServer.SetVectorInitError(err)
			return
		}
		if vf == nil {
			// setupVectorFeaturesForRun returns non-nil whenever
			// cfg.Vector.Enabled is true and err is nil; this guards test
			// seams (and any future caller) that don't uphold that
			// invariant instead of panicking on a nil dereference below.
			logger.Warn("vector init returned no components despite no error; leaving vector search uninitialized")
			return
		}
		h.mu.Lock()
		h.vf = vf
		h.mu.Unlock()
		if cfg.Vector.Enabled {
			apiServer.SetVectorFeatures(
				vf.HybridEngine, vf.PersonSearchEngine, vf.Backend, vf.Cfg,
			)
			apiServer.SetDocumentSearchService(vf.DocumentSearch)
			// Preflight drift detection: vector-search requests re-resolve the
			// durable account scope (throttled) so drift latches index_stale
			// even on daemons whose embed job never runs (empty cron,
			// run_after_sync=false). The embed job's own per-run check remains
			// the detection path for scheduled embeds.
			apiServer.SetVectorScopeCheck(embedScopeDriftCheck(s, vf.Cfg.Embed.Scope.BuildScope()))
			checkVectorIndexFreshness(ctx, apiServer, vf)
			if err := registerEmbedJob(sched, vf, s, apiServer); err != nil {
				// Cron was validated in precheckVectorFeatures, so this is an
				// invariant violation, not user error; vector search still works.
				logger.Error("register embed job failed", "error", err)
			}
		}
		if vf.Visual != nil {
			searchService, searchErr := visual.NewSearchService(s, vf.Visual.Provider, vf.Visual.Backend,
				cfg.Vector.Multimodal.ImageQueriesEnabled())
			if searchErr == nil {
				searchService.ExpectFingerprint(vf.Visual.Generation.Fingerprint)
				searchService.SetScopeCheck(vf.Visual.ScopeCheck)
			}
			if searchErr != nil {
				logger.Error("configure multimodal search failed", "error", searchErr)
			} else {
				apiServer.SetVisualSearch(searchService)
			}
			build := func(runCtx context.Context, scope operations.PassScope) error {
				return runVisualOperation(runCtx, vf.Visual, scope, func(ctx context.Context) (visual.WorkerResult, error) {
					if err := vf.Visual.Archive.ConsentVisualGeneration(
						ctx, vf.Visual.Generation.ID, vf.Visual.PolicyFingerprint); err != nil {
						return visual.WorkerResult{}, err
					}
					return runVisualOnce(ctx, vf.Visual)
				})
			}
			resume := func(runCtx context.Context, scope operations.PassScope) error {
				return runVisualOperation(runCtx, vf.Visual, scope, func(ctx context.Context) (visual.WorkerResult, error) {
					if err := requireVisualConsent(ctx, vf.Visual); err != nil {
						return visual.WorkerResult{}, err
					}
					return runVisualOnce(ctx, vf.Visual)
				})
			}
			apiServer.SetVisualOperations(build, resume, func(
				runCtx context.Context, scope operations.PassScope, messageID int64, hash string,
			) error {
				return runVisualOperation(runCtx, vf.Visual, scope, func(ctx context.Context) (visual.WorkerResult, error) {
					if err := requireVisualConsent(ctx, vf.Visual); err != nil {
						return visual.WorkerResult{}, err
					}
					if vf.Visual.ScopeCheck != nil {
						if err := vf.Visual.ScopeCheck(ctx); err != nil {
							return visual.WorkerResult{}, err
						}
					}
					result, err := vf.Visual.Reconciler.RetryOwner(ctx, messageID, hash)
					if err != nil || len(result.Work) == 0 {
						return visual.WorkerResult{}, err
					}
					return vf.Visual.Worker.Run(ctx, result.Work)
				})
			}, func(statusCtx context.Context, includeCoverage bool) (visual.Status, error) {
				return vf.Visual.Reconciler.Status(statusCtx, visual.ProviderUsage{}, false, includeCoverage)
			}, func(retireCtx context.Context) error {
				// Retire FIRST: prepare and commit refuse retired
				// generations, so no publisher can add a token after this
				// point and the enumeration below is complete.
				if err := vf.Visual.Reconciler.Retire(retireCtx); err != nil {
					return err
				}
				tokens, err := vf.Visual.Reconciler.GenerationTokens(retireCtx)
				if err != nil {
					return err
				}
				visualTokens := make([]visual.VectorToken, len(tokens))
				for i, token := range tokens {
					visualTokens[i] = visual.VectorToken(token)
				}
				return vf.Visual.Backend.DeleteTokens(retireCtx, visualTokens)
			})
			if err := registerVisualJob(sched, vf.Visual); err != nil {
				logger.Error("register multimodal job failed", "error", err)
			}
		}
		if err := registerDocumentVectorJob(sched, vf, s); err != nil {
			logger.Error("register document vector job failed", "error", err)
		}
		logger.Info("daemon startup step complete", "step", "init_vector_backend")
	}()
	return h
}

// requireVisualConsent verifies durable hosted-processing consent AND that it
// was recorded against the docbank policy fingerprint of the manifest now in
// force. A re-probed manifest changes the fingerprint and needs new consent.
func requireVisualConsent(ctx context.Context, vf *visualFeatures) error {
	generation, err := vf.Archive.GetVisualGeneration(ctx, vf.Generation.ID)
	if err != nil {
		return err
	}
	// A retired generation is never revisited: letting the installed resume
	// and retry callbacks run against it would repopulate vectors that
	// activation can never expose and upload media for nothing.
	if generation.State != store.VisualGenerationBuilding && generation.State != store.VisualGenerationActive {
		return errors.New("the configured visual generation is retired; restart the daemon to begin a new build")
	}
	if !generation.Consented {
		return errors.New("visual generation requires explicit hosted-processing consent")
	}
	if generation.ConsentPolicyFingerprint != vf.PolicyFingerprint {
		return errors.New("recorded consent covers a different capability manifest; run `msgvault multimodal build --yes` to consent to the manifest in force")
	}
	return nil
}

func runVisualOperation(
	ctx context.Context,
	vf *visualFeatures,
	scope operations.PassScope,
	execute func(context.Context) (visual.WorkerResult, error),
) (runErr error) {
	pass, terminal, err := beginCommandOperationPass(
		ctx, vf.Archive, operations.KindVisualEmbedding, scope,
	)
	if err != nil {
		return err
	}
	if terminal != nil {
		return operations.TerminalReplayOutcome(terminal)
	}
	var result visual.WorkerResult
	defer func() {
		counters := visualEmbeddingCounters(result)
		pass.checkpoint(ctx, counters)
		pass.finish(ctx, counters, runErr)
	}()
	result, runErr = execute(ctx)
	return runErr
}

func visualEmbeddingCounters(result visual.WorkerResult) operations.InvocationCounters {
	return operations.InvocationCounters{
		Attempted: result.Attempted, Succeeded: result.Succeeded,
		Failed: result.Failed, Skipped: result.Skipped,
	}
}

func runVisualPass(ctx context.Context, vf *visualFeatures, scope operations.PassScope) error {
	return runVisualOperation(ctx, vf, scope, func(ctx context.Context) (visual.WorkerResult, error) {
		return runVisualOnce(ctx, vf)
	})
}

func runVisualOnce(ctx context.Context, vf *visualFeatures) (visual.WorkerResult, error) {
	result, passErr := executeVisualPass(ctx, vf)
	// The pass's own worker mutations (commit replacements, drift discards)
	// park obsolete tokens after the opening sweep already ran; drain them
	// now so an unscheduled installation's final pass does not leave
	// unreachable backend vectors behind. Best-effort on a failed pass.
	if cleanupErr := cleanupObsoleteVisualVectors(ctx, vf); cleanupErr != nil && passErr == nil {
		return result, cleanupErr
	}
	return result, passErr
}

func executeVisualPass(ctx context.Context, vf *visualFeatures) (visual.WorkerResult, error) {
	if vf.ScopeCheck != nil {
		if err := vf.ScopeCheck(ctx); err != nil {
			return visual.WorkerResult{}, err
		}
	}
	if err := cleanupObsoleteVisualVectors(ctx, vf); err != nil {
		return visual.WorkerResult{}, err
	}
	if err := cleanupRetiredVisualGenerations(ctx, vf); err != nil {
		return visual.WorkerResult{}, err
	}
	needsFull, err := vf.Reconciler.NeedsFullReconcile(ctx)
	if err != nil {
		return visual.WorkerResult{}, err
	}
	if needsFull {
		result, reconcileErr := vf.Reconciler.FullReconcile(ctx)
		if reconcileErr != nil {
			return visual.WorkerResult{}, reconcileErr
		}
		if len(result.Work) > 0 {
			return vf.Worker.Run(ctx, result.Work)
		}
		// A page-bounded pass can find no work without reaching the end of
		// the archive. Replay rejects consumers whose baseline is still
		// open, so report the resumable progress as success and let a later
		// pass continue the scan.
		stillNeedsFull, err := vf.Reconciler.NeedsFullReconcile(ctx)
		if err != nil {
			return visual.WorkerResult{}, err
		}
		if stillNeedsFull {
			return visual.WorkerResult{}, nil
		}
	}
	result, err := vf.Reconciler.Replay(ctx)
	if err != nil {
		return visual.WorkerResult{}, err
	}
	if len(result.Work) > 0 {
		return vf.Worker.Run(ctx, result.Work)
	}
	status, err := vf.Reconciler.Status(ctx, visual.ProviderUsage{}, false, false)
	if err != nil {
		return visual.WorkerResult{}, err
	}
	if status.ReconciliationComplete && status.JournalLag == 0 && status.ActiveLeases == 0 &&
		status.Stale == 0 && status.Retryable == 0 && status.Converged == status.ConvergenceTotal {
		if _, err := vf.Reconciler.Activate(ctx); err != nil {
			return visual.WorkerResult{}, err
		}
		return visual.WorkerResult{}, cleanupRetiredVisualGenerations(ctx, vf)
	}
	return visual.WorkerResult{}, nil
}

// cleanupRetiredVisualGenerations deletes retired generations' backend
// vectors and unregisters their journal consumers. Leaving either behind
// would accumulate unreachable vectors and pin journal pruning forever.
// Targets are re-derived from the store rather than taken from the
// activation swap, so a transient cleanup failure is retried on the next
// pass instead of permanently orphaning an already-retired generation.
func cleanupRetiredVisualGenerations(ctx context.Context, vf *visualFeatures) error {
	retired, err := vf.Archive.ListRetiredVisualGenerations(ctx)
	if err != nil {
		return err
	}
	for _, generation := range retired {
		tokens, err := vf.Archive.ListVisualGenerationTokens(ctx, generation.ID)
		if err != nil {
			return err
		}
		if len(tokens) > 0 {
			vectorTokens := make([]visual.VectorToken, len(tokens))
			for index, token := range tokens {
				vectorTokens[index] = visual.VectorToken(token)
			}
			if err := vf.Backend.DeleteTokens(ctx, vectorTokens); err != nil {
				return err
			}
			if err := vf.Archive.PurgeRetiredVisualGeneration(ctx, generation.ID); err != nil {
				return err
			}
		}
		consumerKey := "visual/" + generation.Fingerprint
		if _, err := vf.Archive.GetAttachmentChangeConsumer(ctx, consumerKey); err != nil {
			if errors.Is(err, store.ErrAttachmentChangeConsumerMissing) {
				continue
			}
			return err
		}
		if err := vf.Archive.UnregisterAttachmentChangeConsumer(ctx, consumerKey); err != nil {
			return err
		}
	}
	return nil
}

func cleanupObsoleteVisualVectors(ctx context.Context, vf *visualFeatures) error {
	// Drain every page: one bounded pass can park far more than a page of
	// obsolete tokens, and an installation without a schedule would never
	// revisit the leftovers. Each cleared token shrinks the ledger, so this
	// terminates.
	for {
		tokens, err := vf.Reconciler.ObsoleteTokens(ctx, 100)
		if err != nil {
			return err
		}
		if len(tokens) == 0 {
			return nil
		}
		for _, token := range tokens {
			if err := vf.Backend.DeleteTokens(ctx, []visual.VectorToken{visual.VectorToken(token)}); err != nil {
				return err
			}
			if err := vf.Reconciler.ClearObsoleteToken(ctx, token); err != nil {
				return err
			}
		}
	}
}

func registerVisualJob(sched *scheduler.Scheduler, vf *visualFeatures) error {
	runScheduled := func(ctx context.Context) error {
		generation, err := vf.Archive.GetVisualGeneration(ctx, vf.Generation.ID)
		if err != nil ||
			(generation.State != store.VisualGenerationBuilding && generation.State != store.VisualGenerationActive) {
			return err
		}
		// Consent (bound to the manifest in force) gates every scheduled
		// pass; before it is recorded the schedule is a silent no-op.
		if consentErr := requireVisualConsent(ctx, vf); consentErr != nil {
			return nil //nolint:nilerr // missing consent is an expected idle state, not a job failure
		}
		return runVisualPass(ctx, vf,
			newOperationPassScope("scheduled:visual", operations.TriggerScheduled))
	}
	if cfg.Vector.Multimodal.Schedule.RunAfterSync {
		sched.SetVisualPostSyncJob(runScheduled)
	}
	schedule := cfg.Vector.Multimodal.Schedule.Cron
	if schedule == "" {
		return nil
	}
	return sched.AddJob(scheduler.Job{Name: "multimodal-attachments", Schedule: schedule, Run: func(ctx context.Context) error {
		return runScheduled(ctx)
	}})
}

// checkVectorIndexFreshness runs the same generation-vs-configured
// fingerprint check the query path uses (vector.ResolveActiveForFingerprint)
// once init completes, so the daemon's reported status reflects a stale
// index instead of claiming "ready" while every vector search 503s with
// index_stale. Only ErrIndexStale flips the status: a still-building or
// not-yet-configured index (ErrIndexBuilding/ErrNotEnabled) and transient
// backend errors leave the freshly-installed "ready" status untouched, since
// those are not the "index does not match the configured embedding settings" failure this
// status exists to expose.
func checkVectorIndexFreshness(ctx context.Context, apiServer *api.Server, vf *vectorFeatures) {
	_, err := resolveActiveGeneration(ctx, vf.Backend, vf.Cfg.GenerationFingerprint())
	if !errors.Is(err, vector.ErrIndexStale) {
		return
	}
	detail := err.Error() + "; if this is a one-off account-scoped generation, set matching [vector.embed.scope] accounts and restart the daemon; otherwise run `msgvault embeddings build --full-rebuild` to rebuild"
	logger.Warn("vector index does not match configured embedding settings; vector search unavailable",
		"detail", detail)
	apiServer.SetVectorStale(detail)
}

// embedScopeDriftDetail is the operator-facing explanation for a latched
// scope-drift stale: what changed and how to recover.
func embedScopeDriftDetail(resolved, initialized vector.BuildScope) string {
	return fmt.Sprintf(
		"the configured embedding scope now resolves to %q but vector search was initialized with %q; restart the daemon to reinitialize, then run `msgvault embeddings build --full-rebuild` if the new scope is intended",
		resolved.Fingerprint(), initialized.Fingerprint())
}

// embedScopeDriftCheck is the daemon's preflight scope-drift check: it
// re-resolves the durable account configuration against the open store and
// reports a stale-latching detail when the scope has drifted OR become
// deterministically unresolvable (a configured account removed or
// ambiguous). Transient resolution failures (a busy database) pass through
// as errors so the preflight logs and retries them instead of latching.
func embedScopeDriftCheck(s *store.Store, initialized vector.BuildScope) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		resolved, err := configuredEmbedBuildScope(s)
		if errors.Is(err, vector.ErrScopeUnresolvable) {
			return err.Error() + "; fix [vector.embed.scope] accounts and restart the daemon", nil
		}
		if err != nil {
			return "", err
		}
		if resolved.Fingerprint() == initialized.Fingerprint() {
			return "", nil
		}
		return embedScopeDriftDetail(resolved, initialized), nil
	}
}

// registerEmbedJob wires the embed worker into the scheduler (cron-driven
// plus optional post-sync hook). Extracted from runServe so the background
// vector init can register it once the backend is ready.
type embedJobRegistrar interface {
	SetEmbedJob(job *scheduler.EmbedJob, schedule string, runAfterSync bool) error
}

type documentVectorJobRegistrar interface {
	SetDocumentVectorJob(job func(context.Context) error, schedule string, runAfterSync bool) error
}

func registerDocumentVectorJob(sched documentVectorJobRegistrar, vf *vectorFeatures, st *store.Store) error {
	if cfg == nil || !cfg.Attachments.Documents.Index.Embeddings.Enabled || vf == nil || vf.DocumentBackend == nil {
		return nil
	}
	limit := vf.Cfg.Embeddings.BatchSize
	if limit < 1 {
		limit = defaultDocumentVectorOperationLimit
	}
	if limit > 1000 {
		limit = 1000
	}
	job := func(ctx context.Context) error {
		return runScheduledDocumentVectorGeneration(ctx, st, vf, limit)
	}
	if err := sched.SetDocumentVectorJob(job, cfg.Vector.Embed.Schedule.Cron, cfg.Vector.Embed.Schedule.RunAfterSync); err != nil {
		return fmt.Errorf("register document vector job: %w", err)
	}
	logger.Info("document vectors scheduled", "cron", cfg.Vector.Embed.Schedule.Cron,
		"run_after_sync", cfg.Vector.Embed.Schedule.RunAfterSync)
	return nil
}

func registerEmbedJob(sched embedJobRegistrar, vf *vectorFeatures, s *store.Store, apiServer *api.Server) error {
	embedJob := newSchedulerEmbedJob(vf, s)
	embedJob.ResolveBuildScope = func() (vector.BuildScope, error) {
		return configuredEmbedBuildScope(s)
	}
	// Scope drift also has to reach searchers, not just the log: the
	// installed components still match the active generation's
	// fingerprint, so without this latch the API keeps reporting
	// "ready" while serving the wrongly-scoped index. Tests may register
	// without an API server.
	if apiServer != nil {
		embedJob.OnScopeDrift = apiServer.SetVectorScopeDrift
	}
	schedule := cfg.Vector.Embed.Schedule.Cron
	if err := sched.SetEmbedJob(embedJob, schedule, cfg.Vector.Embed.Schedule.RunAfterSync); err != nil {
		return fmt.Errorf("register embed job: %w", err)
	}
	logger.Info("embed scheduled",
		"cron", schedule,
		"run_after_sync", cfg.Vector.Embed.Schedule.RunAfterSync,
	)
	return nil
}

func newSchedulerEmbedJob(vf *vectorFeatures, s *store.Store) *scheduler.EmbedJob {
	return &scheduler.EmbedJob{
		Worker:      vf.Runner,
		Backend:     vf.Backend,
		Store:       s,
		Convergence: vf.Convergence,
		SequenceBoundActivation: vf.Cfg.Embeddings.EffectiveAPIFormat() ==
			vector.APIFormatVoyageContextual,
		Fingerprint:      vf.Cfg.GenerationFingerprint(),
		BackstopInterval: vf.Cfg.Embed.BackstopInterval,
		BuildScope:       vf.Cfg.Embed.Scope.BuildScope(),
		Log:              logger,
	}
}
