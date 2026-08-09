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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

type cliActivationBackend struct {
	vector.Backend

	activateErr   error
	activateCalls []vector.GenerationID
	sequences     []int64
}

func (b *cliActivationBackend) ActivateGenerationIfConverged(
	_ context.Context, gen vector.GenerationID, expectedSequence int64,
) error {
	b.activateCalls = append(b.activateCalls, gen)
	b.sequences = append(b.sequences, expectedSequence)
	return b.activateErr
}

func (b *cliActivationBackend) ActivateGeneration(_ context.Context, gen vector.GenerationID, force bool) error {
	if force {
		panic("CLI build activation must never force")
	}
	b.activateCalls = append(b.activateCalls, gen)
	return b.activateErr
}

type cliConvergenceChecker struct {
	state scheduler.ConvergenceResult
	err   error
}

type cliPassRunner struct {
	results []embed.RunResult
	calls   int
}

func (r *cliPassRunner) RunOnce(context.Context, vector.GenerationID) (embed.RunResult, error) {
	result := r.results[min(r.calls, len(r.results)-1)]
	r.calls++
	return result, nil
}

func (r *cliPassRunner) RunBackstop(context.Context, vector.GenerationID) (embed.RunResult, error) {
	return embed.RunResult{}, errors.New("unexpected backstop")
}

func (r *cliPassRunner) ReclaimStale(context.Context) (int, error) { return 0, nil }

func TestRunEmbeddingPasses_ContextualCLIContinuesUntilConverged(t *testing.T) {
	runner := &cliPassRunner{results: []embed.RunResult{
		{Claimed: 2, Succeeded: 2, Contextual: &embed.ContextConvergence{Converged: false}},
		{Claimed: 3, Succeeded: 3, Contextual: &embed.ContextConvergence{Converged: false}},
		{Claimed: 1, Succeeded: 1, Contextual: &embed.ContextConvergence{Converged: true}},
	}}

	result, err := runEmbeddingPasses(t.Context(), runner, 7, false, vector.APIFormatVoyageContextual)
	require.NoError(t, err)
	assert.Equal(t, 3, runner.calls)
	assert.Equal(t, 6, result.Claimed)
	assert.Equal(t, 6, result.Succeeded)
	require.NotNil(t, result.Contextual)
	assert.True(t, result.Contextual.Converged)
}

func TestRunEmbeddingPasses_ContextualCLIRejectsNonProgress(t *testing.T) {
	runner := &cliPassRunner{results: []embed.RunResult{{
		Contextual: &embed.ContextConvergence{Converged: false},
	}}}

	_, err := runEmbeddingPasses(t.Context(), runner, 7, false, vector.APIFormatVoyageContextual)
	require.ErrorContains(t, err, "made no progress")
	assert.Equal(t, 1, runner.calls)
}

func (c cliConvergenceChecker) CheckConvergence(context.Context, vector.GenerationID) (scheduler.ConvergenceResult, error) {
	return c.state, c.err
}

func TestActivateBuiltGeneration_ContextualBehavior(t *testing.T) {
	complete := scheduler.ConvergenceResult{
		MessageCoverageComplete: true,
		LatestJournalSequence:   9,
		ConsumedJournalSequence: 9,
		ReconciliationComplete:  true,
	}
	tests := []struct {
		name   string
		mutate func(*scheduler.ConvergenceResult)
		want   string
	}{
		{name: "message coverage incomplete", mutate: func(s *scheduler.ConvergenceResult) {
			s.MessageCoverageComplete = false
			s.MessageCoverageMissing = 2
		}, want: "message_coverage_complete=false (missing=2)"},
		{name: "journal incomplete", mutate: func(s *scheduler.ConvergenceResult) {
			s.ConsumedJournalSequence = 8
		}, want: "journal=8/9"},
		{name: "reconciliation incomplete", mutate: func(s *scheduler.ConvergenceResult) {
			s.ReconciliationComplete = false
		}, want: "reconciliation_complete=false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := complete
			tt.mutate(&state)
			backend := &cliActivationBackend{}
			var stdout, stderr bytes.Buffer
			activated, err := activateBuiltGeneration(t.Context(), backend,
				cliConvergenceChecker{state: state}, 7, vector.APIFormatVoyageContextual,
				&stdout, &stderr)
			require.NoError(t, err)
			assert.False(t, activated)
			assert.Empty(t, backend.activateCalls)
			assert.Empty(t, stdout.String())
			assert.Contains(t, stderr.String(), tt.want)
			assert.Contains(t, stderr.String(), "generation 7 has not converged")
		})
	}

	backend := &cliActivationBackend{}
	var stdout, stderr bytes.Buffer
	activated, err := activateBuiltGeneration(t.Context(), backend,
		cliConvergenceChecker{state: complete}, 7, vector.APIFormatVoyageContextual,
		&stdout, &stderr)
	require.NoError(t, err)
	assert.True(t, activated)
	assert.Equal(t, []vector.GenerationID{7}, backend.activateCalls)
	assert.Equal(t, []int64{9}, backend.sequences)
	assert.Equal(t, "Generation 7 activated.\n", stdout.String())
	assert.Empty(t, stderr.String())
}

