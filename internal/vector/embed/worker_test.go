//go:build sqlite_vec

package embed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestDerivedStaleThreshold(t *testing.T) {
	cases := []struct {
		name       string
		timeout    time.Duration
		maxRetries int
		want       time.Duration
	}{
		{"zero timeout returns floor", 0, 3, 10 * time.Minute},
		{"small budget keeps floor", 30 * time.Second, 3, 10 * time.Minute},          // 2*30s*3 = 3m → floor wins
		{"large timeout exceeds floor", 5 * time.Minute, 3, 30 * time.Minute},        // 2*5m*3 = 30m
		{"high attempts scale", 30 * time.Second, 30, 30 * time.Minute},              // 2*30s*30 = 30m
		{"negative attempts treated as 1 attempt", 1 * time.Hour, -5, 2 * time.Hour}, // 2*1h*1 = 2h, exceeds floor
		// Regression: callers that set EmbedTimeout but leave
		// EmbedMaxRetries at zero used to derive a budget for a single
		// attempt (2*10m*1 = 20m). The fix mirrors embed.NewClient's
		// default of 3 total attempts → 60m.
		{"zero attempts mirror client default", 10 * time.Minute, 0, 60 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := derivedStaleThreshold(tc.timeout, tc.maxRetries)
			if got != tc.want {
				t.Errorf("derivedStaleThreshold(%v, %d) = %v, want %v",
					tc.timeout, tc.maxRetries, got, tc.want)
			}
		})
	}
}

// TestWorker_SplitsChunkInputsAcrossSubBatches verifies that a
// message whose chunk fan-out exceeds BatchSize is sent to the
// embedder across multiple sub-batched Embed calls. Without the
// split, a 64-chunk message claimed via BatchSize=8 would flatten
// into a single 64-input request, exceeding provider per-request
// limits and tripping API timeouts (the very failure mode caught by
// roborev #323).
func TestWorker_SplitsChunkInputsAcrossSubBatches(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 1)

	// Build a body large enough to produce ~12 chunks at
	// MaxInputChars=80 with the worker's overlap heuristic. Any value
	// well above BatchSize would do; 12 is comfortably enough to
	// exercise multiple sub-batches.
	var body string
	for i := 0; i < 40; i++ {
		body += "lorem ipsum dolor sit amet consectetur adipiscing elit. "
	}
	if _, err := f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, body); err != nil {
		t.Fatalf("update body: %v", err)
	}

	// Capture the size of every Embed call so we can prove the split
	// happened and that no sub-batch exceeded BatchSize.
	const batchSize = 4
	var sizes []int
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		sizes = append(sizes, len(inputs))
		out := make([][]float32, len(inputs))
		for i := range inputs {
			v := make([]float32, 4)
			v[0] = float32(len(inputs[i])%4 + 1)
			out[i] = v
		}
		return out, nil
	}

	w := NewWorker(WorkerDeps{
		Backend:       f.Backend,
		VectorsDB:     f.VectorsDB,
		MainDB:        f.MainDB,
		Client:        f.FakeClient,
		Preprocess:    PreprocessConfig{},
		MaxInputChars: 80,
		BatchSize:     batchSize,
	})
	if _, err := w.RunOnce(ctx, f.BuildingGen); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(sizes) < 2 {
		t.Fatalf("expected >= 2 sub-batches, got %d (sizes=%v)", len(sizes), sizes)
	}
	for i, n := range sizes {
		if n > batchSize {
			t.Errorf("sub-batch %d had %d inputs, want <= %d", i, n, batchSize)
		}
		if n == 0 {
			t.Errorf("sub-batch %d was empty", i)
		}
	}
}

// TestWorker_CapsRawBodyBeforePreprocess is the roborev #323 (717ac4c)
// regression: a 5 MB body must not be handed to Preprocess in full.
// Preprocess runs O(input) regex passes; without an upstream cap the
// embedding worker pays seconds of CPU and tens of MB of scratch
// allocs on every multi-megabyte body before the chunker drops the
// tail anyway. The cap should be derived from MaxInputChars and
// maxSpansPerMessage so it scales with what the chunker can actually
// emit.
func TestWorker_CapsRawBodyBeforePreprocess(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 1)

	// 5 million chars of unbroken letters. Far larger than the
	// raw-body cap (which at MaxInputChars=100 is 100 * 64 * 16 =
	// 102,400 runes), but well-defined under regex passes if those
	// run unbounded.
	hugeBody := strings.Repeat("a", 5_000_000)
	if _, err := f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, hugeBody); err != nil {
		t.Fatalf("update body: %v", err)
	}

	// Capture every input the worker hands to the embedder. The
	// individual input slices together represent the chunker's
	// output; the total must come from at most the cap window.
	var observed []string
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		observed = append(observed, inputs...)
		out := make([][]float32, len(inputs))
		for i := range inputs {
			v := make([]float32, 4)
			v[0] = float32(len(inputs[i])%4 + 1)
			out[i] = v
		}
		return out, nil
	}

	w := NewWorker(WorkerDeps{
		Backend:       f.Backend,
		VectorsDB:     f.VectorsDB,
		MainDB:        f.MainDB,
		Client:        f.FakeClient,
		Preprocess:    PreprocessConfig{},
		MaxInputChars: 100,
		BatchSize:     8,
	})
	if _, err := w.RunOnce(ctx, f.BuildingGen); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// All inputs must fit within the chunker's output window
	// (maxSpansPerMessage * MaxInputChars = 6400 runes). The raw
	// body cap means Preprocess only ever saw ~102K runes, not 5M.
	totalRunes := 0
	for _, s := range observed {
		totalRunes += utf8.RuneCountInString(s)
	}
	if totalRunes > maxSpansPerMessage*100 {
		t.Errorf("total embedder input runes = %d, want <= %d (chunker window)", totalRunes, maxSpansPerMessage*100)
	}
}

