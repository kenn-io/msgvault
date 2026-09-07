package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

// defaultBackstopInterval is how often the daemon embed job runs a full
// watermark-ignoring backstop pass when BackstopInterval is left zero.
const defaultBackstopInterval = 24 * time.Hour

// EmbedRunner is the subset of *embed.Worker that EmbedJob needs.
// Tests satisfy it with a fake.
type EmbedRunner interface {
	RunOnce(ctx context.Context, gen vector.GenerationID, scope operations.PassScope) (embed.RunResult, error)
	// RunBackstop performs a full-scan pass that ignores the per-generation
	// watermark, recovering below-watermark stragglers (repair-encoding
	// resets, transient errors, crashes). Idempotent: already-covered rows
	// are skipped by the scan predicate.
	RunBackstop(ctx context.Context, gen vector.GenerationID, scope operations.PassScope) (embed.RunResult, error)
	ReclaimStale(ctx context.Context) (int, error)
}

type activePersonRunner interface {
	RunPersonsOnce(ctx context.Context, gen vector.GenerationID, scope operations.PassScope) (embed.RunResult, error)
}

// ConvergenceResult is the complete activation gate for one generation.
// Every mode requires exact message and curated-person coverage. Contextual
// modes additionally require the journal and reconciliation dimensions;
// legacy modes set those dimensions complete by construction.
type ConvergenceResult struct {
	MessageCoverageComplete  bool
	MessageCoverageMissing   int64
	PersonCoverageComplete   bool
	PersonCoverageMismatched int64
	PersonCoverageRejected   int64
	LatestJournalSequence    int64
	ConsumedJournalSequence  int64
	ReconciliationComplete   bool
}

// Complete reports whether a generation is safe to activate.
func (r ConvergenceResult) Complete() bool {
	return r.MessageCoverageComplete &&
		r.PersonCoverageComplete &&
		r.PersonCoverageRejected == 0 &&
		r.LatestJournalSequence == r.ConsumedJournalSequence &&
		r.ReconciliationComplete
}

// ConvergenceChecker reads the durable source and vector state used by every
// non-force activation path.
type ConvergenceChecker interface {
	CheckConvergence(ctx context.Context, gen vector.GenerationID) (ConvergenceResult, error)
}

// EmbedCoverage is the subset of *store.Store the activation gate needs:
// the count of live messages still needing embedding for a generation,
// read from the main DB. Tests satisfy it with a fake.
type EmbedCoverage interface {
	MissingCount(ctx context.Context, activeGen int64) (int64, error)
}

type ScopedEmbedCoverage interface {
	MissingCountScoped(ctx context.Context, activeGen int64, messageTypes []string, sourceIDs []int64) (int64, error)
}

// Compile-time check that the production worker satisfies EmbedRunner.
var _ EmbedRunner = (*embed.Worker)(nil)
var _ EmbedRunner = (*embed.ContextWorker)(nil)

