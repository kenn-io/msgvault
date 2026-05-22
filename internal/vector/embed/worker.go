package embed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/vector"
)

// EmbeddingClient is the subset of *Client used by Worker; allowing tests
// to inject a fake.
type EmbeddingClient interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

// WorkerDeps bundles the collaborators a Worker needs. Backend, VectorsDB,
// MainDB, and Client are required; the remaining fields have sensible
// defaults when zero: BatchSize defaults to 32, StaleThreshold is
// auto-derived from EmbedTimeout × EmbedMaxRetries with a 10-minute
// floor (see NewWorker), MaxConsecutiveFailures defaults to 5, Log
// defaults to slog.Default().
type WorkerDeps struct {
	Backend        vector.Backend
	VectorsDB      *sql.DB
	MainDB         *sql.DB
	Client         EmbeddingClient
	Preprocess     PreprocessConfig
	MaxInputChars  int
	BatchSize      int
	StaleThreshold time.Duration
	// EmbedTimeout and EmbedMaxRetries inform the StaleThreshold
	// auto-derivation: a single batch can legitimately stay claimed for
	// up to Timeout × MaxRetries (MaxRetries is the embed.Client's
	// total-attempts count, not retries-after-the-first) before the
	// worker gives up, so reclaim must wait longer than that to avoid
	// reclaiming live work. Both are read only when StaleThreshold is
	// zero; EmbedMaxRetries=0 is normalized to 3 to match
	// embed.NewClient's default — see derivedStaleThreshold.
	EmbedTimeout    time.Duration
	EmbedMaxRetries int
	// MaxConsecutiveFailures caps the number of consecutive batch
	// failures (embed error or upsert error) before RunOnce gives up
	// and returns an error. A successful batch resets the counter.
	// Default 5.
	MaxConsecutiveFailures int
	Log                    *slog.Logger
	// TotalPending is the queue depth at run start, used by a Progress
	// callback (if any) to report percent done and ETA. Zero disables
	// the denominator — Progress still fires but leaves ETA empty.
	TotalPending int
	// Progress, if non-nil, is called after queue rows are durably
	// completed, whether they produced embeddings or were intentionally
	// dropped as missing/empty/unembeddable. Done and BatchMsgs count
	// completed queue rows so they can be compared to TotalPending.
	// Callbacks run on the worker goroutine; rate-limit inside the
	// callback if output is expensive.
	Progress func(ProgressReport)
}

// ProgressReport captures RunOnce progress after a set of queue rows
// has been completed. Done and BatchMsgs count completed pending rows;
// BatchChars counts source chars for rows that actually embedded.
// BatchElapsed is end-to-end for that progress unit.
type ProgressReport struct {
	Done         int
	TotalPending int
	BatchMsgs    int
	BatchChars   int
	BatchElapsed time.Duration
	RunElapsed   time.Duration
}

// Worker drives one building generation from claimed pending rows to
// persisted embeddings. A single Worker is safe for sequential use; to
// parallelize, construct multiple workers that share the same Backend
// and DB handles.
type Worker struct {
	deps     WorkerDeps
	q        *Queue
	runStart time.Time // valid only during a RunOnce call
}

// NewWorker constructs a Worker, applying defaults for BatchSize (32),
// StaleThreshold (auto-derived; see derivedStaleThreshold),
// MaxConsecutiveFailures (5), and Log (slog.Default()).
func NewWorker(d WorkerDeps) *Worker {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.BatchSize == 0 {
		d.BatchSize = 32
	}
	if d.StaleThreshold == 0 {
		d.StaleThreshold = derivedStaleThreshold(d.EmbedTimeout, d.EmbedMaxRetries)
	}
	if d.MaxConsecutiveFailures == 0 {
		d.MaxConsecutiveFailures = 5
	}
	return &Worker{deps: d, q: NewQueue(d.VectorsDB)}
}