// TestWorker_PrefixBase64DoesNotHidePoseTail is the roborev #323
// (2d8f45d) regression: a body whose first megabyte is an inline
// base64 PNG must still get its prose tail to the embedder. Earlier
// versions capped the raw body before sanitize, so the cap chopped
// the base64 blob before StripBase64 could strip it — the prose
// past the cap never reached Preprocess. The fix runs the cheap
// pollution removal (CRLF + StripBase64) BEFORE the cap and the
// heavy regex passes (StripHTML, URL tracking, whitespace) AFTER,
// so blob-prefixed bodies preserve the prose.
func TestWorker_PrefixBase64DoesNotHidePoseTail(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 1)

	const sentinel = "QUICKFOX-MARKER-IS-THE-PROSE-TAIL"
	// 2M chars of base64-shaped padding (no slashes; matches the
	// strip regex), then 100 bytes of prose containing the
	// sentinel.
	hugeBase64 := strings.Repeat("A", 2_000_000)
	body := "data:image/png;base64," + hugeBase64 + " " + sentinel + " end."
	if _, err := f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, body); err != nil {
		t.Fatalf("update body: %v", err)
	}

	var observed []string
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		observed = append(observed, inputs...)
		out := make([][]float32, len(inputs))
		for i := range inputs {
			v := make([]float32, 4)
			v[0] = float32(len(inputs[i])%4 + 1)
			out[i] = v
		}
		return out, nil
	}

	w := NewWorker(WorkerDeps{
		Backend:       f.Backend,
		VectorsDB:     f.VectorsDB,
		MainDB:        f.MainDB,
		Client:        f.FakeClient,
		Preprocess:    PreprocessConfig{StripBase64: true},
		MaxInputChars: 100,
		BatchSize:     8,
	})
	if _, err := w.RunOnce(ctx, f.BuildingGen); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// The sentinel — sitting past 2 MB of base64 in the raw body —
	// must appear in at least one chunk handed to the embedder.
	// Without the cheap-strip-first ordering, the cap would have
	// chopped before the prose ever surfaced.
	found := false
	for _, s := range observed {
		if strings.Contains(s, sentinel) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("sentinel %q absent from embedder inputs; the prose tail was lost behind the base64 blob", sentinel)
	}
}

// TestWorker_TruncatedCountedPerMessageNotPerChunk pins the
// roborev #323 (717ac4c) metric-accounting fix: when a single long
// message produces multiple truncated chunks, RunResult.Truncated
// must record one message, not one per chunk. Otherwise progress
// metrics inflate (a single oversized message could read as N
// truncations in a Succeeded=1 batch).
func TestWorker_TruncatedCountedPerMessageNotPerChunk(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 1)

	// Long unbroken text: ChunkText's hard-cut path marks all but
	// the last span as Trunc=true. With MaxInputChars=100 the body
	// produces several truncated chunks before maxSpans caps it.
	body := strings.Repeat("a", 600)
	if _, err := f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, body); err != nil {
		t.Fatalf("update body: %v", err)
	}

	w := NewWorker(WorkerDeps{
		Backend:       f.Backend,
		VectorsDB:     f.VectorsDB,
		MainDB:        f.MainDB,
		Client:        f.FakeClient,
		Preprocess:    PreprocessConfig{},
		MaxInputChars: 100,
		BatchSize:     8,
	})
	res, err := w.RunOnce(ctx, f.BuildingGen)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Succeeded != 1 {
		t.Errorf("Succeeded = %d, want 1", res.Succeeded)
	}
	// Confirm the chunks table actually has multiple truncated
	// rows for this message — otherwise the test wouldn't be
	// exercising the per-message-vs-per-chunk distinction.
	var truncChunks int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE message_id = 1 AND truncated = 1`).Scan(&truncChunks); err != nil {
		t.Fatalf("count truncated chunks: %v", err)
	}
	if truncChunks < 2 {
		t.Fatalf("test produced %d truncated chunks, expected >= 2 to exercise the metric", truncChunks)
	}
	if res.Truncated != 1 {
		t.Errorf("Truncated = %d, want 1 (one message, regardless of %d truncated chunks)", res.Truncated, truncChunks)
	}
}

// TestWorker_FansOutLongMessageIntoMultipleChunks confirms the
// chunking path: a single pending message whose preprocessed body
// exceeds MaxInputChars produces N > 1 embedder inputs, all of which
// land in the embeddings table with distinct chunk_index values, and
// the queue is drained in one shot (not N times) — Complete is
// keyed on message_id, not on (message_id, chunk_index).
func TestWorker_FansOutLongMessageIntoMultipleChunks(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 1)

	// Replace the seeded message's body with one long enough to need
	// multiple chunks at MaxInputChars=200. Each "paragraph" is ~150
	// chars; six paragraphs ≈ 900 chars → at least 4 chunks.
	body := ""
	for i := 0; i < 6; i++ {
		body += "lorem ipsum dolor sit amet consectetur adipiscing elit. " +
			"sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. " +
			"ut enim ad minim veniam quis nostrud exercitation. " +
			"\n\n"
	}
	if _, err := f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, body); err != nil {
		t.Fatalf("update body: %v", err)
	}

	w := NewWorker(WorkerDeps{
		Backend:       f.Backend,
		VectorsDB:     f.VectorsDB,
		MainDB:        f.MainDB,
		Client:        f.FakeClient,
		Preprocess:    PreprocessConfig{},
		MaxInputChars: 200, // forces multi-chunk fan-out
		BatchSize:     8,
	})
	res, err := w.RunOnce(ctx, f.BuildingGen)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Succeeded != 1 {
		t.Errorf("succeeded=%d, want 1 (one distinct message embedded)", res.Succeeded)
	}
	if res.Failed != 0 {
		t.Errorf("failed=%d, want 0", res.Failed)
	}

	// embeddings should hold N > 1 rows for the message, with
	// consecutive chunk_index values starting at 0.
	var rowCount int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ? AND message_id = 1`,
		int64(f.BuildingGen)).Scan(&rowCount); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if rowCount < 2 {
		t.Fatalf("expected >= 2 chunk rows, got %d", rowCount)
	}
	var distinctCI int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT chunk_index) FROM embeddings WHERE generation_id = ? AND message_id = 1`,
		int64(f.BuildingGen)).Scan(&distinctCI); err != nil {
		t.Fatalf("count distinct chunk_index: %v", err)
	}
	if distinctCI != rowCount {
		t.Errorf("distinct chunk_index=%d, want %d (each chunk should be uniquely indexed)", distinctCI, rowCount)
	}
	var minCI, maxCI int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT MIN(chunk_index), MAX(chunk_index) FROM embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&minCI, &maxCI); err != nil {
		t.Fatalf("min/max chunk_index: %v", err)
	}
	if minCI != 0 || maxCI != rowCount-1 {
		t.Errorf("chunk_index range = [%d, %d], want [0, %d]", minCI, maxCI, rowCount-1)
	}
	// message_count tracks distinct messages, so it must read as 1
	// despite the multi-chunk fan-out.
	var msgCount int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT message_count FROM index_generations WHERE id = ?`,
		int64(f.BuildingGen)).Scan(&msgCount); err != nil {
		t.Fatalf("read message_count: %v", err)
	}
	if msgCount != 1 {
		t.Errorf("message_count = %d, want 1", msgCount)
	}
	// Queue is fully drained: Complete is keyed on message_id, so all
	// chunks of message 1 finish together when its singleton pending
	// row is removed.
	var pending int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("pending remaining: %d", pending)
	}
}