func TestActivateBuiltGeneration_OpenAILegacyHintUnchanged(t *testing.T) {
	backend := &cliActivationBackend{}
	state := scheduler.ConvergenceResult{
		MessageCoverageComplete: false,
		MessageCoverageMissing:  3,
		ReconciliationComplete:  true,
	}
	var stdout, stderr bytes.Buffer
	activated, err := activateBuiltGeneration(t.Context(), backend,
		cliConvergenceChecker{state: state}, 7, vector.APIFormatOpenAI, &stdout, &stderr)
	require.NoError(t, err)
	assert.False(t, activated)
	assert.Empty(t, backend.activateCalls)
	assert.Equal(t, remainingCoverageHint(7, 3), stderr.String())
}

func TestActivateBuiltGeneration_OpenAICompleteUsesLegacyActivation(t *testing.T) {
	backend := &cliActivationBackend{}
	state := scheduler.ConvergenceResult{
		MessageCoverageComplete: true,
		ReconciliationComplete:  true,
	}
	var stdout, stderr bytes.Buffer
	activated, err := activateBuiltGeneration(t.Context(), backend,
		cliConvergenceChecker{state: state}, 7, vector.APIFormatOpenAI, &stdout, &stderr)
	require.NoError(t, err)
	assert.True(t, activated)
	assert.Equal(t, []vector.GenerationID{7}, backend.activateCalls)
	assert.Empty(t, backend.sequences, "legacy builds must not require contextual document progress")
	assert.Equal(t, "Generation 7 activated.\n", stdout.String())
	assert.Empty(t, stderr.String())
}

func TestActivateBuiltGeneration_ContextualLifecycleErrorsDoNotActivateAnotherGeneration(t *testing.T) {
	complete := scheduler.ConvergenceResult{
		MessageCoverageComplete: true,
		ReconciliationComplete:  true,
	}
	t.Run("checker rejects retired generation", func(t *testing.T) {
		backend := &cliActivationBackend{}
		activated, err := activateBuiltGeneration(t.Context(), backend,
			cliConvergenceChecker{err: vector.ErrGenerationRetired}, 7,
			vector.APIFormatVoyageContextual, &bytes.Buffer{}, &bytes.Buffer{})
		assert.False(t, activated)
		require.ErrorIs(t, err, vector.ErrGenerationRetired)
		assert.Empty(t, backend.activateCalls)
	})
	t.Run("backend rejects retired generation", func(t *testing.T) {
		backend := &cliActivationBackend{activateErr: vector.ErrGenerationRetired}
		activated, err := activateBuiltGeneration(t.Context(), backend,
			cliConvergenceChecker{state: complete}, 7,
			vector.APIFormatVoyageContextual, &bytes.Buffer{}, &bytes.Buffer{})
		assert.False(t, activated)
		require.ErrorIs(t, err, vector.ErrGenerationRetired)
		assert.Equal(t, []vector.GenerationID{7}, backend.activateCalls)
		assert.Equal(t, []int64{0}, backend.sequences)
	})
}

func setupVectorFeaturesFixture(t *testing.T, apiFormat vector.EmbeddingAPIFormat, readOnly bool) *vectorFeatures {
	t.Helper()
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "msgvault.db")
	c := config.NewDefaultConfig()
	c.Data.DataDir = dir
	c.Vector.Enabled = true
	c.Vector.DBPath = filepath.Join(dir, "vectors.db")
	c.Vector.Embeddings.Endpoint = "https://example.invalid/v1"
	c.Vector.Embeddings.Model = "text-embedding-test"
	c.Vector.Embeddings.Dimension = 4
	c.Vector.Embeddings.APIFormat = apiFormat
	if apiFormat == vector.APIFormatVoyageContextual {
		c.Vector.Embeddings.Model = "voyage-context-4"
	}
	withTestConfig(t, c)

	s, err := store.Open(mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.InitSchema())

	vf, err := setupVectorFeatures(t.Context(), s, mainPath, readOnly)
	require.NoError(t, err)
	require.NotNil(t, vf)
	t.Cleanup(func() { _ = vf.Close() })
	return vf
}

