package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"

	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

// EmbedRunner is the subset of *embed.Worker that EmbedJob needs.
// Tests satisfy it with a fake.
type EmbedRunner interface {
	RunOnce(ctx context.Context, gen vector.GenerationID) (embed.RunResult, error)
	ReclaimStale(ctx context.Context) (int, error)
}

// Compile-time check that the production worker satisfies EmbedRunner.
var _ EmbedRunner = (*embed.Worker)(nil)

// EmbedJob runs the vector-embedding worker. Each invocation prefers
// an in-flight rebuild for the configured fingerprint over the
// existing active generation, drains its queue via RunOnce, and
// activates it once pending hits zero. This mirrors the CLI
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

	// VectorsDB is the vectors.db handle, used to count remaining
	// pending_embeddings for activation gating. May be nil; in that
	// case the daemon will not auto-activate building generations.
	VectorsDB *sql.DB

	// Rebind translates ?-placeholders to the driver's native form for
	// queries this job issues directly against VectorsDB (pendingCount).
	// nil is treated as the identity (used by SQLite); the PostgreSQL
	// serve path must wire in (&store.PostgreSQLDialect{}).Rebind so the
	// activation-gate count runs on pgx — a bare ? is rejected by the
	// pgx driver.
	Rebind func(string) string

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

	// Guard against the CreateGeneration crash window: if a prior
	// rebuild inserted the building row but died before committing
	// the initial seed, the pending queue is empty and the daemon
	// would happily "drain" it and activate an unseeded generation.
	// EnsureSeeded is idempotent on already-seeded generations, so
	// calling it on every resume is cheap and safe.
	if isBuilding {
		if err := j.Backend.EnsureSeeded(ctx, target); err != nil {
			log.Warn("embed: ensure seeded failed; leaving building generation for CLI to resolve",
				"gen", target, "error", err)
			return
		}
	}

	// PostgreSQL self-heal: sync persists a message row and enqueues it
	// for embedding in two separate transactions, so a failed enqueue
	// leaves the message committed but missing from pending_embeddings,
	// and the next incremental sync skips it as already-ingested. Before
	// draining, re-derive target's queue from the live messages table so
	// any such gap is recovered. Idempotent and cheap when nothing is
	// missing (ON CONFLICT drops every would-be duplicate). Only the
	// pgvector backend implements CatchUpPending; SQLite is unaffected
	// (its full-rebuild path already recovers missed IDs).
	if cu, ok := j.Backend.(interface {
		CatchUpPending(ctx context.Context, gen vector.GenerationID) (int64, error)
	}); ok {
		if caught, err := cu.CatchUpPending(ctx, target); err != nil {
			log.Warn("embed: catch-up of missed enqueues failed; continuing with current queue",
				"gen", target, "error", err)
		} else if caught > 0 {
			log.Info("embed: caught up messages missing from the embed queue",
				"gen", target, "count", caught)
		}
	}

	res, err := j.Worker.RunOnce(ctx, target)
	if err != nil {
		log.Warn("embed run failed", "gen", target, "error", err)
		return
	}
	log.Info("embed run complete",
		"gen", target,
		"building", isBuilding,
		"claimed", res.Claimed,
		"succeeded", res.Succeeded,
		"failed", res.Failed,
		"truncated", res.Truncated,
	)

	if !isBuilding {
		return
	}
	// Activation gate: only flip the building generation to active
	// when the queue has fully drained for it. Transient embed
	// failures that the worker later recovers from must not block
	// activation, but a generation with pending rows must not
	// auto-activate either (it would expose an incomplete index).
	//
	// This check + ActivateGeneration is intentionally non-atomic.
	// If sync.EnqueueMessages commits a new pending row between the
	// pendingCount read and the activation call, activation still
	// succeeds and the new row stays bound to the (now-active)
	// generation. The next worker tick picks it up via the active-
	// generation top-up path, so the system reaches consistency on
	// the next run rather than blocking activation forever on a
	// moving target. This is by design — at steady state every
	// active generation has incremental rows showing up between
	// runs, so the activation gate must not require a snapshot.
	if j.VectorsDB == nil {
		log.Debug("embed: building drained but VectorsDB not wired; skipping auto-activation",
			"gen", target)
		return
	}
	remaining, err := j.pendingCount(ctx, target)
	if err != nil {
		log.Warn("embed: count pending after run failed", "gen", target, "error", err)
		return
	}
	if remaining > 0 {
		log.Info("embed: building generation still has pending rows; will retry next tick",
			"gen", target, "remaining", remaining)
		return
	}
	// force=false: the pendingCount==0 check above is the scheduler's gate,
	// and the backend re-asserts it atomically inside the activation tx.
	if err := j.Backend.ActivateGeneration(ctx, target, false); err != nil {
		log.Warn("embed: activation failed", "gen", target, "error", err)
		return
	}
	log.Info("embed: building generation activated", "gen", target)
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

// pendingCount returns the number of pending_embeddings rows for gen.
// Used by the activation gate.
func (j *EmbedJob) pendingCount(ctx context.Context, gen vector.GenerationID) (int, error) {
	rebind := j.Rebind
	if rebind == nil {
		rebind = func(q string) string { return q }
	}
	var n int
	if err := j.VectorsDB.QueryRowContext(ctx,
		rebind(`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`), int64(gen)).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