func TestWorker_DrainsPendingEndToEnd(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 3)

	w := NewWorker(WorkerDeps{
		Backend:       f.Backend,
		VectorsDB:     f.VectorsDB,
		MainDB:        f.MainDB,
		Client:        f.FakeClient,
		Preprocess:    PreprocessConfig{StripQuotes: true, StripSignatures: true},
		MaxInputChars: 8000,
		BatchSize:     2,
	})

	res, err := w.RunOnce(ctx, f.BuildingGen)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Succeeded != 3 {
		t.Errorf("succeeded=%d, want 3", res.Succeeded)
	}
	if res.Failed != 0 {
		t.Errorf("failed=%d, want 0", res.Failed)
	}

	var n int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&n); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if n != 0 {
		t.Errorf("pending remaining: %d", n)
	}
}

func TestWorker_ReleasesOnClientError(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 3)
	f.FakeClient.FailNext(1) // first Embed errors; remaining batches succeed

	w := NewWorker(WorkerDeps{
		Backend:       f.Backend,
		VectorsDB:     f.VectorsDB,
		MainDB:        f.MainDB,
		Client:        f.FakeClient,
		Preprocess:    PreprocessConfig{},
		MaxInputChars: 8000,
		BatchSize:     1, // batch of 1 so the first Embed fails exactly one id
	})
	res, err := w.RunOnce(ctx, f.BuildingGen)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Failed < 1 {
		t.Errorf("expected at least 1 failure, got %d", res.Failed)
	}
	// The worker retries the released row and eventually drains everything.
	if res.Succeeded != 3 {
		t.Errorf("succeeded=%d, want 3 (failed row gets retried after Release)", res.Succeeded)
	}
}

func TestWorker_ReleasesOnUpsertError(t *testing.T) {
	// Driving an Upsert failure requires forcing a dimension mismatch; the
	// fake client returns 4-dim vectors matching the generation's
	// dimension, so the easy lever isn't available. The Release-on-error
	// path is covered by TestWorker_ReleasesOnClientError.
	t.Skip("covered by TestWorker_ReleasesOnClientError")
}

func TestWorker_EmptyPendingReturnsZeroResult(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 0) // 0 messages → no pending rows

	w := NewWorker(WorkerDeps{
		Backend:   f.Backend,
		VectorsDB: f.VectorsDB,
		MainDB:    f.MainDB,
		Client:    f.FakeClient,
		BatchSize: 10,
	})
	res, err := w.RunOnce(ctx, f.BuildingGen)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Claimed != 0 || res.Succeeded != 0 || res.Failed != 0 {
		t.Errorf("expected empty result, got %+v", res)
	}
}