// EmbedJob runs the vector-embedding worker. Each invocation prefers
// an in-flight rebuild for the configured fingerprint over the
// existing active generation, embeds its outstanding messages via
// RunOnce, and activates once coverage is complete (no live message
// still needs embedding). This mirrors the CLI
// (cmd/msgvault/cmd/embed_vector.go pickEmbedGeneration) so a
// daemon-only deployment can complete a `--full-rebuild` started by
// the operator. Without the building-first preference, a daemon
// would keep topping up the old active index forever and leave the
// new generation stuck in `building`.
//
// The zero value is usable; only Worker and Backend are required. Run
// is safe to call from multiple goroutines: a run that starts while
// another is already in flight returns immediately (drop-not-queue —
// the next tick will pick up whatever was missed).
type EmbedJob struct {
	Worker  EmbedRunner
	Backend vector.Backend
	Log     *slog.Logger

	// Store provides the main-DB coverage count used for activation
	// gating (how many live messages still need embedding for the
	// building generation). May be nil; in that case the daemon will not
	// auto-activate building generations.
	Store EmbedCoverage

	// Convergence is the shared activation gate selected by the configured
	// embedding format. When nil, the legacy Store coverage gate remains in
	// effect for compatibility with existing callers.
	Convergence ConvergenceChecker

	// SequenceBoundActivation requires contextual activation to atomically
	// verify the source journal sequence. Legacy OpenAI-format jobs leave it
	// false and use normal generation activation after convergence.
	SequenceBoundActivation bool

	// Fingerprint is the configured generation fingerprint (typically
	// vector.Config.GenerationFingerprint() — "model:dim:preprocess").
	// When set, a building OR active generation whose fingerprint
	// differs is left alone: the CLI is the only entry point that can
	// resolve a mismatch (`embeddings build --full-rebuild` or retire).
	// When empty, the daemon falls back to "any building generation"
	// for building gens and "the active generation as-is" for active —
	// see pickTarget for why empty-fingerprint plus a present building
	// is still refused.
	Fingerprint string

	// BackstopInterval controls how often Run also performs a full
	// watermark-ignoring backstop pass (RunBackstop) in addition to the
	// per-tick RunOnce. The backstop recovers below-watermark stragglers
	// (repair-encoding NULL resets, transient errors, crashes) that the
	// incremental scan skips. Zero uses defaultBackstopInterval (24h).
	// A negative value disables the auto-backstop entirely.
	BackstopInterval time.Duration

	// BuildScope limits coverage checks to the same message universe the
	// worker scans for this generation. Empty means the full live corpus.
	BuildScope vector.BuildScope

	// ResolveBuildScope re-resolves the durable embedding scope immediately
	// before each scheduled run. If it differs from BuildScope, Run fails
	// closed: the worker and backend were initialized with the old scope and
	// must be reinitialized before any embedding can resume. This prevents a
	// reused database source ID from being treated as the formerly configured
	// account.
	ResolveBuildScope func() (vector.BuildScope, error)

	// OnScopeDrift is invoked (once per Run that detects it) when
	// ResolveBuildScope returns a scope different from BuildScope, or fails
	// deterministically (vector.ErrScopeUnresolvable — a configured account
	// removed or ambiguous). The daemon wires this to the API server's
	// stale latch so searches stop reporting "ready" against components
	// initialized with the old scope — the active generation still matches
	// the startup fingerprint, so without this signal nothing else would
	// flag the drift. Transient resolution failures do not trigger it.
	// May be nil.
	OnScopeDrift func(detail string)

	// Now returns the current time; overridable in tests to drive the
	// backstop interval deterministically. nil uses time.Now.
	Now func() time.Time

	// lastBackstop maps each generation to the time its most recent backstop
	// ran, used to gate the next one by BackstopInterval. Keyed per generation
	// so that switching the target (e.g. the active gen recently backstopped,
	// then a building gen selected) does not let one generation's recent
	// backstop throttle a different generation's first backstop — which would
	// otherwise delay recovery of a below-watermark straggler and block
	// auto-activation for up to BackstopInterval. In-memory (not persisted): a
	// daemon restart resets it, so the first eligible active or incomplete-build
	// tick runs one extra backstop — harmless because RunBackstop is idempotent.
	// Read/written only while the running lock is held, so it needs no separate
	// guard. Lazily allocated in maybeRunBackstop so the zero value stays usable.
	// Growth is negligible (a handful of generations over the tool's life), so
	// no pruning is needed.
	lastBackstop map[vector.GenerationID]time.Time

	// running guards against overlapping Run calls (cron fires while a
	// post-sync hook is still draining, etc). sync.Mutex.TryLock gives
	// us "skip if busy" without serializing a queue of waiters.
	running sync.Mutex
}

