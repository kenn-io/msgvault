//go:build sqlite_vec

package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// openTestBackend opens a fresh in-memory-ish sqlitevec backend with a
// single pre-seeded message so CreateGeneration has something to enqueue.
func openTestBackend(t *testing.T) *sqlitevec.Backend {
	t.Helper()
	ctx := context.Background()
	requirepkg.NoError(t, sqlitevec.RegisterExtension(), "RegisterExtension")

	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	main, err := sql.Open("sqlite3", mainPath)
	requirepkg.NoError(t, err, "open main")
	t.Cleanup(func() { _ = main.Close() })
	schema := `
CREATE TABLE messages (
    id INTEGER PRIMARY KEY,
    deleted_at DATETIME,
    deleted_from_source_at DATETIME
);`
	_, err = main.Exec(schema)
	requirepkg.NoError(t, err, "schema")
	_, err = main.Exec(`INSERT INTO messages (id) VALUES (1)`)
	requirepkg.NoError(t, err, "seed")
	b, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path:      filepath.Join(dir, "vectors.db"),
		MainPath:  mainPath,
		Dimension: 4,
		MainDB:    main,
	})
	requirepkg.NoError(t, err, "Open")
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// openStderrSink returns a *os.File pointing at /dev/null so
// pickEmbedGeneration's status prints do not clutter test output.
func openStderrSink(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	requirepkg.NoError(t, err, "open /dev/null")
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestPickEmbedGeneration_ResumesBuildingGeneration covers the main
// recovery path: after a partial full-rebuild, running `msgvault
// embed` (without --full-rebuild) must return the existing building
// generation and report rebuildInProgress=true, so activation logic
// still runs when pending drains to zero. Previously this path
// errored out with ErrIndexBuilding.
func TestPickEmbedGeneration_ResumesBuildingGeneration(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	b := openTestBackend(t)

	// Simulate an interrupted full rebuild: a building generation
	// exists but no active generation.
	gen, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(err, "CreateGeneration")

	gotGen, rebuildInProgress, err := pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "fake",
		Dimension:   4,
		Fingerprint: "fake:4",
		Stderr:      openStderrSink(t),
	})
	require.NoError(err, "pickEmbedGeneration (should resume, not error)")
	assert.Equal(gen, gotGen, "gotGen mismatch")
	assert.True(rebuildInProgress, "rebuildInProgress=false, want true (building generation)")
}

// TestPickEmbedGeneration_NoGenerations_HintsFullRebuild covers the
// "fresh install" path: default-mode embed with no generations must
// surface a clear hint rather than silently doing nothing.
func TestPickEmbedGeneration_NoGenerations_HintsFullRebuild(t *testing.T) {
	ctx := context.Background()
	b := openTestBackend(t)

	_, _, err := pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "fake",
		Dimension:   4,
		Fingerprint: "fake:4",
		Stderr:      openStderrSink(t),
	})
	requirepkg.Error(t, err, "expected error when no generations exist")
	// Intentional: we wrap the underlying error with a hint, but the
	// underlying sentinel should still be errors.Is-reachable so
	// upstream callers can branch on it.
	assertpkg.ErrorIs(t, err, vector.ErrNotEnabled, "err should wrap ErrNotEnabled")
}

// TestPickEmbedGeneration_ResumeFingerprintMismatch rejects a resume
// when the in-progress rebuild was started with a different model or
// dimension than the current config — continuing would silently
// embed against the wrong model.
func TestPickEmbedGeneration_ResumeFingerprintMismatch(t *testing.T) {
	ctx := context.Background()
	b := openTestBackend(t)
	_, err := b.CreateGeneration(ctx, "old-model", 4, "")
	requirepkg.NoError(t, err, "CreateGeneration")

	_, _, err = pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "new-model",
		Dimension:   4,
		Fingerprint: "new-model:4",
		Stderr:      openStderrSink(t),
	})
	requirepkg.Error(t, err, "expected fingerprint mismatch error")
	assertpkg.ErrorContains(t, err, "fingerprint", "error should mention fingerprint")
}