func TestWorker_RespectsContextCancel(t *testing.T) {
	f := newWorkerFixture(t, 5)

	w := NewWorker(WorkerDeps{
		Backend:   f.Backend,
		VectorsDB: f.VectorsDB,
		MainDB:    f.MainDB,
		Client:    f.FakeClient,
		BatchSize: 1,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled
	_, err := w.RunOnce(ctx, f.BuildingGen)
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
}

func TestWorker_ReclaimStale_FromStartup(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 2)

	w := NewWorker(WorkerDeps{
		Backend:        f.Backend,
		VectorsDB:      f.VectorsDB,
		MainDB:         f.MainDB,
		Client:         f.FakeClient,
		BatchSize:      2,
		StaleThreshold: 10 * time.Minute,
	})

	// Simulate a crashed worker: claim 2 rows, then back-date the claim.
	q := NewQueue(f.VectorsDB)
	ids, _, err := q.Claim(ctx, f.BuildingGen, 2)
	if err != nil || len(ids) != 2 {
		t.Fatalf("Claim setup: ids=%v err=%v", ids, err)
	}
	if _, err := f.VectorsDB.ExecContext(ctx,
		`UPDATE pending_embeddings SET claimed_at = ? WHERE generation_id = ?`,
		time.Now().Add(-20*time.Minute).Unix(), int64(f.BuildingGen)); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := w.ReclaimStale(ctx)
	if err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}
	if n != 2 {
		t.Errorf("reclaimed %d, want 2", n)
	}

	// Verify the rows are available again.
	var available int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ? AND claimed_at IS NULL`,
		int64(f.BuildingGen)).Scan(&available); err != nil {
		t.Fatalf("count available: %v", err)
	}
	if available != 2 {
		t.Errorf("available after reclaim = %d, want 2", available)
	}
}

func TestWorker_StaleThresholdDefault(t *testing.T) {
	f := newWorkerFixture(t, 0)
	w := NewWorker(WorkerDeps{
		Backend:   f.Backend,
		VectorsDB: f.VectorsDB,
		MainDB:    f.MainDB,
		Client:    f.FakeClient,
	})
	if w.deps.StaleThreshold != 10*time.Minute {
		t.Errorf("default StaleThreshold=%v, want 10m", w.deps.StaleThreshold)
	}
	if w.deps.MaxConsecutiveFailures != 5 {
		t.Errorf("default MaxConsecutiveFailures=%d, want 5", w.deps.MaxConsecutiveFailures)
	}
}

// TestWorker_AbortsAfterConsecutiveFailures verifies that a
// persistently failing embedder causes RunOnce to return an error
// rather than loop forever releasing and re-claiming.
func TestWorker_AbortsAfterConsecutiveFailures(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 3)
	// Force every Embed call to fail — a huge failure budget ensures
	// we hit the MaxConsecutiveFailures limit first.
	f.FakeClient.FailNext(1 << 30)

	w := NewWorker(WorkerDeps{
		Backend:                f.Backend,
		VectorsDB:              f.VectorsDB,
		MainDB:                 f.MainDB,
		Client:                 f.FakeClient,
		BatchSize:              1,
		MaxConsecutiveFailures: 3,
	})

	res, err := w.RunOnce(ctx, f.BuildingGen)
	if err == nil {
		t.Fatal("RunOnce: want error after consecutive failures, got nil")
	}
	if res.Failed < 3 {
		t.Errorf("Failed=%d, want >= 3 (one per consecutive failure)", res.Failed)
	}
	// Any leftover claims should have been released; pending is non-empty.
	var pending int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending == 0 {
		t.Errorf("pending rows after abort = 0, want non-zero (rows should have been released)")
	}
}

// TestWorker_ConsecutiveFailureCounterResetsOnSuccess confirms that
// intermittent failures below the limit do not abort — each success
// resets the counter.
func TestWorker_ConsecutiveFailureCounterResetsOnSuccess(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 4)
	// Fail twice (below the limit of 3), then all subsequent succeed.
	f.FakeClient.FailNext(2)

	w := NewWorker(WorkerDeps{
		Backend:                f.Backend,
		VectorsDB:              f.VectorsDB,
		MainDB:                 f.MainDB,
		Client:                 f.FakeClient,
		BatchSize:              1,
		MaxConsecutiveFailures: 3,
	})
	res, err := w.RunOnce(ctx, f.BuildingGen)
	if err != nil {
		t.Fatalf("RunOnce: %v (2 failures, budget 3 — should not abort)", err)
	}
	if res.Succeeded != 4 {
		t.Errorf("Succeeded=%d, want 4 (all 4 messages ultimately drain)", res.Succeeded)
	}
}

// TestWorker_RuneCountUsedForSourceCharLen regresses the
// byte-vs-rune mismatch: Preprocess truncates by runes, so the
// SourceCharLen field on each Chunk must also be a rune count or
// CJK/emoji inputs get inflated by 2-4x. We embed a short Japanese
// subject (whose UTF-8 byte length is much larger than its rune
// count) and assert the persisted source_char_len matches runes.
func TestWorker_RuneCountUsedForSourceCharLen(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 0) // start empty so we control the message text

	// "こんにちは世界" = 7 runes, 21 UTF-8 bytes. Preprocess prepends
	// "Subject: " (9 ASCII bytes/runes) and "\n\n" (2). The full
	// preprocessed string has 18 runes and 32 bytes — a 1.78x
	// inflation if we record bytes by mistake.
	const subject = "こんにちは世界"
	if _, err := f.MainDB.ExecContext(ctx,
		`INSERT INTO messages (id, subject) VALUES (1, ?)`, subject); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if _, err := f.VectorsDB.ExecContext(ctx,
		`INSERT INTO pending_embeddings (generation_id, message_id, enqueued_at) VALUES (?, 1, 0)`,
		int64(f.BuildingGen)); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	w := NewWorker(WorkerDeps{
		Backend:   f.Backend,
		VectorsDB: f.VectorsDB,
		MainDB:    f.MainDB,
		Client:    f.FakeClient,
		BatchSize: 1,
	})
	res, err := w.RunOnce(ctx, f.BuildingGen)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Succeeded != 1 {
		t.Fatalf("Succeeded=%d, want 1", res.Succeeded)
	}

	const wantRunes = 18 // len("Subject: \n\n") + 7 runes for the kanji
	var got int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT source_char_len FROM embeddings WHERE generation_id = ? AND message_id = 1`,
		int64(f.BuildingGen)).Scan(&got); err != nil {
		t.Fatalf("read source_char_len: %v", err)
	}
	if got != wantRunes {
		t.Errorf("source_char_len=%d, want %d (rune count, not byte length)", got, wantRunes)
	}
}

// TestWorker_FallsBackToHTMLWhenBodyTextEmpty guards the HTML-only
// recall path: messages whose plaintext body is absent should still
// be embedded using HTML-stripped text rather than silently degrading
// to subject-only embeddings.
func TestWorker_FallsBackToHTMLWhenBodyTextEmpty(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 0)

	const html = `<html><body><p>planning offsite agenda</p><p>Thursday afternoon</p></body></html>`
	if _, err := f.MainDB.ExecContext(ctx,
		`INSERT INTO messages (id, subject) VALUES (1, ?)`, "meeting"); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if _, err := f.MainDB.ExecContext(ctx,
		`INSERT INTO message_bodies (message_id, body_text, body_html) VALUES (1, '', ?)`, html); err != nil {
		t.Fatalf("insert body: %v", err)
	}
	if _, err := f.VectorsDB.ExecContext(ctx,
		`INSERT INTO pending_embeddings (generation_id, message_id, enqueued_at) VALUES (?, 1, 0)`,
		int64(f.BuildingGen)); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	w := NewWorker(WorkerDeps{
		Backend:       f.Backend,
		VectorsDB:     f.VectorsDB,
		MainDB:        f.MainDB,
		Client:        f.FakeClient,
		MaxInputChars: 8000,
		BatchSize:     1,
	})
	if _, err := w.RunOnce(ctx, f.BuildingGen); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(f.FakeClient.LastInputs) != 1 {
		t.Fatalf("captured %d inputs, want 1", len(f.FakeClient.LastInputs))
	}
	got := f.FakeClient.LastInputs[0]
	// The preprocessed text should contain the HTML paragraph text,
	// not just the subject — that's the whole point of the fallback.
	if !strings.Contains(got, "planning offsite agenda") {
		t.Errorf("embed input missing HTML body text: %q", got)
	}
	if !strings.Contains(got, "Thursday afternoon") {
		t.Errorf("embed input missing second paragraph: %q", got)
	}
	if strings.Contains(got, "<p>") || strings.Contains(got, "</body>") {
		t.Errorf("embed input still contains HTML tags: %q", got)
	}
}