// derivedStaleThreshold picks a default StaleThreshold from the
// embedder's per-request timeout and retry count, with a 10-minute
// floor. A claim must outlive at least one full retry budget
// (timeout × attempts) — anything less risks ReclaimStale pulling
// rows out from under a still-running embed call, which would then
// race a concurrent worker on the same batch and leave stale
// Complete tokens. The 2× safety factor absorbs scheduler jitter
// and pre/post-call overhead. The floor preserves the historical
// default for the common case (Timeout=30s × 3 attempts = 3 minutes
// derived; floor wins).
//
// maxRetries here matches embed.Client's MaxRetries semantics: it is
// the TOTAL number of HTTP attempts (not retries-after-the-first).
// A zero value is normalized to 3 to mirror embed.NewClient's
// default. Without this, callers that set EmbedTimeout but leave
// EmbedMaxRetries at its zero value would derive a budget for a
// single attempt, while the client would actually try up to three
// times — and ReclaimStale could pull live claims out from under a
// retrying embed call.
func derivedStaleThreshold(timeout time.Duration, maxRetries int) time.Duration {
	const floor = 10 * time.Minute
	if timeout <= 0 {
		return floor
	}
	attempts := maxRetries
	if attempts == 0 {
		attempts = 3
	}
	if attempts < 1 {
		attempts = 1
	}
	derived := 2 * timeout * time.Duration(attempts)
	if derived < floor {
		return floor
	}
	return derived
}

// RunResult summarizes the outcome of RunOnce.
type RunResult struct {
	Claimed, Succeeded, Failed, Truncated int
}

// msgText is the per-message preprocessed input to the embedder, carried
// from fetch through to Chunk construction.
type msgText struct {
	ID    int64
	Text  string
	Chars int
	Trunc bool
}

// ReclaimStale releases claims older than StaleThreshold so crashed
// workers don't leave rows stuck. Call at startup before RunOnce.
// Returns the number of rows reclaimed.
func (w *Worker) ReclaimStale(ctx context.Context) (int, error) {
	n, err := w.q.ReclaimStale(ctx, w.deps.StaleThreshold)
	if err != nil {
		return 0, fmt.Errorf("reclaim stale: %w", err)
	}
	return n, nil
}