// Run executes one embed cycle. Safe to call from cron or as a
// post-sync hook. Returns immediately when vector search has no
// pending work (no active and no matching building generation), or
// when another Run is already in flight.
func (j *EmbedJob) Run(ctx context.Context) {
	if j == nil || j.Worker == nil || j.Backend == nil {
		return
	}
	log := j.Log
	if log == nil {
		log = slog.Default()
	}

	if !j.running.TryLock() {
		log.Debug("embed run skipped: previous run still in flight")
		return
	}
	defer j.running.Unlock()
	occurrence := j.now().UTC()

	if j.ResolveBuildScope != nil {
		resolved, err := j.ResolveBuildScope()
		if err != nil {
			log.Error("embed run skipped: configured scope could not be resolved", "error", err)
			// A deterministic failure (a configured account removed or
			// ambiguous — vector.ErrScopeUnresolvable) cannot heal on retry
			// and means the initialized scope no longer matches the
			// configuration, so it must latch searches stale like a scope
			// change. Transient failures only skip the run.
			if j.OnScopeDrift != nil && errors.Is(err, vector.ErrScopeUnresolvable) {
				j.OnScopeDrift(err.Error() + "; fix [vector.embed.scope] accounts and restart the daemon")
			}
			return
		}
		configured := vector.NewBuildScope(j.BuildScope.MessageTypes, j.BuildScope.SourceIDs)
		if resolved.Fingerprint() != configured.Fingerprint() {
			log.Error("embed run skipped: configured scope changed; reinitialize vector features and rebuild before embedding",
				"configured_scope", resolved.Fingerprint(),
				"initialized_scope", configured.Fingerprint())
			if j.OnScopeDrift != nil {
				j.OnScopeDrift(fmt.Sprintf(
					"the configured embedding scope now resolves to %q but vector search was initialized with %q; restart the daemon to reinitialize, then run `msgvault embeddings build --full-rebuild` if the new scope is intended",
					resolved.Fingerprint(), configured.Fingerprint()))
			}
			return
		}
	}

	if _, err := j.Worker.ReclaimStale(ctx); err != nil {
		if jobctx.YieldedToWaiter(ctx) {
			return
		}
		log.Warn("embed reclaim failed", "error", err)
	}

	j.maintainActivePeopleDuringBuild(ctx, log, occurrence)
	if jobctx.YieldedToWaiter(ctx) {
		return
	}

	target, isBuilding, ok := j.pickTarget(ctx, log)
	if !ok {
		return
	}

	res, err := j.Worker.RunOnce(ctx, target, scheduledEmbeddingPassScope(occurrence, target, "forward"))
	// The scheduler yield cause takes precedence over an operation error:
	// drivers can return unwrapped errors after cancellation, so the cause at
	// this operation boundary is authoritative.
	if jobctx.YieldedToWaiter(ctx) {
		return
	}
	if generationErr, ok := errors.AsType[*embed.GenerationRunError](err); ok {
		if generationErr.Person != nil {
			log.Warn("person embedding run failed", "gen", target, "error", generationErr.Person)
		}
		err = generationErr.Message
	}
	if err != nil {
		log.Warn("embed run failed", "gen", target, "error", err)
		return
	}
	log.Info("embed run complete",
		"gen", target,
		"building", isBuilding,
		"scanned", res.Claimed,
		"succeeded", res.Succeeded,
		"failed", res.Failed,
		"truncated", res.Truncated,
	)
	// A contextual RunOnce can finish the durable journal, reconciliation,
	// and coverage gates. Check convergence once here: a completed build
	// activates before the periodic backstop can reset its reconciliation
	// cursor and start another archive scan. A transient check failure falls
	// through so it cannot delay the backstop's straggler recovery.
	var contextualState *ConvergenceResult
	if isBuilding && j.Convergence != nil {
		state, err := j.Convergence.CheckConvergence(ctx, target)
		if jobctx.YieldedToWaiter(ctx) {
			return
		}
		if err != nil {
			log.Warn("embed: convergence check after run failed", "gen", target, "error", err)
		} else {
			contextualState = &state
			if state.Complete() {
				j.activateBuilding(ctx, target, &state, log)
				return
			}
		}
	}

	// Periodic full backstop (~once per BackstopInterval). RunOnce only
	// scans forward from the per-gen watermark, so below-watermark
	// stragglers (repair-encoding NULL resets, transient errors, crashes)
	// are otherwise only recovered by the manual `embeddings build
	// --backstop`. Weaving it into this existing job gives `msgvault serve`
	// users that recovery for free. The backstop reuses the same
	// scan/embed/stamp path with the cursor pinned at 0, in modest
	// non-locking batches, and is idempotent (already-covered rows are
	// skipped) so it never re-embeds stamped messages.
	backstopRan := j.maybeRunBackstop(ctx, target, log, occurrence)
	if jobctx.YieldedToWaiter(ctx) {
		return
	}

	if !isBuilding {
		return
	}
	// Activation gate: only flip the building generation to active when
	// coverage is complete (no live message still needs embedding for it).
	// Transient embed failures that the worker later recovers from must
	// not block activation, but an incompletely-covered generation must
	// not auto-activate either (it would expose an incomplete index).
	//
	if j.Convergence != nil {
		// The archive-wide convergence check is not free; re-run it only when
		// the backstop may have changed durable state or the pre-backstop
		// check did not produce a result.
		if backstopRan || contextualState == nil {
			state, err := j.Convergence.CheckConvergence(ctx, target)
			if jobctx.YieldedToWaiter(ctx) {
				return
			}
			if err != nil {
				log.Warn("embed: convergence check after run failed", "gen", target, "error", err)
				return
			}
			contextualState = &state
		}
		if !contextualState.Complete() {
			state := *contextualState
			log.Info("embed: building generation has not converged; will retry next tick",
				"gen", target,
				"message_coverage_complete", state.MessageCoverageComplete,
				"message_coverage_missing", state.MessageCoverageMissing,
				"person_coverage_complete", state.PersonCoverageComplete,
				"person_coverage_mismatched", state.PersonCoverageMismatched,
				"person_coverage_rejected", state.PersonCoverageRejected,
				"latest_journal_sequence", state.LatestJournalSequence,
				"consumed_journal_sequence", state.ConsumedJournalSequence,
				"reconciliation_complete", state.ReconciliationComplete)
			return
		}
	} else if j.Store == nil {
		log.Debug("embed: building covered but Store not wired; skipping auto-activation",
			"gen", target)
		return
	} else {
		missing, err := j.missingCount(ctx, target)
		if jobctx.YieldedToWaiter(ctx) {
			return
		}
		if err != nil {
			log.Warn("embed: coverage count after run failed", "gen", target, "error", err)
			return
		}
		if missing > 0 {
			log.Info("embed: building generation still has messages needing embedding; will retry next tick",
				"gen", target, "remaining", missing)
			return
		}
	}
	j.activateBuilding(ctx, target, contextualState, log)
}

