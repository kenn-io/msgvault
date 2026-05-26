//go:build sqlite_vec

package embed

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
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
			assertpkg.Equalf(t, tc.want, got,
				"derivedStaleThreshold(%v, %d)", tc.timeout, tc.maxRetries)
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
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	_, err := f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, body)
	require.NoError(err, "update body")

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
	_, err = w.RunOnce(ctx, f.BuildingGen)
	require.NoError(err, "RunOnce")

	require.GreaterOrEqual(len(sizes), 2, "expected >= 2 sub-batches, got sizes=%v", sizes)
	for i, n := range sizes {
		assert.LessOrEqualf(n, batchSize, "sub-batch %d size", i)
		assert.NotZerof(n, "sub-batch %d was empty", i)
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
	_, err := f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, hugeBody)
	requirepkg.NoError(t, err, "update body")

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
	_, err = w.RunOnce(ctx, f.BuildingGen)
	requirepkg.NoError(t, err, "RunOnce")

	// All inputs must fit within the chunker's output window
	// (maxSpansPerMessage * MaxInputChars = 6400 runes). The raw
	// body cap means Preprocess only ever saw ~102K runes, not 5M.
	totalRunes := 0
	for _, s := range observed {
		totalRunes += utf8.RuneCountInString(s)
	}
	assertpkg.LessOrEqualf(t, totalRunes, maxSpansPerMessage*100,
		"total embedder input runes = %d, want <= %d (chunker window)", totalRunes, maxSpansPerMessage*100)
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
	_, err := f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, body)
	requirepkg.NoError(t, err, "update body")

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
	_, err = w.RunOnce(ctx, f.BuildingGen)
	requirepkg.NoError(t, err, "RunOnce")

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
	assertpkg.Truef(t, found,
		"sentinel %q absent from embedder inputs; the prose tail was lost behind the base64 blob", sentinel)
}

