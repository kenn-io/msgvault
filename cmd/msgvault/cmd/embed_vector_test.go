//go:build sqlite_vec

package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wesm/msgvault/internal/vector"
	"github.com/wesm/msgvault/internal/vector/embed"
	"github.com/wesm/msgvault/internal/vector/sqlitevec"
)

// openTestBackend opens a fresh in-memory-ish sqlitevec backend with a
// single pre-seeded message so CreateGeneration has something to enqueue.
func openTestBackend(t *testing.T) *sqlitevec.Backend {
	t.Helper()
	ctx := context.Background()
	if err := sqlitevec.RegisterExtension(); err != nil {
		t.Fatalf("RegisterExtension: %v", err)
	}

	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	main, err := sql.Open("sqlite3", mainPath)
	if err != nil {
		t.Fatalf("open main: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	schema := `
CREATE TABLE messages (
    id INTEGER PRIMARY KEY,
    deleted_at DATETIME,
    deleted_from_source_at DATETIME
);`
	if _, err := main.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := main.Exec(`INSERT INTO messages (id) VALUES (1)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	b, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path:      filepath.Join(dir, "vectors.db"),
		MainPath:  mainPath,
		Dimension: 4,
		MainDB:    main,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// openStderrSink returns a *os.File pointing at /dev/null so
// pickEmbedGeneration's status prints do not clutter test output.
func openStderrSink(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
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
	ctx := context.Background()
	b := openTestBackend(t)

	// Simulate an interrupted full rebuild: a building generation
	// exists but no active generation.
	gen, err := b.CreateGeneration(ctx, "fake", 4)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}

	gotGen, rebuildInProgress, err := pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "fake",
		Dimension:   4,
		Fingerprint: "fake:4",
		Stderr:      openStderrSink(t),
	})
	if err != nil {
		t.Fatalf("pickEmbedGeneration: %v (should resume, not error)", err)
	}
	if gotGen != gen {
		t.Errorf("gotGen=%d, want %d", gotGen, gen)
	}
	if !rebuildInProgress {
		t.Errorf("rebuildInProgress=false, want true (building generation)")
	}
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
	if err == nil {
		t.Fatal("expected error when no generations exist")
	}
	if !errors.Is(err, vector.ErrNotEnabled) {
		// Intentional: we wrap the underlying error with a hint, but the
		// underlying sentinel should still be errors.Is-reachable so
		// upstream callers can branch on it.
		t.Errorf("err = %v, want wrapping ErrNotEnabled", err)
	}
}

// TestPickEmbedGeneration_ResumeFingerprintMismatch rejects a resume
// when the in-progress rebuild was started with a different model or
// dimension than the current config — continuing would silently
// embed against the wrong model.
func TestPickEmbedGeneration_ResumeFingerprintMismatch(t *testing.T) {
	ctx := context.Background()
	b := openTestBackend(t)
	if _, err := b.CreateGeneration(ctx, "old-model", 4); err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}

	_, _, err := pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "new-model",
		Dimension:   4,
		Fingerprint: "new-model:4",
		Stderr:      openStderrSink(t),
	})
	if err == nil {
		t.Fatal("expected fingerprint mismatch error, got nil")
	}
	var buf bytes.Buffer
	_, _ = buf.WriteString(err.Error())
	if !bytes.Contains(buf.Bytes(), []byte("fingerprint")) {
		t.Errorf("error should mention fingerprint, got %q", err.Error())
	}
}