func (j *EmbedJob) maintainActivePeopleDuringBuild(
	ctx context.Context, log *slog.Logger, occurrence time.Time,
) {
	runner, ok := j.Worker.(activePersonRunner)
	if !ok || j.Fingerprint == "" {
		return
	}
	building, err := j.Backend.BuildingGeneration(ctx)
	if err != nil || building == nil || jobctx.YieldedToWaiter(ctx) {
		return
	}
	active, err := j.Backend.ActiveGeneration(ctx)
	if errors.Is(err, vector.ErrNoActiveGeneration) || jobctx.YieldedToWaiter(ctx) {
		return
	}
	if err != nil {
		log.Warn("active person embedding generation lookup failed", "error", err)
		return
	}
	if active.Fingerprint != j.Fingerprint {
		return
	}
	res, err := runner.RunPersonsOnce(
		ctx, active.ID, scheduledEmbeddingPassScope(occurrence, active.ID, "active-people"),
	)
	if jobctx.YieldedToWaiter(ctx) {
		return
	}
	if err != nil {
		log.Warn("active person embedding run failed", "gen", active.ID, "error", err)
		return
	}
	log.Info("active person embedding run complete",
		"gen", active.ID,
		"scanned", res.Claimed,
		"succeeded", res.Succeeded,
		"failed", res.Failed,
		"truncated", res.Truncated,
	)
}