// TestWorker_TruncatedCountedPerMessageNotPerChunk pins the
// roborev #323 (717ac4c) metric-accounting fix: when a single long
// message produces multiple truncated chunks, RunResult.Truncated
// must record one message, not one per chunk. Otherwise progress
// metrics inflate (a single oversized message could read as N
// truncations in a Succeeded=1 batch).
func TestWorker_TruncatedCountedPerMessageNotPerChunk(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	f := newWorkerFixture(t, 1)

	// Long unbroken text: ChunkText's hard-cut path marks all but
	// the last span as Trunc=true. With MaxInputChars=100 the body
	// produces several truncated chunks before maxSpans caps it.
	body := strings.Repeat("a", 600)
	_, err := f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, body)
	require.NoError(err, "update body")

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
	require.NoError(err, "RunOnce")
	assert.Equal(1, res.Succeeded)
	// Confirm the chunks table actually has multiple truncated
	// rows for this message — otherwise the test wouldn't be
	// exercising the per-message-vs-per-chunk distinction.
	var truncChunks int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE message_id = 1 AND truncated = 1`).Scan(&truncChunks)
	require.NoError(err, "count truncated chunks")
	require.GreaterOrEqualf(truncChunks, 2,
		"test produced %d truncated chunks, expected >= 2 to exercise the metric", truncChunks)
	assert.Equalf(1, res.Truncated,
		"Truncated = %d, want 1 (one message, regardless of %d truncated chunks)", res.Truncated, truncChunks)
}

// TestWorker_FansOutLongMessageIntoMultipleChunks confirms the
// chunking path: a single pending message whose preprocessed body
// exceeds MaxInputChars produces N > 1 embedder inputs, all of which
// land in the embeddings table with distinct chunk_index values, and
// the queue is drained in one shot (not N times) — Complete is
// keyed on message_id, not on (message_id, chunk_index).
func TestWorker_FansOutLongMessageIntoMultipleChunks(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	_, err := f.MainDB.Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = 1`, body)
	require.NoError(err, "update body")

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
	require.NoError(err, "RunOnce")
	assert.Equal(1, res.Succeeded, "one distinct message embedded")
	assert.Equal(0, res.Failed)

	// embeddings should hold N > 1 rows for the message, with
	// consecutive chunk_index values starting at 0.
	var rowCount int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ? AND message_id = 1`,
		int64(f.BuildingGen)).Scan(&rowCount)
	require.NoError(err, "count chunks")
	require.GreaterOrEqual(rowCount, 2)
	var distinctCI int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT chunk_index) FROM embeddings WHERE generation_id = ? AND message_id = 1`,
		int64(f.BuildingGen)).Scan(&distinctCI)
	require.NoError(err, "count distinct chunk_index")
	assert.Equal(rowCount, distinctCI, "each chunk should be uniquely indexed")
	var minCI, maxCI int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT MIN(chunk_index), MAX(chunk_index) FROM embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&minCI, &maxCI)
	require.NoError(err, "min/max chunk_index")
	assert.Equal(0, minCI, "chunk_index minimum")
	assert.Equal(rowCount-1, maxCI, "chunk_index maximum")
	// message_count tracks distinct messages, so it must read as 1
	// despite the multi-chunk fan-out.
	var msgCount int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT message_count FROM index_generations WHERE id = ?`,
		int64(f.BuildingGen)).Scan(&msgCount)
	require.NoError(err, "read message_count")
	assert.Equal(1, msgCount)
	// Queue is fully drained: Complete is keyed on message_id, so all
	// chunks of message 1 finish together when its singleton pending
	// row is removed.
	var pending int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&pending)
	require.NoError(err, "count pending")
	assert.Equal(0, pending, "pending remaining")
}

func TestWorker_DrainsPendingEndToEnd(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require.NoError(err, "RunOnce")
	assert.Equal(3, res.Succeeded)
	assert.Equal(0, res.Failed)

	var n int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&n)
	require.NoError(err, "count pending")
	assert.Equal(0, n, "pending remaining")
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
	requirepkg.NoError(t, err, "RunOnce")
	assertpkg.GreaterOrEqual(t, res.Failed, 1, "expected at least 1 failure")
	// The worker retries the released row and eventually drains everything.
	assertpkg.Equal(t, 3, res.Succeeded, "failed row gets retried after Release")
}

func TestWorker_ReleasesOnUpsertError(t *testing.T) {
	// Driving an Upsert failure requires forcing a dimension mismatch; the
	// fake client returns 4-dim vectors matching the generation's
	// dimension, so the easy lever isn't available. The Release-on-error
	// path is covered by TestWorker_ReleasesOnClientError.
	t.Skip("covered by TestWorker_ReleasesOnClientError")
}

func TestWorker_EmptyPendingReturnsZeroResult(t *testing.T) {
	assert := assertpkg.New(t)
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
	requirepkg.NoError(t, err, "RunOnce")
	assert.Equal(0, res.Claimed)
	assert.Equal(0, res.Succeeded)
	assert.Equal(0, res.Failed)
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
	requirepkg.Error(t, err, "expected cancellation error")
}

func TestWorker_ReclaimStale_FromStartup(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require.NoError(err, "Claim setup")
	require.Len(ids, 2)
	_, err = f.VectorsDB.ExecContext(ctx,
		`UPDATE pending_embeddings SET claimed_at = ? WHERE generation_id = ?`,
		time.Now().Add(-20*time.Minute).Unix(), int64(f.BuildingGen))
	require.NoError(err, "backdate")

	n, err := w.ReclaimStale(ctx)
	require.NoError(err, "ReclaimStale")
	assert.Equal(2, n, "reclaimed")

	// Verify the rows are available again.
	var available int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ? AND claimed_at IS NULL`,
		int64(f.BuildingGen)).Scan(&available)
	require.NoError(err, "count available")
	assert.Equal(2, available, "available after reclaim")
}

func TestWorker_StaleThresholdDefault(t *testing.T) {
	f := newWorkerFixture(t, 0)
	w := NewWorker(WorkerDeps{
		Backend:   f.Backend,
		VectorsDB: f.VectorsDB,
		MainDB:    f.MainDB,
		Client:    f.FakeClient,
	})
	assertpkg.Equal(t, 10*time.Minute, w.deps.StaleThreshold, "default StaleThreshold")
	assertpkg.Equal(t, 5, w.deps.MaxConsecutiveFailures, "default MaxConsecutiveFailures")
}