// TestPickEmbedGeneration_PrefersBuildingOverActive_MatchingFingerprint
// regression-guards the precedence bug where pickEmbedGeneration
// targeted an existing active generation even when a building
// generation for the configured model was in flight. The user
// expectation is that `msgvault build-embeddings` drains the in-progress build
// (so it can be activated) rather than continuing to top up the old
// active generation.
func TestPickEmbedGeneration_PrefersBuildingOverActive_MatchingFingerprint(t *testing.T) {
	ctx := context.Background()
	b := openTestBackend(t)

	// Build state: an active generation exists, and a second building
	// generation has been created for the SAME model+dim (the typical
	// "I want to refresh my index" pattern).
	activeGen, err := b.CreateGeneration(ctx, "fake", 4)
	if err != nil {
		t.Fatalf("CreateGeneration (active): %v", err)
	}
	if err := b.ActivateGeneration(ctx, activeGen); err != nil {
		t.Fatalf("ActivateGeneration: %v", err)
	}
	buildingGen, err := b.CreateGeneration(ctx, "fake", 4)
	if err != nil {
		t.Fatalf("CreateGeneration (building): %v", err)
	}

	gotGen, rebuildInProgress, err := pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "fake",
		Dimension:   4,
		Fingerprint: "fake:4",
		Stderr:      openStderrSink(t),
	})
	if err != nil {
		t.Fatalf("pickEmbedGeneration: %v", err)
	}
	if gotGen != buildingGen {
		t.Errorf("gotGen=%d, want building=%d (preferring active=%d would leave the build stranded)",
			gotGen, buildingGen, activeGen)
	}
	if !rebuildInProgress {
		t.Errorf("rebuildInProgress=false, want true (we picked the building generation)")
	}
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
	if _, err := b.CreateGeneration(ctx, "old-model", 4); err != nil {
		t.Fatalf("CreateGeneration (building): %v", err)
	}

	_, _, err := pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "new-model",
		Dimension:   4,
		Fingerprint: "new-model:4",
		Stderr:      openStderrSink(t),
	})
	if err == nil {
		t.Fatal("expected error for mismatched-fingerprint building generation, got nil")
	}
	var buf bytes.Buffer
	_, _ = buf.WriteString(err.Error())
	if !bytes.Contains(buf.Bytes(), []byte("fingerprint")) {
		t.Errorf("error should mention fingerprint, got %q", err.Error())
	}
}

// TestPickEmbedGeneration_StaleActivePlusMatchingBuilding covers the
// "stale active + matching building" combination R51a calls out: an
// older active generation exists with a fingerprint that no longer
// matches the configured model, and a newer building generation
// matches. The configured-model build must be drained instead of the
// stale active one being topped up — otherwise the new build stays
// stuck in `building` indefinitely.
func TestPickEmbedGeneration_StaleActivePlusMatchingBuilding(t *testing.T) {
	ctx := context.Background()
	b := openTestBackend(t)

	staleActive, err := b.CreateGeneration(ctx, "old-model", 4)
	if err != nil {
		t.Fatalf("CreateGeneration (stale active): %v", err)
	}
	if err := b.ActivateGeneration(ctx, staleActive); err != nil {
		t.Fatalf("ActivateGeneration: %v", err)
	}
	matchingBuilding, err := b.CreateGeneration(ctx, "new-model", 4)
	if err != nil {
		t.Fatalf("CreateGeneration (matching building): %v", err)
	}

	gotGen, rebuildInProgress, err := pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "new-model",
		Dimension:   4,
		Fingerprint: "new-model:4",
		Stderr:      openStderrSink(t),
	})
	if err != nil {
		t.Fatalf("pickEmbedGeneration: %v (should resume matching build)", err)
	}
	if gotGen != matchingBuilding {
		t.Errorf("gotGen=%d, want building=%d (stale active=%d must not steal precedence)",
			gotGen, matchingBuilding, staleActive)
	}
	if !rebuildInProgress {
		t.Errorf("rebuildInProgress=false, want true")
	}
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
	ctx := context.Background()
	b := openTestBackend(t)

	matchingActive, err := b.CreateGeneration(ctx, "fake", 4)
	if err != nil {
		t.Fatalf("CreateGeneration (active): %v", err)
	}
	if err := b.ActivateGeneration(ctx, matchingActive); err != nil {
		t.Fatalf("ActivateGeneration: %v", err)
	}
	if _, err := b.CreateGeneration(ctx, "old-model", 4); err != nil {
		t.Fatalf("CreateGeneration (stale building): %v", err)
	}

	_, _, err = pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "fake",
		Dimension:   4,
		Fingerprint: "fake:4",
		Stderr:      openStderrSink(t),
	})
	if err == nil {
		t.Fatal("expected error when a mismatched building exists alongside matching active, got nil")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("fingerprint")) {
		t.Errorf("error should mention fingerprint, got %q", err.Error())
	}
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
	if err == nil {
		t.Fatal("expected abort error, got nil")
	}
}