func (j *EmbedJob) activateBuilding(
	ctx context.Context, target vector.GenerationID, contextualState *ConvergenceResult, log *slog.Logger,
) {
	if jobctx.YieldedToWaiter(ctx) {
		return
	}
	var activateErr error
	if j.SequenceBoundActivation {
		if contextualState == nil {
			log.Warn("embed: contextual activation lacks convergence state", "gen", target)
			return
		}
		activator, ok := j.Backend.(vector.ConvergedGenerationActivator)
		if !ok {
			log.Warn("embed: contextual backend lacks sequence-bound activation", "gen", target)
			return
		}
		activateErr = activator.ActivateGenerationIfConverged(ctx, target, contextualState.LatestJournalSequence)
	} else {
		activateErr = j.Backend.ActivateGeneration(ctx, target, false)
	}
	if activateErr != nil {
		if jobctx.YieldedToWaiter(ctx) {
			return
		}
		log.Warn("embed: activation failed", "gen", target, "error", activateErr)
		return
	}
	if jobctx.YieldedToWaiter(ctx) {
		return
	}
	log.Info("embed: building generation activated", "gen", target)
}

func (j *EmbedJob) missingCount(ctx context.Context, target vector.GenerationID) (int64, error) {
	scope := vector.NewBuildScope(j.BuildScope.MessageTypes, j.BuildScope.SourceIDs)
	if scope.IsEmpty() {
		return j.Store.MissingCount(ctx, int64(target))
	}
	if scoped, ok := j.Store.(ScopedEmbedCoverage); ok {
		return scoped.MissingCountScoped(ctx, int64(target), scope.MessageTypes, scope.SourceIDs)
	}
	return 0, errors.New("embed coverage store does not support scoped missing counts")
}

// maybeRunBackstop runs a full watermark-ignoring backstop pass on gen when
// BackstopInterval has elapsed since this generation's last one, then records
// the time. The throttle is keyed per generation so a recent backstop of one
// generation cannot suppress a different generation's first backstop. Called
// with the running lock held (from Run), so lastBackstop needs no separate
// guard. A negative BackstopInterval disables it; zero defaults to 24h. A
// backstop failure is logged, not fatal — the next interval retries.
// maybeRunBackstop reports whether it invoked RunBackstop, so the caller
// knows durable state may have changed since any earlier convergence check.
func (j *EmbedJob) maybeRunBackstop(
	ctx context.Context, gen vector.GenerationID, log *slog.Logger, occurrence time.Time,
) bool {
	interval := j.BackstopInterval
	if interval < 0 {
		return false // explicitly disabled
	}
	if interval == 0 {
		interval = defaultBackstopInterval
	}
	t := occurrence
	// First run for this generation (no recorded time) always runs a backstop;
	// thereafter gate by the interval against this generation's own last run.
	if last, ok := j.lastBackstop[gen]; ok && t.Sub(last) < interval {
		return false
	}
	res, err := j.Worker.RunBackstop(ctx, gen, scheduledEmbeddingPassScope(occurrence, gen, "backstop"))
	if jobctx.YieldedToWaiter(ctx) {
		return true
	}
	if generationErr, ok := errors.AsType[*embed.GenerationRunError](err); ok {
		if generationErr.Person != nil {
			log.Warn("person embedding backstop failed", "gen", gen, "error", generationErr.Person)
		}
		err = generationErr.Message
	}
	if err != nil {
		log.Warn("embed backstop failed", "gen", gen, "error", err)
		// Do not advance lastBackstop on failure so the next tick retries.
		return true
	}
	if j.lastBackstop == nil {
		j.lastBackstop = make(map[vector.GenerationID]time.Time)
	}
	j.lastBackstop[gen] = t
	log.Info("embed backstop complete",
		"gen", gen,
		"scanned", res.Claimed,
		"succeeded", res.Succeeded,
		"failed", res.Failed,
		"truncated", res.Truncated,
	)
	return true
}