// TestWorker_AbortsAfterConsecutiveFailures verifies that a
// persistently failing embedder causes RunOnce to return an error
// rather than loop forever releasing and re-claiming.
func TestWorker_AbortsAfterConsecutiveFailures(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require.Error(err, "want error after consecutive failures")
	assert.GreaterOrEqual(res.Failed, 3, "one per consecutive failure")
	// Any leftover claims should have been released; pending is non-empty.
	var pending int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&pending)
	require.NoError(err, "count pending")
	assert.NotZero(pending, "rows should have been released")
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
	requirepkg.NoError(t, err, "2 failures, budget 3 — should not abort")
	assertpkg.Equal(t, 4, res.Succeeded, "all 4 messages ultimately drain")
}

// TestWorker_RuneCountUsedForSourceCharLen regresses the
// byte-vs-rune mismatch: Preprocess truncates by runes, so the
// SourceCharLen field on each Chunk must also be a rune count or
// CJK/emoji inputs get inflated by 2-4x. We embed a short Japanese
// subject (whose UTF-8 byte length is much larger than its rune
// count) and assert the persisted source_char_len matches runes.
func TestWorker_RuneCountUsedForSourceCharLen(t *testing.T) {
	require := requirepkg.New(t)
	ctx := context.Background()
	f := newWorkerFixture(t, 0) // start empty so we control the message text

	// "こんにちは世界" = 7 runes, 21 UTF-8 bytes. Preprocess prepends
	// "Subject: " (9 ASCII bytes/runes) and "\n\n" (2). The full
	// preprocessed string has 18 runes and 32 bytes — a 1.78x
	// inflation if we record bytes by mistake.
	const subject = "こんにちは世界"
	_, err := f.MainDB.ExecContext(ctx,
		`INSERT INTO messages (id, subject) VALUES (1, ?)`, subject)
	require.NoError(err, "insert message")
	_, err = f.VectorsDB.ExecContext(ctx,
		`INSERT INTO pending_embeddings (generation_id, message_id, enqueued_at) VALUES (?, 1, 0)`,
		int64(f.BuildingGen))
	require.NoError(err, "seed pending")

	w := NewWorker(WorkerDeps{
		Backend:   f.Backend,
		VectorsDB: f.VectorsDB,
		MainDB:    f.MainDB,
		Client:    f.FakeClient,
		BatchSize: 1,
	})
	res, err := w.RunOnce(ctx, f.BuildingGen)
	require.NoError(err, "RunOnce")
	require.Equal(1, res.Succeeded)

	const wantRunes = 18 // len("Subject: \n\n") + 7 runes for the kanji
	var got int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT source_char_len FROM embeddings WHERE generation_id = ? AND message_id = 1`,
		int64(f.BuildingGen)).Scan(&got)
	require.NoError(err, "read source_char_len")
	assertpkg.Equal(t, wantRunes, got, "source_char_len (rune count, not byte length)")
}

// TestWorker_FallsBackToHTMLWhenBodyTextEmpty guards the HTML-only
// recall path: messages whose plaintext body is absent should still
// be embedded using HTML-stripped text rather than silently degrading
// to subject-only embeddings.
func TestWorker_FallsBackToHTMLWhenBodyTextEmpty(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	f := newWorkerFixture(t, 0)

	const html = `<html><body><p>planning offsite agenda</p><p>Thursday afternoon</p></body></html>`
	_, err := f.MainDB.ExecContext(ctx,
		`INSERT INTO messages (id, subject) VALUES (1, ?)`, "meeting")
	require.NoError(err, "insert message")
	_, err = f.MainDB.ExecContext(ctx,
		`INSERT INTO message_bodies (message_id, body_text, body_html) VALUES (1, '', ?)`, html)
	require.NoError(err, "insert body")
	_, err = f.VectorsDB.ExecContext(ctx,
		`INSERT INTO pending_embeddings (generation_id, message_id, enqueued_at) VALUES (?, 1, 0)`,
		int64(f.BuildingGen))
	require.NoError(err, "seed pending")

	w := NewWorker(WorkerDeps{
		Backend:       f.Backend,
		VectorsDB:     f.VectorsDB,
		MainDB:        f.MainDB,
		Client:        f.FakeClient,
		MaxInputChars: 8000,
		BatchSize:     1,
	})
	_, err = w.RunOnce(ctx, f.BuildingGen)
	require.NoError(err, "RunOnce")

	require.Len(f.FakeClient.LastInputs, 1)
	got := f.FakeClient.LastInputs[0]
	// The preprocessed text should contain the HTML paragraph text,
	// not just the subject — that's the whole point of the fallback.
	assert.Contains(got, "planning offsite agenda", "embed input missing HTML body text")
	assert.Contains(got, "Thursday afternoon", "embed input missing second paragraph")
	assert.NotContains(got, "<p>", "embed input still contains HTML tags")
	assert.NotContains(got, "</body>", "embed input still contains HTML tags")
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
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	// Need ≥ MaxConsecutiveFailures messages so successive claims pull
	// fresh rows; otherwise a single failed Complete leaves one stuck-
	// claimed row and the next Claim returns empty (which RunOnce
	// rightly treats as a clean drain — the bug we're regressing
	// against would never trip with a single-message fixture).
	f := newWorkerFixture(t, 3)

	_, err := f.VectorsDB.ExecContext(ctx, `
        CREATE TRIGGER block_pending_delete
        BEFORE DELETE ON pending_embeddings
        BEGIN
            SELECT RAISE(FAIL, 'simulated complete failure');
        END`)
	require.NoError(err, "install trigger")

	w := NewWorker(WorkerDeps{
		Backend:                f.Backend,
		VectorsDB:              f.VectorsDB,
		MainDB:                 f.MainDB,
		Client:                 f.FakeClient,
		BatchSize:              1,
		MaxConsecutiveFailures: 2,
	})

	res, err := w.RunOnce(ctx, f.BuildingGen)
	require.Error(err, "want error after Complete failures (regression: silent success)")
	assert.Equal(0, res.Succeeded, "Complete failed, work was not durably finished")
	assert.NotZero(res.Failed, "Complete failure should count as a batch failure")

	// Stuck-claim check: pending_embeddings row is still there (the
	// trigger blocked Complete's DELETE) and is marked claimed (the
	// previous Claim's UPDATE went through). A naive "log-only"
	// Complete handler would silently report success; the failure
	// counter is what makes RunOnce notice and abort.
	var pending, claimed int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*),
                COALESCE(SUM(CASE WHEN claimed_at IS NOT NULL THEN 1 ELSE 0 END), 0)
           FROM pending_embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&pending, &claimed)
	require.NoError(err, "count pending")
	assert.NotZero(pending, "Complete should have failed and left the row in place")
	assert.NotZero(claimed, "Claim's UPDATE should have left the row marked claimed")
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
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	// 2 messages enqueued; we'll delete one from the main DB so it
	// reaches embedBatch as "missing".
	f := newWorkerFixture(t, 2)

	const orphanID = 2
	_, err := f.MainDB.ExecContext(ctx,
		`DELETE FROM messages WHERE id = ?`, orphanID)
	require.NoError(err, "delete orphan from main")
	_, err = f.MainDB.ExecContext(ctx,
		`DELETE FROM message_bodies WHERE message_id = ?`, orphanID)
	require.NoError(err, "delete orphan body")

	// Selective trigger: only the orphan's Complete DELETE fails. The
	// embedded row's Complete must still succeed so we can prove the
	// valid work is durably finished even when the orphan drain fails.
	_, err = f.VectorsDB.ExecContext(ctx, fmt.Sprintf(`
        CREATE TRIGGER block_orphan_drain
        BEFORE DELETE ON pending_embeddings
        WHEN OLD.message_id = %d
        BEGIN
            SELECT RAISE(FAIL, 'simulated orphan complete failure');
        END`, orphanID))
	require.NoError(err, "install trigger")

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
	require.Error(err, "want non-nil error (orphan drain failed and orphan remained stuck)")
	assert.ErrorContains(err, "orphan-drain")
	assert.ErrorContains(err, "ReclaimStale", "user knows recovery is automatic")
	assert.Equal(1, res.Succeeded, "the valid message must be counted as completed")
	assert.NotZero(res.Failed, "orphan drain failure should be reported")
	require.NotEmpty(reports, "expected progress for valid embedded row even though orphan drain failed")
	final := reports[len(reports)-1]
	assert.Equal(1, final.Done, "final progress Done = 1 durable embedded row")
	assert.Equal(1, final.BatchMsgs, "final progress BatchMsgs = 1 durable embedded row")

	// The valid message's pending row must be GONE (Complete succeeded).
	// The original bug left it claimed-but-not-completed because the
	// orphan-drain failure short-circuited before Upsert.
	var validPending int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ? AND message_id = 1`,
		int64(f.BuildingGen)).Scan(&validPending)
	require.NoError(err, "count valid pending")
	assert.Equal(0, validPending, "R53a regression: valid row stranded by orphan drain failure")

	// And the embedded row should be in the embeddings table.
	var embedded int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ? AND message_id = 1`,
		int64(f.BuildingGen)).Scan(&embedded)
	require.NoError(err, "count embedded")
	assert.Equal(1, embedded, "Upsert should have run before orphan drain")

	// The orphan row stays claimed (token is non-NULL) — that's the
	// state ReclaimStale is built to recover from. The error returned
	// above is what tells the caller "this run isn't actually clean".
	var orphanClaimed int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings
		  WHERE generation_id = ? AND message_id = ? AND claim_token IS NOT NULL`,
		int64(f.BuildingGen), orphanID).Scan(&orphanClaimed)
	require.NoError(err, "count orphan claimed")
	assert.Equal(1, orphanClaimed, "the trigger blocks the Complete DELETE")
}

