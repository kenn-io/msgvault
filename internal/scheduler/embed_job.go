package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

// defaultBackstopInterval is how often the daemon embed job runs a full
// watermark-ignoring backstop pass when BackstopInterval is left zero.
const defaultBackstopInterval = 24 * time.Hour

// EmbedRunner is the subset of *embed.Worker that EmbedJob needs.
// Tests satisfy it with a fake.
type EmbedRunner interface {
	RunOnce(ctx context.Context, gen vector.GenerationID) (embed.RunResult, error)
	// RunBackstop performs a full-scan pass that ignores the per-generation
	// watermark, recovering below-watermark stragglers (repair-encoding
	// resets, transient errors, crashes). Idempotent: already-covered rows
	// are skipped by the scan predicate.
	RunBackstop(ctx context.Context, gen vector.GenerationID) (embed.RunResult, error)
	ReclaimStale(ctx context.Context) (int, error)
}

// EmbedCoverage is the subset of *store.Store the activation gate needs:
// the count of live messages still needing embedding for a generation,
// read from the main DB. Tests satisfy it with a fake.
type EmbedCoverage interface {
	MissingCount(ctx context.Context, activeGen int64) (int64, error)
}

type ScopedEmbedCoverage interface {
	MissingCountScoped(ctx context.Context, activeGen int64, messageTypes []string) (int64, error)
}

// Compile-time check that the production worker satisfies EmbedRunner.
var _ EmbedRunner = (*embed.Worker)(nil)

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
	// daemon restart resets it, so the first tick after a restart runs one extra
	// backstop per generation — harmless because RunBackstop is idempotent.
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

	if _, err := j.Worker.ReclaimStale(ctx); err != nil {
		log.Warn("embed reclaim failed", "error", err)
	}

	target, isBuilding, ok := j.pickTarget(ctx, log)
	if !ok {
		return
	}

	res, err := j.Worker.RunOnce(ctx, target)
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

	// Periodic full backstop (~once per BackstopInterval). RunOnce only
	// scans forward from the per-gen watermark, so below-watermark
	// stragglers (repair-encoding NULL resets, transient errors, crashes)
	// are otherwise only recovered by the manual `embeddings build
	// --backstop`. Weaving it into this existing job gives `msgvault serve`
	// users that recovery for free. The backstop reuses the same
	// scan/embed/stamp path with the cursor pinned at 0, in modest
	// non-locking batches, and is idempotent (already-covered rows are
	// skipped) so it never re-embeds stamped messages.
	j.maybeRunBackstop(ctx, target, log)

	if !isBuilding {
		return
	}
	// Activation gate: only flip the building generation to active when
	// coverage is complete (no live message still needs embedding for it).
	// Transient embed failures that the worker later recovers from must
	// not block activation, but an incompletely-covered generation must
	// not auto-activate either (it would expose an incomplete index).
	//
	// This check + ActivateGeneration is intentionally non-atomic on
	// SQLite (cross-DB): a message synced between the coverage read and
	// the activation call leaves embed_gen NULL on the now-active
	// generation. The next worker tick (and the full-scan backstop) picks
	// it up via the active-generation scan, so the system reaches
	// consistency on the next run rather than blocking activation forever
	// on a moving target. The backend re-asserts the gate (atomically on
	// PG; via a Go pre-check on SQLite) inside ActivateGeneration.
	if j.Store == nil {
		log.Debug("embed: building covered but Store not wired; skipping auto-activation",
			"gen", target)
		return
	}
	missing, err := j.missingCount(ctx, target)
	if err != nil {
		log.Warn("embed: coverage count after run failed", "gen", target, "error", err)
		return
	}
	if missing > 0 {
		log.Info("embed: building generation still has messages needing embedding; will retry next tick",
			"gen", target, "remaining", missing)
		return
	}
	// force=false: the missing==0 check above is the scheduler's gate,
	// and the backend re-asserts it inside ActivateGeneration.
	if err := j.Backend.ActivateGeneration(ctx, target, false); err != nil {
		log.Warn("embed: activation failed", "gen", target, "error", err)
		return
	}
	log.Info("embed: building generation activated", "gen", target)
}

func (j *EmbedJob) missingCount(ctx context.Context, target vector.GenerationID) (int64, error) {
	scope := vector.NewBuildScope(j.BuildScope.MessageTypes)
	if scope.IsEmpty() {
		return j.Store.MissingCount(ctx, int64(target))
	}
	if scoped, ok := j.Store.(ScopedEmbedCoverage); ok {
		return scoped.MissingCountScoped(ctx, int64(target), scope.MessageTypes)
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
func (j *EmbedJob) maybeRunBackstop(ctx context.Context, gen vector.GenerationID, log *slog.Logger) {
	interval := j.BackstopInterval
	if interval < 0 {
		return // explicitly disabled
	}
	if interval == 0 {
		interval = defaultBackstopInterval
	}
	now := time.Now
	if j.Now != nil {
		now = j.Now
	}
	t := now()
	// First run for this generation (no recorded time) always runs a backstop;
	// thereafter gate by the interval against this generation's own last run.
	if last, ok := j.lastBackstop[gen]; ok && t.Sub(last) < interval {
		return
	}
	res, err := j.Worker.RunBackstop(ctx, gen)
	if err != nil {
		log.Warn("embed backstop failed", "gen", gen, "error", err)
		// Do not advance lastBackstop on failure so the next tick retries.
		return
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