// TestPickEmbedGeneration_PrefersBuildingOverActive_MatchingFingerprint
// regression-guards the precedence bug where pickEmbedGeneration
// targeted an existing active generation even when a building
// generation for the configured model was in flight. The user
// expectation is that `msgvault embeddings build` drains the in-progress build
// (so it can be activated) rather than continuing to top up the old
// active generation.
func TestPickEmbedGeneration_PrefersBuildingOverActive_MatchingFingerprint(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	b := openTestBackend(t)

	// Build state: an active generation exists, and a second building
	// generation has been created for the SAME model+dim (the typical
	// "I want to refresh my index" pattern).
	activeGen, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(err, "CreateGeneration (active)")
	require.NoError(b.ActivateGeneration(ctx, activeGen), "ActivateGeneration")
	buildingGen, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(err, "CreateGeneration (building)")

	gotGen, rebuildInProgress, err := pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "fake",
		Dimension:   4,
		Fingerprint: "fake:4",
		Stderr:      openStderrSink(t),
	})
	require.NoError(err, "pickEmbedGeneration")
	assert.Equal(buildingGen, gotGen, "preferring active=%d would leave the build stranded", activeGen)
	assert.True(rebuildInProgress, "rebuildInProgress=false, want true (we picked the building generation)")
}

// TestPickEmbedGeneration_RejectsBuildingWithMismatchedFingerprint
// regression-guards the case where an active generation matches the
// config but a building generation exists for a DIFFERENT model. The
// previous code called ResolveActive first, found the matching active,
// and silently topped it up — leaving the mismatched build stranded
// without any warning. The new precedence-then-mismatch flow should
// either resume a matching build or refuse with a clear error.
func TestPickEmbedGeneration_RejectsBuildingWithMismatchedFingerprint(t *testing.T) {
	ctx := context.Background()
	b := openTestBackend(t)

	// State: building generation exists for an old model. No active
	// generation, and config now points at a different model.
	_, err := b.CreateGeneration(ctx, "old-model", 4, "")
	requirepkg.NoError(t, err, "CreateGeneration (building)")

	_, _, err = pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "new-model",
		Dimension:   4,
		Fingerprint: "new-model:4",
		Stderr:      openStderrSink(t),
	})
	requirepkg.Error(t, err, "expected error for mismatched-fingerprint building generation")
	assertpkg.ErrorContains(t, err, "fingerprint", "error should mention fingerprint")
}

// TestPickEmbedGeneration_StaleActivePlusMatchingBuilding covers the
// "stale active + matching building" combination R51a calls out: an
// older active generation exists with a fingerprint that no longer
// matches the configured model, and a newer building generation
// matches. The configured-model build must be drained instead of the
// stale active one being topped up — otherwise the new build stays
// stuck in `building` indefinitely.
func TestPickEmbedGeneration_StaleActivePlusMatchingBuilding(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	b := openTestBackend(t)

	staleActive, err := b.CreateGeneration(ctx, "old-model", 4, "")
	require.NoError(err, "CreateGeneration (stale active)")
	require.NoError(b.ActivateGeneration(ctx, staleActive), "ActivateGeneration")
	matchingBuilding, err := b.CreateGeneration(ctx, "new-model", 4, "")
	require.NoError(err, "CreateGeneration (matching building)")

	gotGen, rebuildInProgress, err := pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "new-model",
		Dimension:   4,
		Fingerprint: "new-model:4",
		Stderr:      openStderrSink(t),
	})
	require.NoError(err, "pickEmbedGeneration (should resume matching build)")
	assert.Equal(matchingBuilding, gotGen, "stale active=%d must not steal precedence", staleActive)
	assert.True(rebuildInProgress, "rebuildInProgress=false, want true")
}

// TestPickEmbedGeneration_ActivePlusMismatchedBuildingRejected covers
// the case where the active generation matches the configured
// fingerprint AND a building generation exists for a different model.
// Silently topping up the active would leave the wrong-model build
// stranded forever; the user has to explicitly retire or activate it
// before embedding can proceed. Regression for the bug where the code
// only rejected mismatched builds via the ErrIndexBuilding branch and
// missed this active-also-matches case.
func TestPickEmbedGeneration_ActivePlusMismatchedBuildingRejected(t *testing.T) {
	require := requirepkg.New(t)
	ctx := context.Background()
	b := openTestBackend(t)

	matchingActive, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(err, "CreateGeneration (active)")
	require.NoError(b.ActivateGeneration(ctx, matchingActive), "ActivateGeneration")
	_, err = b.CreateGeneration(ctx, "old-model", 4, "")
	require.NoError(err, "CreateGeneration (stale building)")

	_, _, err = pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "fake",
		Dimension:   4,
		Fingerprint: "fake:4",
		Stderr:      openStderrSink(t),
	})
	require.Error(err, "expected error when a mismatched building exists alongside matching active")
	assertpkg.ErrorContains(t, err, "fingerprint", "error should mention fingerprint")
}