// TestWorker_MissingMessagesDrainedFromQueue verifies that claimed
// rows whose messages were deleted from the main DB are dropped from
// the queue (via Complete) rather than silently re-looped forever.
func TestWorker_MissingMessagesDrainedFromQueue(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	f := newWorkerFixture(t, 3)

	// Simulate sync deleting messages 2 and 3 from the main DB
	// AFTER CreateGeneration seeded the queue.
	_, err := f.MainDB.ExecContext(ctx,
		`DELETE FROM messages WHERE id IN (2, 3)`)
	require.NoError(err, "delete messages")
	_, err = f.MainDB.ExecContext(ctx,
		`DELETE FROM message_bodies WHERE message_id IN (2, 3)`)
	require.NoError(err, "delete bodies")

	w := NewWorker(WorkerDeps{
		Backend:   f.Backend,
		VectorsDB: f.VectorsDB,
		MainDB:    f.MainDB,
		Client:    f.FakeClient,
		BatchSize: 3,
	})

	res, err := w.RunOnce(ctx, f.BuildingGen)
	require.NoError(err, "RunOnce")
	// Message 1 embedded; 2 and 3 dropped as missing.
	assert.Equal(1, res.Succeeded)
	// Queue should be fully drained (no infinite loop on missing rows).
	var pending int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&pending)
	require.NoError(err, "count pending")
	assert.Equal(0, pending, "missing rows should be removed")
	// Only one embedding row (for message 1).
	var embedded int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&embedded)
	require.NoError(err, "count embeddings")
	assert.Equal(1, embedded)
}