func (j *EmbedJob) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

func scheduledEmbeddingPassScope(
	occurrence time.Time, gen vector.GenerationID, phase string,
) operations.PassScope {
	occurrence = occurrence.UTC()
	return operations.PassScope{
		Key:     fmt.Sprintf("scheduled:%s:g:%d:%s", occurrence.Format(time.RFC3339Nano), gen, phase),
		Trigger: operations.TriggerScheduled, StartedAt: occurrence,
	}
}

// pickTarget returns the generation to drain plus an isBuilding flag
// for the activation gate. Order:
//
//  1. Building generation matching the configured fingerprint (or any
//     building generation when Fingerprint is empty) — drain so it
//     can activate. Building takes precedence over active even when
//     active matches, because a stranded build is the bigger problem.
//  2. Mismatched building generation — log and bail. Resolution
//     requires the CLI (`msgvault embeddings build --full-rebuild` or retire),
//     not the daemon.
//  3. Active generation whose fingerprint matches config — incremental
//     top-up. A mismatched active fingerprint is treated the same as a
//     mismatched building: log and bail. Topping it up would let the
//     daemon embed new messages under the current preprocessing policy
//     into an index whose existing vectors used a different policy,
//     silently mixing two embedding spaces in one generation.
//
// The bool is false when there's nothing to do or a lookup error
// occurred (already logged); the caller should return.
func (j *EmbedJob) pickTarget(ctx context.Context, log *slog.Logger) (vector.GenerationID, bool, bool) {
	bg, bgErr := j.Backend.BuildingGeneration(ctx)
	if jobctx.YieldedToWaiter(ctx) {
		return 0, false, false
	}
	if bgErr != nil {
		log.Warn("embed: building generation lookup failed", "error", bgErr)
		return 0, false, false
	}
	if bg != nil {
		if j.Fingerprint == "" {
			// Without a configured fingerprint we cannot tell
			// whether this building generation matches the model
			// the daemon is supposed to be using. Draining (and
			// thus auto-activating) it could silently swap the
			// production index to a different model. Refuse;
			// resolution requires the CLI, where pickEmbedGeneration
			// enforces a fingerprint match.
			log.Warn("embed: in-flight rebuild present but no configured fingerprint — refusing to drain",
				"building_fingerprint", bg.Fingerprint)
			return 0, false, false
		}
		if bg.Fingerprint != j.Fingerprint {
			log.Warn("embed: in-flight rebuild fingerprint differs from config — leaving for CLI to resolve",
				"building_fingerprint", bg.Fingerprint, "config_fingerprint", j.Fingerprint)
			return 0, false, false
		}
		return bg.ID, true, true
	}

	active, err := j.Backend.ActiveGeneration(ctx)
	if jobctx.YieldedToWaiter(ctx) {
		return 0, false, false
	}
	switch {
	case err == nil:
		if j.Fingerprint != "" && active.Fingerprint != j.Fingerprint {
			log.Warn("embed: active generation fingerprint differs from config — leaving for CLI to resolve",
				"active_fingerprint", active.Fingerprint, "config_fingerprint", j.Fingerprint)
			return 0, false, false
		}
		return active.ID, false, true
	case errors.Is(err, vector.ErrNoActiveGeneration):
		return 0, false, false // nothing to do
	default:
		log.Warn("embed: active generation lookup failed", "error", err)
		return 0, false, false
	}
}