func TestSetupVectorFeatures_SelectsRunnerByAPIFormat(t *testing.T) {
	t.Run("implicit OpenAI", func(t *testing.T) {
		vf := setupVectorFeaturesFixture(t, "", false)
		assert.IsType(t, &embed.Worker{}, vf.Runner)
		assert.IsType(t, &legacyConvergenceChecker{}, vf.Convergence)
	})
	t.Run("explicit OpenAI", func(t *testing.T) {
		vf := setupVectorFeaturesFixture(t, vector.APIFormatOpenAI, false)
		assert.IsType(t, &embed.Worker{}, vf.Runner)
		assert.IsType(t, &legacyConvergenceChecker{}, vf.Convergence)
	})
	t.Run("Voyage contextual", func(t *testing.T) {
		vf := setupVectorFeaturesFixture(t, vector.APIFormatVoyageContextual, false)
		assert.IsType(t, &embed.ContextWorker{}, vf.Runner)
		assert.IsType(t, &contextualConvergenceChecker{}, vf.Convergence)
	})
}

func TestSetupVectorFeatures_ReadOnlyContextualDoesNotStartRunnerWrites(t *testing.T) {
	vf := setupVectorFeaturesFixture(t, vector.APIFormatVoyageContextual, true)
	assert.IsType(t, &embed.ContextWorker{}, vf.Runner)
}

// openTestBackend opens a fresh in-memory-ish sqlitevec backend with a
// single pre-seeded message so the scan-and-fill worker has a message to
// discover and embed.
func openTestBackend(t *testing.T) *sqlitevec.Backend {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, sqlitevec.RegisterExtension(), "RegisterExtension")

	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	main, err := sql.Open("sqlite3", mainPath)
	require.NoError(t, err, "open main")
	t.Cleanup(func() { _ = main.Close() })
	schema := `
CREATE TABLE messages (
    id INTEGER PRIMARY KEY,
    deleted_at DATETIME,
    deleted_from_source_at DATETIME
);`
	_, err = main.Exec(schema)
	require.NoError(t, err, "schema")
	_, err = main.Exec(`INSERT INTO messages (id) VALUES (1)`)
	require.NoError(t, err, "seed")
	b, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path:      filepath.Join(dir, "vectors.db"),
		MainPath:  mainPath,
		Dimension: 4,
		MainDB:    main,
	})
	require.NoError(t, err, "Open")
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// openStderrSink returns a *os.File pointing at /dev/null so
// pickEmbedGeneration's status prints do not clutter test output.
func openStderrSink(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err, "open /dev/null")
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
	require := require.New(t)
	assert := assert.New(t)
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
	require.Error(t, err, "expected error when no generations exist")
	// Intentional: we wrap the underlying error with a hint, but the
	// underlying sentinel should still be errors.Is-reachable so
	// upstream callers can branch on it.
	require.ErrorIs(t, err, vector.ErrNotEnabled, "err should wrap ErrNotEnabled")
}

// TestPickEmbedGeneration_ResumeFingerprintMismatch rejects a resume
// when the in-progress rebuild was started with a different model or
// dimension than the current config — continuing would silently
// embed against the wrong model.
func TestPickEmbedGeneration_ResumeFingerprintMismatch(t *testing.T) {
	ctx := context.Background()
	b := openTestBackend(t)
	_, err := b.CreateGeneration(ctx, "old-model", 4, "")
	require.NoError(t, err, "CreateGeneration")

	_, _, err = pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "new-model",
		Dimension:   4,
		Fingerprint: "new-model:4",
		Stderr:      openStderrSink(t),
	})
	require.Error(t, err, "expected fingerprint mismatch error")
	require.ErrorContains(t, err, "fingerprint", "error should mention fingerprint")
}

// TestPickEmbedGeneration_PrefersBuildingOverActive_MatchingFingerprint
// regression-guards the precedence bug where pickEmbedGeneration
// targeted an existing active generation even when a building
// generation for the configured model was in flight. The user
// expectation is that `msgvault embeddings build` drains the in-progress build
// (so it can be activated) rather than continuing to top up the old
// active generation.
func TestPickEmbedGeneration_PrefersBuildingOverActive_MatchingFingerprint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	b := openTestBackend(t)

	// Build state: an active generation exists, and a second building
	// generation has been created for the SAME model+dim (the typical
	// "I want to refresh my index" pattern).
	activeGen, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(err, "CreateGeneration (active)")
	require.NoError(b.ActivateGeneration(ctx, activeGen, true), "ActivateGeneration")
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
	require.NoError(t, err, "CreateGeneration (building)")

	_, _, err = pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "new-model",
		Dimension:   4,
		Fingerprint: "new-model:4",
		Stderr:      openStderrSink(t),
	})
	require.Error(t, err, "expected error for mismatched-fingerprint building generation")
	require.ErrorContains(t, err, "fingerprint", "error should mention fingerprint")
}