// RunOnce drains the queue for the given generation until empty,
// releasing claimed rows on embed or upsert error so another worker can
// retry them. Returns when pending is empty or ctx is cancelled.
//
// Returns an error when consecutive batch failures reach
// MaxConsecutiveFailures, so a persistently misconfigured embedder
// (bad credentials, unreachable endpoint) surfaces quickly instead of
// looping forever. A successful batch resets the failure counter.
func (w *Worker) RunOnce(ctx context.Context, gen vector.GenerationID) (RunResult, error) {
	var res RunResult
	consecutiveFailures := 0
	var lastErr error
	completedRows := 0
	w.runStart = time.Now()
	// orphanDrainErr/orphanDrainCount preserve the latest orphan-drain
	// failure across iterations so we can surface it on the empty-claim
	// exit. Without this, a Complete() failure on orphan rows would be
	// logged but invisible to the caller — and if those orphans were
	// the last queue rows, the next Claim returns empty and RunOnce
	// would falsely report a clean drain even though stuck claimed
	// rows persist until ReclaimStale (~10 min later).
	var orphanDrainErr error
	var orphanDrainCount int
	for {
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("RunOnce: %w", err)
		}
		batchStart := time.Now()
		ids, token, err := w.q.Claim(ctx, gen, w.deps.BatchSize)
		if err != nil {
			return res, fmt.Errorf("claim: %w", err)
		}
		if len(ids) == 0 {
			if orphanDrainErr != nil {
				return res, fmt.Errorf(
					"orphan-drain failed for %d row(s); they remain claimed and will be recovered by ReclaimStale on the next run: %w",
					orphanDrainCount, orphanDrainErr)
			}
			return res, nil
		}
		res.Claimed += len(ids)

		eb, err := w.embedBatch(ctx, ids)
		if err != nil {
			consecutiveFailures++
			lastErr = err
			w.deps.Log.Warn("embed batch failed", "gen", gen, "ids", len(ids), "error", err)

			if errors.Is(err, ErrPermanent4xx) {
				// Walk the claimed IDs one at a time. Drain decides
				// per-ID whether to drop (if some embed, the 4xxs are
				// message-specific) or release (if none embed,
				// endpoint-wide failure can't be ruled out).
				w.deps.Log.Info("embed: downshifting to BatchSize=1 to drain failing batch",
					"gen", gen, "batch_size", len(ids))
				embedded, dropped, drainErr := w.downshiftDrain(ctx, gen, token, ids, &res, &completedRows)
				res.Succeeded += embedded
				if drainErr != nil {
					w.deps.Log.Info("embed: downshift drain returned error",
						"gen", gen, "batch_size", len(ids),
						"embedded", embedded, "dropped", dropped,
						"error", drainErr)
				} else {
					w.deps.Log.Info("embed: downshift drain complete; resuming configured batch size",
						"gen", gen, "batch_size", len(ids),
						"embedded", embedded, "dropped", dropped)
				}

				// Forward progress resets the cap. Same rule as the
				// all-clean main-loop success path.
				if embedded > 0 {
					consecutiveFailures = 0
				}

				if drainErr != nil {
					// Distinguish "drain confirms upstream 4xx"
					// (every singleton returned the same
					// ErrPermanent4xx — same failure as the upstream
					// batch, already counted) from "drain hit an
					// independent error" (transient-after-retries,
					// upsert/complete failure, ctx cancel — a fresh
					// failure that should fail this run immediately.
					lastErr = drainErr
					if !errors.Is(drainErr, ErrPermanent4xx) {
						return res, fmt.Errorf("downshift drain: %w", drainErr)
					}
					if consecutiveFailures >= w.deps.MaxConsecutiveFailures {
						return res, fmt.Errorf("embed worker aborting after %d consecutive failures: %w",
							consecutiveFailures, lastErr)
					}
				}
				continue
			}

			// Non-4xx error: original release-and-fail path.
			res.Failed += len(ids)
			if rerr := w.q.Release(ctx, gen, token, ids); rerr != nil {
				w.deps.Log.Error("release after embed failure", "error", rerr)
			}
			if consecutiveFailures >= w.deps.MaxConsecutiveFailures {
				return res, fmt.Errorf("embed worker aborting after %d consecutive failures: %w",
					consecutiveFailures, lastErr)
			}
			continue
		}
		res.Truncated += eb.truncated

		if len(eb.chunks) == 0 {
			// Nothing to embed (every claimed id was missing from
			// main DB or preprocessed to empty). Drop the orphans
			// and move on; failure here counts toward
			// MaxConsecutiveFailures because the loop would
			// otherwise busy-spin on a stuck claim until
			// ReclaimStale runs (10 min default).
			dropIDs := append(append([]int64(nil), eb.missing...), eb.empty...)
			if len(dropIDs) > 0 {
				if len(eb.missing) > 0 {
					w.deps.Log.Warn("pending messages missing from main DB",
						"gen", gen, "ids", eb.missing)
				}
				if len(eb.empty) > 0 {
					w.deps.Log.Warn("pending messages empty after preprocess",
						"gen", gen, "ids", eb.empty)
				}
				if cerr := w.q.Complete(ctx, gen, token, dropIDs); cerr != nil {
					res.Failed += len(dropIDs)
					w.deps.Log.Error("complete drop failed", "error", cerr,
						"gen", gen, "ids", len(dropIDs))
					consecutiveFailures++
					lastErr = cerr
					orphanDrainErr = cerr
					orphanDrainCount += len(dropIDs)
					if consecutiveFailures >= w.deps.MaxConsecutiveFailures {
						return res, fmt.Errorf("embed worker aborting after %d consecutive failures: %w",
							consecutiveFailures, lastErr)
					}
					continue
				}
				completedRows += len(dropIDs)
				w.reportProgress(completedRows, len(dropIDs), 0, time.Since(batchStart))
			}
			continue
		}

		if err := w.deps.Backend.Upsert(ctx, gen, eb.chunks); err != nil {
			res.Failed += len(eb.embeddedIDs)
			if rerr := w.q.Release(ctx, gen, token, eb.embeddedIDs); rerr != nil {
				w.deps.Log.Error("release after upsert failure", "error", rerr)
			}
			w.deps.Log.Error("upsert failed", "gen", gen, "ids", len(eb.embeddedIDs), "error", err)
			consecutiveFailures++
			lastErr = err
			if consecutiveFailures >= w.deps.MaxConsecutiveFailures {
				return res, fmt.Errorf("embed worker aborting after %d consecutive failures: %w",
					consecutiveFailures, lastErr)
			}
			continue
		}
		// Complete acknowledges work via (gen, msg, claim_token) so a
		// stale worker whose claim was already reclaimed cannot wipe
		// the queue row belonging to the newer worker. Failure here
		// means the embedded rows stay claimed; ReclaimStale will
		// rescue them eventually but the next RunOnce would falsely
		// report a clean drain in the meantime — count the batch as
		// failed so the failure cap can short-circuit the loop.
		if cerr := w.q.Complete(ctx, gen, token, eb.embeddedIDs); cerr != nil {
			res.Failed += len(eb.embeddedIDs)
			w.deps.Log.Error("complete failed", "error", cerr,
				"gen", gen, "ids", len(eb.embeddedIDs))
			consecutiveFailures++
			lastErr = cerr
			if consecutiveFailures >= w.deps.MaxConsecutiveFailures {
				return res, fmt.Errorf("embed worker aborting after %d consecutive failures: %w",
					consecutiveFailures, lastErr)
			}
			continue
		}

		// Drop queue rows for messages that disappeared between
		// enqueue and claim. We do this AFTER embedded rows are
		// safely upserted and acknowledged so a Complete failure on
		// the orphans does not strand the valid embedded rows in a
		// claimed-but-unembedded state. Using Complete with our claim
		// token makes this a token-aware delete: we only remove rows
		// we still own. Failure here still counts as a batch failure
		// because the orphan rows would stay claimed until
		// ReclaimStale runs and falsely block the queue.
		dropIDs := append(append([]int64(nil), eb.missing...), eb.empty...)
		if len(dropIDs) > 0 {
			if len(eb.missing) > 0 {
				w.deps.Log.Warn("pending messages missing from main DB",
					"gen", gen, "ids", eb.missing)
			}
			if len(eb.empty) > 0 {
				w.deps.Log.Warn("pending messages empty after preprocess",
					"gen", gen, "ids", eb.empty)
			}
			if cerr := w.q.Complete(ctx, gen, token, dropIDs); cerr != nil {
				res.Failed += len(dropIDs)
				w.deps.Log.Error("complete drop failed", "error", cerr,
					"gen", gen, "ids", len(dropIDs))
				consecutiveFailures++
				lastErr = cerr
				orphanDrainErr = cerr
				orphanDrainCount += len(dropIDs)
				batchChars := 0
				for _, c := range eb.chunks {
					batchChars += c.SourceCharLen
				}
				completedRows += len(eb.embeddedIDs)
				w.reportProgress(completedRows, len(eb.embeddedIDs), batchChars, time.Since(batchStart))
				if consecutiveFailures >= w.deps.MaxConsecutiveFailures {
					// Embedded rows were already counted into
					// res.Succeeded above; record the orphan-drain
					// failure and abort.
					res.Succeeded += len(eb.embeddedIDs)
					return res, fmt.Errorf("embed worker aborting after %d consecutive failures: %w",
						consecutiveFailures, lastErr)
				}
				// Even though the orphan drain failed, the embedded
				// rows ARE done — count them and reset the cap.
				// Forward progress on real messages should reset
				// consecutiveFailures the same way it does in the
				// downshift drain path and the all-clean success
				// path. The orphan-drop Complete failure has its
				// own surfacing channel via orphanDrainErr (the
				// empty-claim exit returns it instead of nil), so
				// we don't need consecutiveFailures to escalate
				// orphan failures into an abort. The orphan rows
				// stay claimed and ReclaimStale recovers them.
				res.Succeeded += len(eb.embeddedIDs)
				consecutiveFailures = 0
				continue
			}
		}
		res.Succeeded += len(eb.embeddedIDs)
		consecutiveFailures = 0
		batchProcessed := len(eb.embeddedIDs) + len(dropIDs)
		completedRows += batchProcessed
		batchChars := 0
		for _, c := range eb.chunks {
			batchChars += c.SourceCharLen
		}
		w.reportProgress(completedRows, batchProcessed, batchChars, time.Since(batchStart))
	}
}