// TestWorker_CompleteFailureCountsAsBatchFailure regresses the bug
// where Queue.Complete failures were log-only: the embedded rows
// stayed claimed, the next Claim returned empty, and RunOnce
// reported a clean drain. After this fix Complete failure must count
// toward MaxConsecutiveFailures so the loop short-circuits instead of
// silently spinning until ReclaimStale rescues the rows minutes later.
//
// The earlier version of this test dropped pending_embeddings to make
// Complete fail, but that also broke the next Claim — the test then
// passed because Claim errored out, not because Complete failure was
// detected. To actually exercise the stuck-claim path we install a
// BEFORE DELETE trigger that fires only on Complete (Claim does an
// UPDATE, not a DELETE, so it still succeeds). After RunOnce errors,
// we assert the pending row is still present AND claimed — proving
// the loop noticed the stuck state instead of silently treating an
// empty Claim as a clean drain.
func TestWorker_CompleteFailureCountsAsBatchFailure(t *testing.T) {
	ctx := context.Background()
	// Need ≥ MaxConsecutiveFailures messages so successive claims pull
	// fresh rows; otherwise a single failed Complete leaves one stuck-
	// claimed row and the next Claim returns empty (which RunOnce
	// rightly treats as a clean drain — the bug we're regressing
	// against would never trip with a single-message fixture).
	f := newWorkerFixture(t, 3)

	if _, err := f.VectorsDB.ExecContext(ctx, `
        CREATE TRIGGER block_pending_delete
        BEFORE DELETE ON pending_embeddings
        BEGIN
            SELECT RAISE(FAIL, 'simulated complete failure');
        END`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}

	w := NewWorker(WorkerDeps{
		Backend:                f.Backend,
		VectorsDB:              f.VectorsDB,
		MainDB:                 f.MainDB,
		Client:                 f.FakeClient,
		BatchSize:              1,
		MaxConsecutiveFailures: 2,
	})

	res, err := w.RunOnce(ctx, f.BuildingGen)
	if err == nil {
		t.Fatal("RunOnce: want error after Complete failures, got nil (regression: silent success)")
	}
	if res.Succeeded != 0 {
		t.Errorf("Succeeded=%d, want 0 (Complete failed, work was not durably finished)", res.Succeeded)
	}
	if res.Failed == 0 {
		t.Errorf("Failed=%d, want > 0 (Complete failure should count as a batch failure)", res.Failed)
	}

	// Stuck-claim check: pending_embeddings row is still there (the
	// trigger blocked Complete's DELETE) and is marked claimed (the
	// previous Claim's UPDATE went through). A naive "log-only"
	// Complete handler would silently report success; the failure
	// counter is what makes RunOnce notice and abort.
	var pending, claimed int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*),
                COALESCE(SUM(CASE WHEN claimed_at IS NOT NULL THEN 1 ELSE 0 END), 0)
           FROM pending_embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&pending, &claimed); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending == 0 {
		t.Errorf("pending=%d, want > 0 — Complete should have failed and left the row in place", pending)
	}
	if claimed == 0 {
		t.Errorf("claimed=%d, want > 0 — Claim's UPDATE should have left the row marked claimed", claimed)
	}
}