// TestPickEmbedGeneration_FullRebuildAbortsWhenDeclined verifies the
// Confirm hook short-circuits when the user declines a rebuild.
func TestPickEmbedGeneration_FullRebuildAbortsWhenDeclined(t *testing.T) {
	ctx := context.Background()
	b := openTestBackend(t)

	_, _, err := pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: true,
		Model:       "fake",
		Dimension:   4,
		Fingerprint: "fake:4",
		Confirm:     func() bool { return false },
		Stderr:      openStderrSink(t),
	})
	requirepkg.Error(t, err, "expected abort error")
}

// TestPickEmbedGeneration_ResumeReseedsUnseededBuilding regression-
// guards the crash-window bug where a process that died between
// inserting the building row and committing the initial seed would
// leave the queue empty; a later `msgvault embeddings build` would then "drain"
// zero rows and silently activate an unseeded generation. The resume
// path must call EnsureSeeded on the matched build before returning,
// reseeding pending_embeddings so the activation gate sees real work
// (or the absence of any) instead of a vacuous empty queue.
func TestPickEmbedGeneration_ResumeReseedsUnseededBuilding(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	b := openTestBackend(t)

	// Step 1: create a building gen the normal way (which seeds + marks
	// seeded_at).
	gen, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(err, "CreateGeneration")

	// Step 2: simulate the crash window — clear pending_embeddings and
	// blank seeded_at so the next resume must reseed. This mirrors the
	// state after a process dies between the building-row insert and
	// the seedPending commit.
	_, err = b.DB().ExecContext(ctx,
		`DELETE FROM pending_embeddings WHERE generation_id = ?`, int64(gen))
	require.NoError(err, "clear pending")
	_, err = b.DB().ExecContext(ctx,
		`UPDATE index_generations SET seeded_at = NULL WHERE id = ?`, int64(gen))
	require.NoError(err, "clear seeded_at")

	// Sanity: pending really is empty before the resume.
	var pendingBefore int
	require.NoError(b.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`, int64(gen)).Scan(&pendingBefore),
		"count pending before")
	require.Equal(0, pendingBefore, "pending count before resume = %d, want 0 (test setup wrong)", pendingBefore)

	// Step 3: run pickEmbedGeneration on the resume path.
	gotGen, rebuildInProgress, err := pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "fake",
		Dimension:   4,
		Fingerprint: "fake:4",
		Stderr:      openStderrSink(t),
	})
	require.NoError(err, "pickEmbedGeneration")
	assert.Equal(gen, gotGen, "gotGen mismatch")
	assert.True(rebuildInProgress, "rebuildInProgress=false, want true")

	// Step 4: pending_embeddings should now contain the message we
	// seeded in openTestBackend (id=1). Without EnsureSeeded on the
	// resume path, this would still be 0.
	var pendingAfter int
	require.NoError(b.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`, int64(gen)).Scan(&pendingAfter),
		"count pending after")
	assert.Equal(1, pendingAfter, "pending count after resume = %d, want 1 (EnsureSeeded should have reseeded)", pendingAfter)
	// And seeded_at should be set so a subsequent resume skips the work.
	var seededAt sql.NullInt64
	require.NoError(b.DB().QueryRowContext(ctx,
		`SELECT seeded_at FROM index_generations WHERE id = ?`, int64(gen)).Scan(&seededAt),
		"read seeded_at")
	assert.True(seededAt.Valid, "seeded_at still NULL after resume, want set")
}

// TestPickEmbedGeneration_ResumeRacesActivation regresses the case
// where the `building` row flips to `active` between the
// BuildingGeneration read and EnsureSeeded. Before the fix this
// surfaced a fatal `ensure seeded: ... state="active"` error even
// though a legitimate active generation (matching the configured
// fingerprint) now existed. After the fix we fall through to the
// active-generation lookup and top it up as a normal incremental
// pass.
func TestPickEmbedGeneration_ResumeRacesActivation(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	b := openTestBackend(t)

	// Create the building generation as if the operator had just run
	// `msgvault embeddings build --full-rebuild`. CreateGeneration seeds pending
	// rows for id=1 via openTestBackend's seed message.
	gen, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(err, "CreateGeneration")
	// Simulate the race: another actor (the daemon, or a concurrent
	// `msgvault embeddings build` run that finished first) activated the
	// generation. From this actor's perspective BuildingGeneration
	// returned non-nil a moment ago, but the state has since flipped.
	require.NoError(b.ActivateGeneration(ctx, gen), "ActivateGeneration")

	// Intercepting the race is hard to do in a single-threaded test,
	// but we can drive the same code path by calling
	// pickEmbedGeneration with a backend that reports the now-active
	// generation when BuildingGeneration is queried. We use a
	// shim that wraps the real backend and overrides only
	// BuildingGeneration.
	shim := &buildingShim{Backend: b, forceBuilding: &vector.Generation{
		ID: gen, Fingerprint: "fake:4", State: vector.GenerationBuilding,
	}}

	gotGen, rebuildInProgress, err := pickEmbedGeneration(ctx, shim, embedGenerationOpts{
		FullRebuild: false,
		Model:       "fake",
		Dimension:   4,
		Fingerprint: "fake:4",
		Stderr:      openStderrSink(t),
	})
	require.NoError(err, "pickEmbedGeneration (race must be retryable, not fatal)")
	assert.Equal(gen, gotGen, "same generation, but now active")
	assert.False(rebuildInProgress, "rebuildInProgress=true, want false (now on the active path)")
}