// embedBatchResult carries the output of embedBatch. chunks and
// embeddedIDs are aligned by position and correspond to messages that
// were actually fetched and embedded. missing lists ids from the
// input that had no row in the messages table; empty lists ids whose
// content preprocessed to empty and therefore should not be sent to
// embedders that reject blank strings.
type embedBatchResult struct {
	chunks      []vector.Chunk
	embeddedIDs []int64
	missing     []int64
	empty       []int64
	truncated   int
}

// embedBatch fetches subject/body for ids, preprocesses each, calls the
// embedding client, and assembles the resulting chunks. Messages that
// vanished between enqueue and claim (e.g. the sync deleted them) are
// reported in the returned result's missing slice rather than causing
// a failure — the caller decides how to drain them from the queue.
func (w *Worker) embedBatch(ctx context.Context, ids []int64) (embedBatchResult, error) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`
        SELECT m.id, COALESCE(m.subject, ''), COALESCE(mb.body_text, ''), COALESCE(mb.body_html, '')
          FROM messages m
          LEFT JOIN message_bodies mb ON mb.message_id = m.id
         WHERE m.id IN (%s)`, strings.Join(placeholders, ","))

	rows, err := w.deps.MainDB.QueryContext(ctx, query, args...)
	if err != nil {
		return embedBatchResult{}, fmt.Errorf("fetch bodies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var msgs []msgText
	var inputs []string
	var empty []int64
	fetched := make(map[int64]struct{}, len(ids))
	for rows.Next() {
		var id int64
		var subject, bodyText, bodyHTML string
		if err := rows.Scan(&id, &subject, &bodyText, &bodyHTML); err != nil {
			return embedBatchResult{}, fmt.Errorf("scan message row: %w", err)
		}
		// Fall back to HTML-to-text when the plaintext body is empty —
		// HTML-only messages would otherwise get subject-only embeddings
		// and have materially worse semantic recall.
		body := bodyText
		if body == "" && bodyHTML != "" {
			body = mime.StripHTML(bodyHTML)
		}
		txt, trunc := Preprocess(subject, body, w.deps.MaxInputChars, w.deps.Preprocess)
		fetched[id] = struct{}{}
		if strings.TrimSpace(txt) == "" {
			empty = append(empty, id)
			continue
		}
		// Preprocess truncates by runes, so the recorded length must
		// also be a rune count. Using len(txt) (bytes) inflates
		// SourceCharLen by 2-4x for CJK / emoji / accented text and
		// breaks any downstream "did we truncate?" / "how big was the
		// input?" reasoning.
		msgs = append(msgs, msgText{ID: id, Text: txt, Chars: utf8.RuneCountInString(txt), Trunc: trunc})
		inputs = append(inputs, txt)
	}
	if err := rows.Err(); err != nil {
		return embedBatchResult{}, fmt.Errorf("iterate message rows: %w", err)
	}

	// Identify claimed ids that had no row in messages; we'll report
	// them back so the caller can drop them from the queue.
	var missing []int64
	for _, id := range ids {
		if _, ok := fetched[id]; !ok {
			missing = append(missing, id)
		}
	}

	if len(msgs) == 0 {
		// All claimed ids are missing — return an empty result (no
		// chunks, no error). Caller handles the drop.
		return embedBatchResult{missing: missing, empty: empty}, nil
	}

	start := time.Now()
	vecs, err := w.deps.Client.Embed(ctx, inputs)
	if err != nil {
		return embedBatchResult{}, fmt.Errorf("embed: %w", err)
	}
	w.deps.Log.Debug("embed batch",
		"count", len(vecs), "chars", totalChars(msgs), "duration_ms", time.Since(start).Milliseconds())

	if len(vecs) != len(msgs) {
		return embedBatchResult{}, fmt.Errorf("embedder returned %d vectors for %d inputs", len(vecs), len(msgs))
	}

	truncated := 0
	chunks := make([]vector.Chunk, 0, len(vecs))
	embeddedIDs := make([]int64, 0, len(vecs))
	for i, m := range msgs {
		if m.Trunc {
			truncated++
		}
		chunks = append(chunks, vector.Chunk{
			MessageID:     m.ID,
			Vector:        vecs[i],
			SourceCharLen: m.Chars,
			Truncated:     m.Trunc,
		})
		embeddedIDs = append(embeddedIDs, m.ID)
	}
	return embedBatchResult{
		chunks:      chunks,
		embeddedIDs: embeddedIDs,
		missing:     missing,
		empty:       empty,
		truncated:   truncated,
	}, nil
}

// downshiftDrain handles a non-retryable 4xx on a claimed batch by
// walking the same already-claimed IDs one at a time. The IDs remain
// owned under the caller's claim_token throughout the drain, so we
// never re-Claim them — that would race other workers.
//
// Singleton 4xxs are NOT eagerly Completed. ErrPermanent4xx covers
// both message-specific failures (413 payload-too-large, 422
// Unprocessable, 400 invalid-input) and endpoint/config-wide
// failures (401 bad-key, 403 forbidden, 404 invalid-model, 400
// malformed-shared-config) — the two are indistinguishable at the
// call site. If we Complete-deleted on every singleton 4xx, a
// misconfigured endpoint would silently destroy work. Instead we
// defer the drop decision: collect the 4xxing IDs, and at end of
// drain decide based on whether anything embedded.
//
// Returned `embedded` is the count of singletons that successfully
// embedded.
//
// Returned `dropped` is the count of singletons whose drop was
// confirmed (Complete succeeded). A drain that releases its deferred
// IDs back to the queue (because no singleton embedded) returns
// `dropped == 0`.
//
// Returned `err`:
//   - non-nil all-drop: every singleton 4xxd, no embeds. Deferred
//     IDs were Released back to the queue (so a misconfigured
//     endpoint does not lose work) and the wrapped 4xx is returned.
//     The caller increments consecutiveFailures and the cap will
//     eventually trip, surfacing the original 4xx body.
//   - non-nil non-4xx interruption: transient errors that exhausted
//     retries inside embedBatch, upsert/complete failures, or a
//     cancellation seen after embedBatch starts. Deferred and
//     unprocessed IDs are released before returning so a later run
//     can retry them promptly. A cancellation observed before the
//     next singleton starts returns ctx.Err without releasing; those
//     rows remain claimed for ReclaimStale to recover.
//   - nil: drain completed cleanly. If `embedded > 0` and there were
//     deferred IDs, they were Completed as message-specific drops.
func (w *Worker) downshiftDrain(
	ctx context.Context,
	gen vector.GenerationID,
	token string,
	ids []int64,
	res *RunResult,
	completedRows *int,
) (embedded int, dropped int, err error) {
	var deferredDrops []int64
	var lastDeferredErr error

	for i, id := range ids {
		select {
		case <-ctx.Done():
			return embedded, dropped, ctx.Err()
		default:
		}

		batchStart := time.Now()
		eb, e := w.embedBatch(ctx, []int64{id})
		if e != nil {
			if errors.Is(e, ErrPermanent4xx) {
				// Defer the drop decision. See function-level
				// comment for the endpoint-vs-message distinction.
				deferredDrops = append(deferredDrops, id)
				lastDeferredErr = e
				continue
			}
			w.releaseDownshiftRemainder(ctx, gen, token, append(append([]int64(nil), deferredDrops...), ids[i:]...))
			return embedded, dropped, e
		}
		if len(eb.chunks) == 0 {
			drop := append(append([]int64(nil), eb.missing...), eb.empty...)
			if len(drop) > 0 {
				if cerr := w.q.Complete(ctx, gen, token, drop); cerr != nil {
					res.Failed += len(drop)
					w.releaseDownshiftRemainder(ctx, gen, token, append(append([]int64(nil), deferredDrops...), ids[i:]...))
					return embedded, dropped, fmt.Errorf("complete drop: %w", cerr)
				}
				dropped += len(drop)
				*completedRows += len(drop)
				w.reportProgress(*completedRows, len(drop), 0, time.Since(batchStart))
			}
			continue
		}
		if uerr := w.deps.Backend.Upsert(ctx, gen, eb.chunks); uerr != nil {
			w.releaseDownshiftRemainder(ctx, gen, token, append(append([]int64(nil), deferredDrops...), ids[i:]...))
			return embedded, dropped, fmt.Errorf("upsert: %w", uerr)
		}
		if cerr := w.q.Complete(ctx, gen, token, eb.embeddedIDs); cerr != nil {
			w.releaseDownshiftRemainder(ctx, gen, token, append(append([]int64(nil), deferredDrops...), ids[i:]...))
			return embedded, dropped, fmt.Errorf("complete: %w", cerr)
		}
		res.Truncated += eb.truncated
		embedded += len(eb.embeddedIDs)
		*completedRows += len(eb.embeddedIDs)
		batchChars := 0
		for _, c := range eb.chunks {
			batchChars += c.SourceCharLen
		}
		w.reportProgress(*completedRows, len(eb.embeddedIDs), batchChars, time.Since(batchStart))
	}

	// Drain finished. Decide deferred-drop fate.
	if len(deferredDrops) == 0 {
		return embedded, dropped, nil
	}
	if embedded > 0 {
		// Endpoint works for some messages, so the 4xxs are
		// message-specific (oversize input, malformed input, etc.).
		// Drop them.
		for _, id := range deferredDrops {
			w.deps.Log.Warn("dropping pending message after singleton 4xx",
				"gen", gen, "id", id, "error", lastDeferredErr)
		}
		dropStart := time.Now()
		if cerr := w.q.Complete(ctx, gen, token, deferredDrops); cerr != nil {
			res.Failed += len(deferredDrops)
			return embedded, dropped, fmt.Errorf("complete drop: %w", cerr)
		}
		dropped += len(deferredDrops)
		*completedRows += len(deferredDrops)
		w.reportProgress(*completedRows, len(deferredDrops), 0, time.Since(dropStart))
		return embedded, dropped, nil
	}
	// embedded == 0. We can't distinguish endpoint-wide failure from a
	// batch where every message just happened to be unembeddable.
	// Release the deferred IDs (rather than Completing them) so a
	// misconfigured endpoint does not silently destroy work, and
	// return the wrapped 4xx so the caller surfaces it. The released
	// IDs go back to the pending queue and will be re-claimed; if the
	// underlying problem persists, the consecutive-failure cap will
	// eventually trip with the same 4xx body in lastErr.
	if rerr := w.q.Release(ctx, gen, token, deferredDrops); rerr != nil {
		w.deps.Log.Error("release after all-drop drain", "error", rerr,
			"gen", gen, "ids", len(deferredDrops))
	}
	return embedded, dropped, fmt.Errorf("downshift all-drop: every singleton returned non-retryable 4xx (released %d row(s) back to queue): %w",
		len(deferredDrops), lastDeferredErr)
}

func (w *Worker) releaseDownshiftRemainder(ctx context.Context, gen vector.GenerationID, token string, ids []int64) {
	if len(ids) == 0 {
		return
	}
	if rerr := w.q.Release(ctx, gen, token, ids); rerr != nil {
		w.deps.Log.Error("release after downshift interruption", "error", rerr,
			"gen", gen, "ids", len(ids))
	}
}

func (w *Worker) reportProgress(done, batchMsgs, batchChars int, batchElapsed time.Duration) {
	if w.deps.Progress == nil {
		return
	}
	w.deps.Progress(ProgressReport{
		Done:         done,
		TotalPending: w.deps.TotalPending,
		BatchMsgs:    batchMsgs,
		BatchChars:   batchChars,
		BatchElapsed: batchElapsed,
		RunElapsed:   time.Since(w.runStart),
	})
}

func totalChars(ms []msgText) int {
	n := 0
	for _, m := range ms {
		n += m.Chars
	}
	return n
}