func TestPickEmbedGeneration_ContextualRejectsWrongGenerationFingerprint(t *testing.T) {
	ctx := context.Background()
	backend := openTestBackend(t)
	_, err := backend.CreateGeneration(ctx, "voyage-context-4", 4,
		"voyage-context-4:4:p1-111111:c32768:e1:avoyage-contextual:v0")
	require.NoError(t, err)

	_, rebuild, err := pickEmbedGeneration(ctx, backend, embedGenerationOpts{
		Model: "voyage-context-4", Dimension: 4,
		Fingerprint: "voyage-context-4:4:p1-111111:c32768:e1:avoyage-contextual:v1",
		Stderr:      openStderrSink(t),
	})
	require.Error(t, err)
	assert.False(t, rebuild)
	assert.Contains(t, err.Error(), "in-progress rebuild has fingerprint")
	assert.Contains(t, err.Error(), "avoyage-contextual:v0")
	assert.Contains(t, err.Error(), "avoyage-contextual:v1")

	building, lookupErr := backend.BuildingGeneration(ctx)
	require.NoError(t, lookupErr)
	require.NotNil(t, building)
	assert.Equal(t, vector.GenerationBuilding, building.State)
}

// TestPickEmbedGeneration_StaleActivePlusMatchingBuilding covers the
// "stale active + matching building" combination R51a calls out: an
// older active generation exists with a fingerprint that no longer
// matches the configured model, and a newer building generation
// matches. The configured-model build must be drained instead of the
// stale active one being topped up — otherwise the new build stays
// stuck in `building` indefinitely.
func TestPickEmbedGeneration_StaleActivePlusMatchingBuilding(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	b := openTestBackend(t)

	staleActive, err := b.CreateGeneration(ctx, "old-model", 4, "")
	require.NoError(err, "CreateGeneration (stale active)")
	require.NoError(b.ActivateGeneration(ctx, staleActive, true), "ActivateGeneration")
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
	require := require.New(t)
	ctx := context.Background()
	b := openTestBackend(t)

	matchingActive, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(err, "CreateGeneration (active)")
	require.NoError(b.ActivateGeneration(ctx, matchingActive, true), "ActivateGeneration")
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
	require.ErrorContains(err, "fingerprint", "error should mention fingerprint")
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
	require.Error(t, err, "expected abort error")
}

func TestRemainingCoverageHintMentionsBackstop(t *testing.T) {
	got := remainingCoverageHint(7, 3)

	assert.Contains(t, got, "Generation 7 still has 3 message(s) needing embedding")
	assert.Contains(t, got, "msgvault embeddings resume --backstop")
	assert.NotContains(t, got, "resume` again")
}

func TestNewProgressPrinter_UsesWindowedRate(t *testing.T) {
	assert := assert.New(t)
	var buf bytes.Buffer
	// window=2, total=210 so the percent path runs. The zero
	// interval keeps the test deterministic without sleeping.
	printer := newProgressPrinterWithMinInterval(&buf, 210, 2, 0)

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
	printer(embed.ProgressReport{
		Done: 100, TotalPending: 210,
		BatchMsgs: 100, BatchChars: 1000,
		BatchElapsed: 1 * time.Second,
		RunElapsed:   1 * time.Second,
	})
	printer(embed.ProgressReport{
		Done: 200, TotalPending: 210,
		BatchMsgs: 100, BatchChars: 1000,
		BatchElapsed: 1 * time.Second,
		RunElapsed:   2 * time.Second,
	})
	printer(embed.ProgressReport{
		Done: 210, TotalPending: 210,
		BatchMsgs: 10, BatchChars: 100,
		BatchElapsed: 5 * time.Second,
		RunElapsed:   7 * time.Second,
	})

	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.GreaterOrEqual(t, len(lines), 2, "expected at least 2 emitted lines, got:\n%s", out)
	finalLine := lines[len(lines)-1]

	assert.Contains(finalLine, "(last 2)", "expected `(last 2)` annotation on final line")
	assert.Contains(finalLine, "18 msg/s", "expected windowed `18 msg/s` on final line")
	assert.NotContains(finalLine, "30 msg/s", "final line shows cumulative rate `30 msg/s`; windowed implementation should not produce this")
}

func TestNewProgressPrinter_DoesNotBypassThrottleAfterInitialTotal(t *testing.T) {
	var buf bytes.Buffer
	printer := newProgressPrinter(&buf, 2, 2)

	printer(embed.ProgressReport{
		Done: 2, TotalPending: 2,
		BatchMsgs: 2, BatchChars: 20,
		BatchElapsed: 1 * time.Second,
		RunElapsed:   1 * time.Second,
	})
	printer(embed.ProgressReport{
		Done: 3, TotalPending: 2,
		BatchMsgs: 1, BatchChars: 10,
		BatchElapsed: 1 * time.Second,
		RunElapsed:   2 * time.Second,
	})

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 1, "progress emitted %d lines, want 1 throttled line after initial total:\n%s", len(lines), buf.String())
}