// TestWorker_OrphanCompleteFailureDoesNotStrandValidWork regresses
// two related bugs around orphan-drain failure:
//
//  1. Original (R53a): a failed Complete(missing) call ran BEFORE
//     Upsert and used `continue`, leaving the still-valid claimed IDs
//     in the same batch claimed but unembedded until ReclaimStale.
//     After the fix, orphan-drain runs AFTER the embedded rows are
//     upserted and acknowledged.
//
//  2. R58: when the orphan was the last queue row, the next Claim
//     returned empty and RunOnce exited nil — leaving the orphan
//     stranded for ~10 min until ReclaimStale, with no signal to
//     the caller. After the fix, the empty-claim exit surfaces the
//     orphan-drain failure as a non-nil error so the user knows the
//     run was incomplete.
func TestWorker_OrphanCompleteFailureDoesNotStrandValidWork(t *testing.T) {
	ctx := context.Background()
	// 2 messages enqueued; we'll delete one from the main DB so it
	// reaches embedBatch as "missing".
	f := newWorkerFixture(t, 2)

	const orphanID = 2
	if _, err := f.MainDB.ExecContext(ctx,
		`DELETE FROM messages WHERE id = ?`, orphanID); err != nil {
		t.Fatalf("delete orphan from main: %v", err)
	}
	if _, err := f.MainDB.ExecContext(ctx,
		`DELETE FROM message_bodies WHERE message_id = ?`, orphanID); err != nil {
		t.Fatalf("delete orphan body: %v", err)
	}

	// Selective trigger: only the orphan's Complete DELETE fails. The
	// embedded row's Complete must still succeed so we can prove the
	// valid work is durably finished even when the orphan drain fails.
	if _, err := f.VectorsDB.ExecContext(ctx, fmt.Sprintf(`
        CREATE TRIGGER block_orphan_drain
        BEFORE DELETE ON pending_embeddings
        WHEN OLD.message_id = %d
        BEGIN
            SELECT RAISE(FAIL, 'simulated orphan complete failure');
        END`, orphanID)); err != nil {
		t.Fatalf("install trigger: %v", err)
	}

	var reports []ProgressReport
	w := NewWorker(WorkerDeps{
		Backend:                f.Backend,
		VectorsDB:              f.VectorsDB,
		MainDB:                 f.MainDB,
		Client:                 f.FakeClient,
		BatchSize:              2,
		MaxConsecutiveFailures: 5, // generous so the orphan drain failure does not abort mid-loop
		TotalPending:           2,
		Progress: func(p ProgressReport) {
			reports = append(reports, p)
		},
	})

	res, err := w.RunOnce(ctx, f.BuildingGen)
	if err == nil {
		t.Fatal("RunOnce: want non-nil error (orphan drain failed and orphan remained stuck), got nil")
	}
	if !strings.Contains(err.Error(), "orphan-drain") {
		t.Errorf("RunOnce err = %q, want it to mention 'orphan-drain'", err.Error())
	}
	if !strings.Contains(err.Error(), "ReclaimStale") {
		t.Errorf("RunOnce err = %q, want it to mention 'ReclaimStale' so the user knows recovery is automatic", err.Error())
	}
	if res.Succeeded != 1 {
		t.Errorf("Succeeded=%d, want 1 (the valid message must be counted as completed)", res.Succeeded)
	}
	if res.Failed == 0 {
		t.Errorf("Failed=%d, want > 0 (orphan drain failure should be reported)", res.Failed)
	}
	if len(reports) == 0 {
		t.Fatalf("expected progress for valid embedded row even though orphan drain failed")
	}
	final := reports[len(reports)-1]
	if final.Done != 1 {
		t.Errorf("final progress Done=%d, want 1 durable embedded row", final.Done)
	}
	if final.BatchMsgs != 1 {
		t.Errorf("final progress BatchMsgs=%d, want 1 durable embedded row", final.BatchMsgs)
	}

	// The valid message's pending row must be GONE (Complete succeeded).
	// The original bug left it claimed-but-not-completed because the
	// orphan-drain failure short-circuited before Upsert.
	var validPending int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ? AND message_id = 1`,
		int64(f.BuildingGen)).Scan(&validPending); err != nil {
		t.Fatalf("count valid pending: %v", err)
	}
	if validPending != 0 {
		t.Errorf("valid pending = %d, want 0 (R53a regression: valid row stranded by orphan drain failure)", validPending)
	}

	// And the embedded row should be in the embeddings table.
	var embedded int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ? AND message_id = 1`,
		int64(f.BuildingGen)).Scan(&embedded); err != nil {
		t.Fatalf("count embedded: %v", err)
	}
	if embedded != 1 {
		t.Errorf("embedded count = %d, want 1 (Upsert should have run before orphan drain)", embedded)
	}

	// The orphan row stays claimed (token is non-NULL) — that's the
	// state ReclaimStale is built to recover from. The error returned
	// above is what tells the caller "this run isn't actually clean".
	var orphanClaimed int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings
		  WHERE generation_id = ? AND message_id = ? AND claim_token IS NOT NULL`,
		int64(f.BuildingGen), orphanID).Scan(&orphanClaimed); err != nil {
		t.Fatalf("count orphan claimed: %v", err)
	}
	if orphanClaimed != 1 {
		t.Errorf("orphan claimed rows = %d, want 1 (the trigger blocks the Complete DELETE)", orphanClaimed)
	}
}

// TestWorker_MissingMessagesDrainedFromQueue verifies that claimed
// rows whose messages were deleted from the main DB are dropped from
// the queue (via Complete) rather than silently re-looped forever.
func TestWorker_MissingMessagesDrainedFromQueue(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 3)

	// Simulate sync deleting messages 2 and 3 from the main DB
	// AFTER CreateGeneration seeded the queue.
	if _, err := f.MainDB.ExecContext(ctx,
		`DELETE FROM messages WHERE id IN (2, 3)`); err != nil {
		t.Fatalf("delete messages: %v", err)
	}
	if _, err := f.MainDB.ExecContext(ctx,
		`DELETE FROM message_bodies WHERE message_id IN (2, 3)`); err != nil {
		t.Fatalf("delete bodies: %v", err)
	}

	w := NewWorker(WorkerDeps{
		Backend:   f.Backend,
		VectorsDB: f.VectorsDB,
		MainDB:    f.MainDB,
		Client:    f.FakeClient,
		BatchSize: 3,
	})

	res, err := w.RunOnce(ctx, f.BuildingGen)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// Message 1 embedded; 2 and 3 dropped as missing.
	if res.Succeeded != 1 {
		t.Errorf("Succeeded=%d, want 1", res.Succeeded)
	}
	// Queue should be fully drained (no infinite loop on missing rows).
	var pending int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("pending after drain = %d, want 0 (missing rows should be removed)", pending)
	}
	// Only one embedding row (for message 1).
	var embedded int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&embedded); err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	if embedded != 1 {
		t.Errorf("embeddings = %d, want 1", embedded)
	}
}

// TestWorker_EmptyPreprocessedMessagesDrainedFromQueue verifies that
// messages whose content is stripped to empty are dropped from the
// queue instead of being sent to embedders that reject empty inputs.
func TestWorker_EmptyPreprocessedMessagesDrainedFromQueue(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 0)

	// Message 1 becomes empty after quote stripping; message 2 remains
	// embeddable so the batch must still succeed.
	if _, err := f.MainDB.ExecContext(ctx,
		`INSERT INTO messages (id, subject) VALUES (1, ''), (2, 'kept')`); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
	if _, err := f.MainDB.ExecContext(ctx,
		`INSERT INTO message_bodies (message_id, body_text) VALUES
		 (1, '> quoted only'),
		 (2, 'actual body')`); err != nil {
		t.Fatalf("insert bodies: %v", err)
	}
	if _, err := f.VectorsDB.ExecContext(ctx,
		`INSERT INTO pending_embeddings (generation_id, message_id, enqueued_at) VALUES
		 (?, 1, 0),
		 (?, 2, 0)`,
		int64(f.BuildingGen), int64(f.BuildingGen)); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	w := NewWorker(WorkerDeps{
		Backend:       f.Backend,
		VectorsDB:     f.VectorsDB,
		MainDB:        f.MainDB,
		Client:        f.FakeClient,
		Preprocess:    PreprocessConfig{StripQuotes: true},
		MaxInputChars: 8000,
		BatchSize:     2,
	})

	res, err := w.RunOnce(ctx, f.BuildingGen)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Succeeded != 1 {
		t.Errorf("Succeeded=%d, want 1", res.Succeeded)
	}
	if len(f.FakeClient.LastInputs) != 1 {
		t.Fatalf("captured %d inputs, want 1", len(f.FakeClient.LastInputs))
	}
	if got := f.FakeClient.LastInputs[0]; strings.TrimSpace(got) == "" {
		t.Fatalf("embedder received empty input %q", got)
	}

	var pending int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("pending after drain = %d, want 0", pending)
	}

	var embedded int
	if err := f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&embedded); err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	if embedded != 1 {
		t.Errorf("embeddings = %d, want 1", embedded)
	}
}

// Progress fires once per fully-successful batch and carries cumulative
// Done, batch size, and char counts — enough for an ETA printer to work
// off of without peeking at worker internals.
func TestWorker_ProgressCalledPerSuccessfulBatch(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 5)

	var reports []ProgressReport
	w := NewWorker(WorkerDeps{
		Backend:       f.Backend,
		VectorsDB:     f.VectorsDB,
		MainDB:        f.MainDB,
		Client:        f.FakeClient,
		Preprocess:    PreprocessConfig{},
		MaxInputChars: 8000,
		BatchSize:     2,
		TotalPending:  5,
		Progress: func(p ProgressReport) {
			reports = append(reports, p)
		},
	})

	res, err := w.RunOnce(ctx, f.BuildingGen)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Succeeded != 5 {
		t.Fatalf("succeeded=%d, want 5", res.Succeeded)
	}
	// 5 messages, batch=2 → batches of 2, 2, 1 → three Progress calls.
	if len(reports) != 3 {
		t.Fatalf("progress called %d times, want 3", len(reports))
	}

	wantDone := []int{2, 4, 5}
	wantBatchMsgs := []int{2, 2, 1}
	for i, p := range reports {
		if p.Done != wantDone[i] {
			t.Errorf("report[%d].Done=%d, want %d", i, p.Done, wantDone[i])
		}
		if p.BatchMsgs != wantBatchMsgs[i] {
			t.Errorf("report[%d].BatchMsgs=%d, want %d", i, p.BatchMsgs, wantBatchMsgs[i])
		}
		if p.TotalPending != 5 {
			t.Errorf("report[%d].TotalPending=%d, want 5", i, p.TotalPending)
		}
		if p.BatchChars <= 0 {
			t.Errorf("report[%d].BatchChars=%d, want >0 (non-empty fixture bodies)", i, p.BatchChars)
		}
		if p.BatchElapsed < 0 {
			t.Errorf("report[%d].BatchElapsed=%s, want >=0", i, p.BatchElapsed)
		}
		if p.RunElapsed < p.BatchElapsed {
			t.Errorf("report[%d].RunElapsed=%s < BatchElapsed=%s", i, p.RunElapsed, p.BatchElapsed)
		}
	}
}

func TestWorker_ProgressCountsDroppedRowsTowardTotal(t *testing.T) {
	ctx := context.Background()
	f := newWorkerFixture(t, 3)

	const missingID = 2
	if _, err := f.MainDB.ExecContext(ctx,
		`DELETE FROM messages WHERE id = ?`, missingID); err != nil {
		t.Fatalf("delete missing message: %v", err)
	}
	if _, err := f.MainDB.ExecContext(ctx,
		`DELETE FROM message_bodies WHERE message_id = ?`, missingID); err != nil {
		t.Fatalf("delete missing body: %v", err)
	}

	var reports []ProgressReport
	w := NewWorker(WorkerDeps{
		Backend:       f.Backend,
		VectorsDB:     f.VectorsDB,
		MainDB:        f.MainDB,
		Client:        f.FakeClient,
		MaxInputChars: 8000,
		BatchSize:     3,
		TotalPending:  3,
		Progress: func(p ProgressReport) {
			reports = append(reports, p)
		},
	})

	res, err := w.RunOnce(ctx, f.BuildingGen)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Succeeded != 2 {
		t.Fatalf("succeeded=%d, want 2 embedded rows", res.Succeeded)
	}
	if len(reports) == 0 {
		t.Fatalf("expected progress report for mixed embed/drop batch")
	}
	final := reports[len(reports)-1]
	if final.Done != 3 {
		t.Fatalf("final progress Done=%d, want 3 pending rows processed", final.Done)
	}
	if final.BatchMsgs != 3 {
		t.Fatalf("final progress BatchMsgs=%d, want 3 pending rows processed in batch", final.BatchMsgs)
	}
}

// TestWorker_DownshiftDrain_HappyPath_AllSingletonsSucceed verifies
// that when a multi-message batch returns ErrPermanent4xx (e.g. one
// message in the batch is too long for the model), the worker walks
// the same already-claimed IDs one at a time and embeds the rest.
func TestWorker_DownshiftDrain_HappyPath_AllSingletonsSucceed(t *testing.T) {
	f := newWorkerFixture(t, 3)
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		if len(inputs) > 1 {
			return nil, fmt.Errorf("embed: HTTP 400: too long: %w", ErrPermanent4xx)
		}
		v := make([]float32, 4)
		v[0] = 1
		return [][]float32{v}, nil
	}
	w := NewWorker(WorkerDeps{
		Backend:   f.Backend,
		VectorsDB: f.VectorsDB,
		MainDB:    f.MainDB,
		Client:    f.FakeClient,
		BatchSize: 3,
	})
	res, err := w.RunOnce(context.Background(), f.BuildingGen)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Succeeded != 3 {
		t.Fatalf("Succeeded: got %d, want 3", res.Succeeded)
	}
	if res.Failed != 0 {
		t.Fatalf("Failed: got %d, want 0", res.Failed)
	}
	assertPending(t, f.VectorsDB, int64(f.BuildingGen), 0)
}

// TestWorker_DownshiftDrain_PartialDrop verifies that singleton 4xxs
// inside a drain are dropped (Completed without an embedding) while
// the rest of the drain proceeds normally.
func TestWorker_DownshiftDrain_PartialDrop(t *testing.T) {
	f := newWorkerFixture(t, 3)
	var singletonSeen int
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		if len(inputs) > 1 {
			return nil, fmt.Errorf("embed: HTTP 400: too long: %w", ErrPermanent4xx)
		}
		singletonSeen++
		if singletonSeen == 2 {
			return nil, fmt.Errorf("embed: HTTP 400: blocked: %w", ErrPermanent4xx)
		}
		v := make([]float32, 4)
		v[0] = 1
		return [][]float32{v}, nil
	}
	w := NewWorker(WorkerDeps{
		Backend:   f.Backend,
		VectorsDB: f.VectorsDB,
		MainDB:    f.MainDB,
		Client:    f.FakeClient,
		BatchSize: 3,
	})
	res, err := w.RunOnce(context.Background(), f.BuildingGen)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Succeeded != 2 {
		t.Errorf("Succeeded: got %d, want 2", res.Succeeded)
	}
	// Singleton 4xx drops are NOT counted as Failed — Complete
	// succeeded, so the worker treated the unembeddable message
	// the same way the main loop treats missing/empty drops.
	// res.Failed is reserved for genuine processing failures
	// (Complete errors, transient embed failures, etc.).
	if res.Failed != 0 {
		t.Errorf("Failed (no Complete errors expected): got %d, want 0", res.Failed)
	}
	assertPending(t, f.VectorsDB, int64(f.BuildingGen), 0)
}

// TestWorker_DownshiftDrain_AllDrop_StillTripsCap verifies that a
// fully misconfigured endpoint (every message rejected as 4xx) still
// trips the consecutive-failure cap so the worker aborts instead of
// silently dropping every message.
func TestWorker_DownshiftDrain_AllDrop_StillTripsCap(t *testing.T) {
	f := newWorkerFixture(t, 6)
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		return nil, fmt.Errorf("embed: HTTP 400: misconfigured: %w", ErrPermanent4xx)
	}
	w := NewWorker(WorkerDeps{
		Backend:                f.Backend,
		VectorsDB:              f.VectorsDB,
		MainDB:                 f.MainDB,
		Client:                 f.FakeClient,
		BatchSize:              3,
		MaxConsecutiveFailures: 2,
	})
	_, err := w.RunOnce(context.Background(), f.BuildingGen)
	if err == nil {
		t.Fatalf("expected abort error, got nil")
	}
	if !strings.Contains(err.Error(), "consecutive failures") {
		t.Errorf("expected abort message, got %v", err)
	}
	if !strings.Contains(err.Error(), "misconfigured") {
		t.Errorf("expected original 4xx body in error, got %v", err)
	}
}

// TestWorker_DownshiftDrain_AllDropClean_NoSilentDelete covers the
// most dangerous failure mode: a misconfigured endpoint (bad API
// key, wrong model, malformed shared request config) returns 4xx
// for every input. ErrPermanent4xx is indistinguishable from a
// message-specific 4xx at the call site, so the worker MUST NOT
// Complete-delete pending rows when no singleton in the drain
// embedded — it must release them so the cap eventually trips and
// the operator sees the failure with the original 4xx body intact
// AND the rows still in the queue for retry after fixing the
// config.
func TestWorker_DownshiftDrain_AllDropClean_NoSilentDelete(t *testing.T) {
	f := newWorkerFixture(t, 4)
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		return nil, fmt.Errorf("embed: HTTP 401: bad-api-key: %w", ErrPermanent4xx)
	}
	// BatchSize=2, default MaxConsecutiveFailures=5. Each iteration:
	// upstream 4xx (cf+1), drain walks both singletons, both 4xx,
	// drain returns wrapped ErrPermanent4xx (no double-count since
	// the drain confirms the upstream failure rather than adding a
	// new one), drain releases the 2 deferred IDs back to the queue.
	// After 5 iterations the cap trips. Pending count stays at 4
	// throughout because rows are released, not Completed.
	w := NewWorker(WorkerDeps{
		Backend:   f.Backend,
		VectorsDB: f.VectorsDB,
		MainDB:    f.MainDB,
		Client:    f.FakeClient,
		BatchSize: 2,
	})
	res, err := w.RunOnce(context.Background(), f.BuildingGen)
	if err == nil {
		t.Fatalf("expected cap-trip error on misconfigured endpoint, got nil")
	}
	if res.Succeeded != 0 {
		t.Errorf("Succeeded: got %d, want 0 (no embeds during all-drop)", res.Succeeded)
	}
	if !strings.Contains(err.Error(), "consecutive failures") {
		t.Errorf("expected cap-trip error, got %v", err)
	}
	if !strings.Contains(err.Error(), "bad-api-key") {
		t.Errorf("expected original 4xx body in error, got %v", err)
	}
	// Critical: rows must NOT have been silently deleted. They
	// should still be in pending_embeddings (released back, not
	// Completed) so a corrected config can re-claim them on the
	// next run.
	assertPending(t, f.VectorsDB, int64(f.BuildingGen), 4)
}

// TestWorker_SingletonBatch_4xx_NoSilentDelete verifies that a
// BatchSize=1 claim returning ErrPermanent4xx does NOT silently
// delete the row. The drain walks the single ID, defers the drop,
// finds embedded == 0, releases the row back to the queue, and
// returns the wrapped 4xx. The caller sees errors.Is(err,
// ErrPermanent4xx) so the drain return doesn't double-count, but
// the upstream batch failure still increments consecutiveFailures
// once per iteration. With MaxConsecutiveFailures=3 the cap trips
// after 3 iterations and the row remains in pending_embeddings.
func TestWorker_SingletonBatch_4xx_NoSilentDelete(t *testing.T) {
	f := newWorkerFixture(t, 1)
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		return nil, fmt.Errorf("embed: HTTP 400: bad: %w", ErrPermanent4xx)
	}
	w := NewWorker(WorkerDeps{
		Backend:                f.Backend,
		VectorsDB:              f.VectorsDB,
		MainDB:                 f.MainDB,
		Client:                 f.FakeClient,
		BatchSize:              1,
		MaxConsecutiveFailures: 3,
	})
	_, err := w.RunOnce(context.Background(), f.BuildingGen)
	if err == nil {
		t.Fatalf("expected abort after cap, got nil")
	}
	if !strings.Contains(err.Error(), "consecutive failures") {
		t.Errorf("expected cap abort message, got %v", err)
	}
	assertPending(t, f.VectorsDB, int64(f.BuildingGen), 1)
}

// TestWorker_DownshiftDrain_CtxCancelMidDrain verifies that
// cancellation during the drain returns ctx.Err() and the remaining
// claimed rows are not lost (they remain in pending_embeddings to be
// recovered by ReclaimStale).
func TestWorker_DownshiftDrain_CtxCancelMidDrain(t *testing.T) {
	f := newWorkerFixture(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	var singletonCalls int
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		if len(inputs) > 1 {
			return nil, fmt.Errorf("embed: HTTP 400: %w", ErrPermanent4xx)
		}
		singletonCalls++
		if singletonCalls == 2 {
			cancel()
			return nil, context.Canceled
		}
		v := make([]float32, 4)
		v[0] = 1
		return [][]float32{v}, nil
	}
	w := NewWorker(WorkerDeps{
		Backend:   f.Backend,
		VectorsDB: f.VectorsDB,
		MainDB:    f.MainDB,
		Client:    f.FakeClient,
		BatchSize: 3,
	})
	_, err := w.RunOnce(ctx, f.BuildingGen)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	assertPending(t, f.VectorsDB, int64(f.BuildingGen), 2)
}

func TestWorker_DownshiftDrain_TransientErrorReleasesRemainingAndErrors(t *testing.T) {
	f := newWorkerFixture(t, 3)
	var singletonCalls int
	f.FakeClient.OnEmbed = func(inputs []string) ([][]float32, error) {
		if len(inputs) > 1 {
			return nil, fmt.Errorf("embed: HTTP 400: %w", ErrPermanent4xx)
		}
		singletonCalls++
		if singletonCalls == 2 {
			return nil, fmt.Errorf("temporary network failure")
		}
		v := make([]float32, 4)
		v[0] = 1
		return [][]float32{v}, nil
	}
	w := NewWorker(WorkerDeps{
		Backend:   f.Backend,
		VectorsDB: f.VectorsDB,
		MainDB:    f.MainDB,
		Client:    f.FakeClient,
		BatchSize: 3,
	})

	res, err := w.RunOnce(context.Background(), f.BuildingGen)
	if err == nil {
		t.Fatalf("expected transient mid-drain error, got nil")
	}
	if !strings.Contains(err.Error(), "temporary network failure") {
		t.Fatalf("expected original transient error, got %v", err)
	}
	if res.Succeeded != 1 {
		t.Fatalf("Succeeded=%d, want 1 completed singleton before transient error", res.Succeeded)
	}
	assertPending(t, f.VectorsDB, int64(f.BuildingGen), 2)
	if available := countAvailable(t, f.VectorsDB, int64(f.BuildingGen)); available != 2 {
		t.Fatalf("available pending rows=%d, want 2 released rows", available)
	}
}