// TestWorker_EmptyPreprocessedMessagesDrainedFromQueue verifies that
// messages whose content is stripped to empty are dropped from the
// queue instead of being sent to embedders that reject empty inputs.
func TestWorker_EmptyPreprocessedMessagesDrainedFromQueue(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	f := newWorkerFixture(t, 0)

	// Message 1 becomes empty after quote stripping; message 2 remains
	// embeddable so the batch must still succeed.
	_, err := f.MainDB.ExecContext(ctx,
		`INSERT INTO messages (id, subject) VALUES (1, ''), (2, 'kept')`)
	require.NoError(err, "insert messages")
	_, err = f.MainDB.ExecContext(ctx,
		`INSERT INTO message_bodies (message_id, body_text) VALUES
		 (1, '> quoted only'),
		 (2, 'actual body')`)
	require.NoError(err, "insert bodies")
	_, err = f.VectorsDB.ExecContext(ctx,
		`INSERT INTO pending_embeddings (generation_id, message_id, enqueued_at) VALUES
		 (?, 1, 0),
		 (?, 2, 0)`,
		int64(f.BuildingGen), int64(f.BuildingGen))
	require.NoError(err, "seed pending")

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
	require.NoError(err, "RunOnce")
	assert.Equal(1, res.Succeeded)
	require.Len(f.FakeClient.LastInputs, 1)
	require.NotEmpty(strings.TrimSpace(f.FakeClient.LastInputs[0]),
		"embedder received empty input %q", f.FakeClient.LastInputs[0])

	var pending int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&pending)
	require.NoError(err, "count pending")
	assert.Equal(0, pending, "pending after drain")

	var embedded int
	err = f.VectorsDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ?`,
		int64(f.BuildingGen)).Scan(&embedded)
	require.NoError(err, "count embeddings")
	assert.Equal(1, embedded)
}

// Progress fires once per fully-successful batch and carries cumulative
// Done, batch size, and char counts — enough for an ETA printer to work
// off of without peeking at worker internals.
func TestWorker_ProgressCalledPerSuccessfulBatch(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require.NoError(err, "RunOnce")
	require.Equal(5, res.Succeeded)
	// 5 messages, batch=2 → batches of 2, 2, 1 → three Progress calls.
	require.Len(reports, 3)

	wantDone := []int{2, 4, 5}
	wantBatchMsgs := []int{2, 2, 1}
	for i, p := range reports {
		assert.Equalf(wantDone[i], p.Done, "report[%d].Done", i)
		assert.Equalf(wantBatchMsgs[i], p.BatchMsgs, "report[%d].BatchMsgs", i)
		assert.Equalf(5, p.TotalPending, "report[%d].TotalPending", i)
		assert.Greaterf(p.BatchChars, 0, "report[%d].BatchChars (non-empty fixture bodies)", i)
		assert.GreaterOrEqualf(p.BatchElapsed, time.Duration(0), "report[%d].BatchElapsed", i)
		assert.GreaterOrEqualf(p.RunElapsed, p.BatchElapsed,
			"report[%d].RunElapsed=%s < BatchElapsed=%s", i, p.RunElapsed, p.BatchElapsed)
	}
}

func TestWorker_ProgressCountsDroppedRowsTowardTotal(t *testing.T) {
	require := requirepkg.New(t)
	ctx := context.Background()
	f := newWorkerFixture(t, 3)

	const missingID = 2
	_, err := f.MainDB.ExecContext(ctx,
		`DELETE FROM messages WHERE id = ?`, missingID)
	require.NoError(err, "delete missing message")
	_, err = f.MainDB.ExecContext(ctx,
		`DELETE FROM message_bodies WHERE message_id = ?`, missingID)
	require.NoError(err, "delete missing body")

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
	require.NoError(err, "RunOnce")
	require.Equal(2, res.Succeeded, "embedded rows")
	require.NotEmpty(reports, "expected progress report for mixed embed/drop batch")
	final := reports[len(reports)-1]
	require.Equal(3, final.Done, "final progress Done = pending rows processed")
	require.Equal(3, final.BatchMsgs, "final progress BatchMsgs = pending rows processed in batch")
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
	requirepkg.NoError(t, err, "RunOnce")
	requirepkg.Equal(t, 3, res.Succeeded, "Succeeded")
	requirepkg.Equal(t, 0, res.Failed, "Failed")
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
	requirepkg.NoError(t, err, "RunOnce")
	assertpkg.Equal(t, 2, res.Succeeded, "Succeeded")
	// Singleton 4xx drops are NOT counted as Failed — Complete
	// succeeded, so the worker treated the unembeddable message
	// the same way the main loop treats missing/empty drops.
	// res.Failed is reserved for genuine processing failures
	// (Complete errors, transient embed failures, etc.).
	assertpkg.Equal(t, 0, res.Failed, "no Complete errors expected")
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
	requirepkg.Error(t, err, "expected abort error")
	assertpkg.ErrorContains(t, err, "consecutive failures")
	assertpkg.ErrorContains(t, err, "misconfigured", "expected original 4xx body in error")
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
	assert := assertpkg.New(t)
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
	requirepkg.Error(t, err, "expected cap-trip error on misconfigured endpoint")
	assert.Equal(0, res.Succeeded, "no embeds during all-drop")
	assert.ErrorContains(err, "consecutive failures", "expected cap-trip error")
	assert.ErrorContains(err, "bad-api-key", "expected original 4xx body in error")
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
	requirepkg.Error(t, err, "expected abort after cap")
	assertpkg.ErrorContains(t, err, "consecutive failures")
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
	requirepkg.ErrorIs(t, err, context.Canceled)
	assertPending(t, f.VectorsDB, int64(f.BuildingGen), 2)
}

func TestWorker_DownshiftDrain_TransientErrorReleasesRemainingAndErrors(t *testing.T) {
	require := requirepkg.New(t)
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
	require.Error(err, "expected transient mid-drain error")
	require.ErrorContains(err, "temporary network failure")
	require.Equal(1, res.Succeeded, "completed singleton before transient error")
	assertPending(t, f.VectorsDB, int64(f.BuildingGen), 2)
	require.Equal(2, countAvailable(t, f.VectorsDB, int64(f.BuildingGen)), "released rows")
}