// buildingShim wraps a real backend, overriding only BuildingGeneration
// to return a forced value. Used by TestPickEmbedGeneration_ResumeRacesActivation
// to simulate a stale read where the generation flipped to active
// underneath us after BuildingGeneration returned.
type buildingShim struct {
	vector.Backend
	forceBuilding *vector.Generation
}

func (s *buildingShim) BuildingGeneration(ctx context.Context) (*vector.Generation, error) {
	return s.forceBuilding, nil
}

func TestNewProgressPrinter_UsesWindowedRate(t *testing.T) {
	assert := assertpkg.New(t)
	var buf bytes.Buffer
	// window=2, total=210 so the percent path runs. The zero
	// interval keeps the test deterministic without sleeping.
	print := newProgressPrinterWithMinInterval(&buf, 210, 2, 0)

	// Three calls. Pick values so the windowed rate at the final
	// event is different from the cumulative rate the old printer
	// would have shown — that way a regression to cumulative would
	// fail the assertion below, not just pass coincidentally.
	//
	//   call 1: Done=100, BatchMsgs=100, BatchElapsed=1s (lastPrint
	//           starts zero, so this emits and Adds).
	//   call 2: Done=200, BatchMsgs=100, BatchElapsed=1s.
	//   call 3: Done=210, BatchMsgs=10, BatchElapsed=5s.
	//
	// After call 3 the window holds the last two samples: (100,1s) and
	// (10,5s) → windowed rate = 110/6 ≈ 18.33 → printed "18 msg/s".
	// The old cumulative implementation would have printed
	// 210/RunElapsed=7s = 30 → "30 msg/s". Asserting on the final
	// line distinguishes the two.
	print(embed.ProgressReport{
		Done: 100, TotalPending: 210,
		BatchMsgs: 100, BatchChars: 1000,
		BatchElapsed: 1 * time.Second,
		RunElapsed:   1 * time.Second,
	})
	print(embed.ProgressReport{
		Done: 200, TotalPending: 210,
		BatchMsgs: 100, BatchChars: 1000,
		BatchElapsed: 1 * time.Second,
		RunElapsed:   2 * time.Second,
	})
	print(embed.ProgressReport{
		Done: 210, TotalPending: 210,
		BatchMsgs: 10, BatchChars: 100,
		BatchElapsed: 5 * time.Second,
		RunElapsed:   7 * time.Second,
	})

	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	requirepkg.GreaterOrEqual(t, len(lines), 2, "expected at least 2 emitted lines, got:\n%s", out)
	finalLine := lines[len(lines)-1]

	assert.Contains(finalLine, "(last 2)", "expected `(last 2)` annotation on final line")
	assert.Contains(finalLine, "18 msg/s", "expected windowed `18 msg/s` on final line")
	assert.NotContains(finalLine, "30 msg/s", "final line shows cumulative rate `30 msg/s`; windowed implementation should not produce this")
}

func TestNewProgressPrinter_DoesNotBypassThrottleAfterInitialTotal(t *testing.T) {
	var buf bytes.Buffer
	print := newProgressPrinter(&buf, 2, 2)

	print(embed.ProgressReport{
		Done: 2, TotalPending: 2,
		BatchMsgs: 2, BatchChars: 20,
		BatchElapsed: 1 * time.Second,
		RunElapsed:   1 * time.Second,
	})
	print(embed.ProgressReport{
		Done: 3, TotalPending: 2,
		BatchMsgs: 1, BatchChars: 10,
		BatchElapsed: 1 * time.Second,
		RunElapsed:   2 * time.Second,
	})

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	requirepkg.Len(t, lines, 1, "progress emitted %d lines, want 1 throttled line after initial total:\n%s", len(lines), buf.String())
}