// TestPickEmbedGeneration_ResumeReseedsUnseededBuilding regression-
// guards the crash-window bug where a process that died between
// inserting the building row and committing the initial seed would
// leave the queue empty; a later `msgvault build-embeddings` would then "drain"
// zero rows and silently activate an unseeded generation. The resume
// path must call EnsureSeeded on the matched build before returning,
// reseeding pending_embeddings so the activation gate sees real work
// (or the absence of any) instead of a vacuous empty queue.
func TestPickEmbedGeneration_ResumeReseedsUnseededBuilding(t *testing.T) {
	ctx := context.Background()
	b := openTestBackend(t)

	// Step 1: create a building gen the normal way (which seeds + marks
	// seeded_at).
	gen, err := b.CreateGeneration(ctx, "fake", 4)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}

	// Step 2: simulate the crash window — clear pending_embeddings and
	// blank seeded_at so the next resume must reseed. This mirrors the
	// state after a process dies between the building-row insert and
	// the seedPending commit.
	if _, err := b.DB().ExecContext(ctx,
		`DELETE FROM pending_embeddings WHERE generation_id = ?`, int64(gen)); err != nil {
		t.Fatalf("clear pending: %v", err)
	}
	if _, err := b.DB().ExecContext(ctx,
		`UPDATE index_generations SET seeded_at = NULL WHERE id = ?`, int64(gen)); err != nil {
		t.Fatalf("clear seeded_at: %v", err)
	}

	// Sanity: pending really is empty before the resume.
	var pendingBefore int
	if err := b.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`, int64(gen)).Scan(&pendingBefore); err != nil {
		t.Fatalf("count pending before: %v", err)
	}
	if pendingBefore != 0 {
		t.Fatalf("pending count before resume = %d, want 0 (test setup wrong)", pendingBefore)
	}

	// Step 3: run pickEmbedGeneration on the resume path.
	gotGen, rebuildInProgress, err := pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "fake",
		Dimension:   4,
		Fingerprint: "fake:4",
		Stderr:      openStderrSink(t),
	})
	if err != nil {
		t.Fatalf("pickEmbedGeneration: %v", err)
	}
	if gotGen != gen {
		t.Errorf("gotGen=%d, want %d", gotGen, gen)
	}
	if !rebuildInProgress {
		t.Error("rebuildInProgress=false, want true")
	}

	// Step 4: pending_embeddings should now contain the message we
	// seeded in openTestBackend (id=1). Without EnsureSeeded on the
	// resume path, this would still be 0.
	var pendingAfter int
	if err := b.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`, int64(gen)).Scan(&pendingAfter); err != nil {
		t.Fatalf("count pending after: %v", err)
	}
	if pendingAfter != 1 {
		t.Errorf("pending count after resume = %d, want 1 (EnsureSeeded should have reseeded)", pendingAfter)
	}
	// And seeded_at should be set so a subsequent resume skips the work.
	var seededAt sql.NullInt64
	if err := b.DB().QueryRowContext(ctx,
		`SELECT seeded_at FROM index_generations WHERE id = ?`, int64(gen)).Scan(&seededAt); err != nil {
		t.Fatalf("read seeded_at: %v", err)
	}
	if !seededAt.Valid {
		t.Error("seeded_at still NULL after resume, want set")
	}
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
	ctx := context.Background()
	b := openTestBackend(t)

	// Create the building generation as if the operator had just run
	// `msgvault build-embeddings --full-rebuild`. CreateGeneration seeds pending
	// rows for id=1 via openTestBackend's seed message.
	gen, err := b.CreateGeneration(ctx, "fake", 4)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	// Simulate the race: another actor (the daemon, or a concurrent
	// `msgvault build-embeddings` run that finished first) activated the
	// generation. From this actor's perspective BuildingGeneration
	// returned non-nil a moment ago, but the state has since flipped.
	if err := b.ActivateGeneration(ctx, gen); err != nil {
		t.Fatalf("ActivateGeneration: %v", err)
	}

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
	if err != nil {
		t.Fatalf("pickEmbedGeneration: %v (race must be retryable, not fatal)", err)
	}
	if gotGen != gen {
		t.Errorf("gotGen=%d, want %d (same generation, but now active)", gotGen, gen)
	}
	if rebuildInProgress {
		t.Errorf("rebuildInProgress=true, want false (now on the active path)")
	}
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
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 emitted lines, got:\n%s", out)
	}
	finalLine := lines[len(lines)-1]

	if !strings.Contains(finalLine, "(last 2)") {
		t.Errorf("expected `(last 2)` annotation on final line, got:\n%s", finalLine)
	}
	if !strings.Contains(finalLine, "18 msg/s") {
		t.Errorf("expected windowed `18 msg/s` on final line, got:\n%s", finalLine)
	}
	if strings.Contains(finalLine, "30 msg/s") {
		t.Errorf("final line shows cumulative rate `30 msg/s`; windowed implementation should not produce this:\n%s", finalLine)
	}
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
	if len(lines) != 1 {
		t.Fatalf("progress emitted %d lines, want 1 throttled line after initial total:\n%s", len(lines), buf.String())
	}
}
