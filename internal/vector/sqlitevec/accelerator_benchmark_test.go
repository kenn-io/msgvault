//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// This file is a deliberately gated qualification harness, not a routine
// microbenchmark. It writes the real archive and vector schemas in bounded
// batches and measures complete backend calls. Synthetic vectors are useful
// for scale and latency stress, but are not evidence of recall on an affected
// archive's embedding distribution.
//
// Generate a fixture:
//
//   MSGVAULT_ANN_GENERATE_DIR=/tmp/msgvault-ann \
//   MSGVAULT_ANN_CHUNKS=2700000 MSGVAULT_ANN_DIMENSION=1024 \
//   go test -tags 'fts5 sqlite_vec' ./internal/vector/sqlitevec \
//     -run TestGenerateSQLiteAcceleratorQualificationFixture -count=1 -v
//
// Resume an interrupted, manifest-less fixture after inspecting it:
//
//   MSGVAULT_ANN_GENERATE_DIR=/tmp/msgvault-ann \
//   MSGVAULT_ANN_CHUNKS=2700000 MSGVAULT_ANN_DIMENSION=1024 \
//   MSGVAULT_ANN_RESUME=true \
//   go test -tags 'fts5 sqlite_vec' ./internal/vector/sqlitevec \
//     -run TestGenerateSQLiteAcceleratorQualificationFixture -count=1 -v
//
// Build the accelerator and run the qualification cases:
//
//   MSGVAULT_ANN_QUALIFY_DIR=/tmp/msgvault-ann \
//   go test -tags 'fts5 sqlite_vec' ./internal/vector/sqlitevec \
//     -run TestSQLiteAcceleratorQualification -count=1 -v -timeout 0

const (
	qualificationManifestName = "qualification.json"
	qualificationModel        = "synthetic-qualification"
	qualificationBatchSize    = 2048
)

type qualificationManifest struct {
	Version           int   `json:"version"`
	Chunks            int64 `json:"chunks"`
	Messages          int64 `json:"messages"`
	Dimension         int   `json:"dimension"`
	GenerationID      int64 `json:"generation_id"`
	MaxBatchBytes     int64 `json:"max_batch_bytes"`
	GeneratedMS       int64 `json:"generated_ms"`
	ResumedFromChunks int64 `json:"resumed_from_chunks,omitempty"`
	Synthetic         bool  `json:"synthetic"`
}

type qualificationCase struct {
	Name    string
	Ordinal int64
	Filter  vector.Filter
}

type qualificationCaseResult struct {
	Name             string  `json:"name"`
	RecallAt10       float64 `json:"recall_at_10"`
	Accelerator      string  `json:"accelerator"`
	CandidateCount   int     `json:"candidate_count"`
	PoolSaturated    bool    `json:"pool_saturated"`
	WarmP50MS        float64 `json:"warm_p50_ms"`
	WarmP95MS        float64 `json:"warm_p95_ms"`
	ProcessColdP50MS float64 `json:"process_cold_p50_ms"`
	ProcessColdP95MS float64 `json:"process_cold_p95_ms"`
	FusedWarmP50MS   float64 `json:"fused_warm_p50_ms"`
	FusedWarmP95MS   float64 `json:"fused_warm_p95_ms"`
}

type qualificationReport struct {
	Fixture           qualificationManifest     `json:"fixture"`
	BuildMS           int64                     `json:"build_ms"`
	DiskBytes         int64                     `json:"disk_bytes"`
	ProcessPeakRSSKB  int64                     `json:"process_peak_rss_kb"`
	WarmIterations    int                       `json:"warm_iterations"`
	ColdIterations    int                       `json:"process_cold_iterations"`
	ColdDefinition    string                    `json:"cold_definition"`
	CancellationOK    bool                      `json:"cancellation_ok"`
	Cancellation      qualificationCancellation `json:"cancellation"`
	PrepareRecoveryOK bool                      `json:"prepare_cancellation_recovery_ok"`
	Cases             []qualificationCaseResult `json:"cases"`
	SyntheticRecall   bool                      `json:"synthetic_recall_only"`
	Thresholds        qualificationThresholds   `json:"thresholds"`
	ThresholdsPassed  bool                      `json:"thresholds_passed"`
}

type qualificationThresholds struct {
	RecallAt10       float64 `json:"recall_at_10"`
	WarmP95MS        float64 `json:"warm_p95_ms"`
	ProcessColdP95MS float64 `json:"process_cold_p95_ms"`
	CancellationMS   float64 `json:"cancellation_ms"`
}

type qualificationCancellation struct {
	Candidates    int     `json:"candidates"`
	NaturalMS     float64 `json:"natural_ms"`
	CancelDelayMS float64 `json:"cancel_delay_ms"`
	ReturnMS      float64 `json:"return_after_cancel_ms"`
}

func TestQualificationFixtureGeneratorUsesBoundedBatches(t *testing.T) {
	dir := t.TempDir()
	manifest, err := generateQualificationFixture(t.Context(), dir, 513, 8, 37, false)
	require.NoError(t, err)
	assert.Equal(t, int64(513), manifest.Chunks)
	assert.Equal(t, int64(462), manifest.Messages)
	assert.LessOrEqual(t, manifest.MaxBatchBytes, int64((37+1)*8*4))

	mainStore, err := store.OpenForTest(filepath.Join(dir, "msgvault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mainStore.Close() })
	backend := openQualificationBackend(t, dir, mainStore, manifest, "auto")
	var messages, ftsRows, deleted, multiChunk int64
	require.NoError(t, mainStore.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	require.NoError(t, mainStore.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts`).Scan(&ftsRows))
	require.NoError(t, mainStore.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE deleted_at IS NOT NULL`).Scan(&deleted))
	require.NoError(t, backend.db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT message_id FROM embeddings GROUP BY message_id HAVING COUNT(*) > 1
		)`,
	).Scan(&multiChunk))
	assert.Equal(t, manifest.Messages, messages)
	assert.Equal(t, manifest.Messages, ftsRows)
	assert.Positive(t, deleted)
	assert.Equal(t, int64(51), multiChunk)
}

func TestQualificationVectorsPreserveDeterministicOrder(t *testing.T) {
	ordinals := []int64{17, 2, 99, 1, 64}
	vectors := qualificationVectors(ordinals, 8, 2)
	require.Len(t, vectors, len(ordinals))
	for i, ordinal := range ordinals {
		assert.Equal(t, qualificationVector(ordinal, 8), vectors[i])
	}
}

func TestQualificationFixtureGeneratorResumesCrossDatabaseInterruption(t *testing.T) {
	dir := t.TempDir()
	partial, err := generateQualificationFixture(t.Context(), dir, 200, 8, 37, false)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dir, qualificationManifestName)))

	mainStore, err := store.OpenForTest(filepath.Join(dir, "msgvault.db"))
	require.NoError(t, err)
	require.NoError(t, insertQualificationMessages(t.Context(), mainStore, []int64{201, 202, 203}, vector.GenerationID(partial.GenerationID), false))
	require.NoError(t, mainStore.Close())

	backend, err := Open(t.Context(), Options{
		Path: filepath.Join(dir, "vectors.db"), MainPath: filepath.Join(dir, "msgvault.db"), Dimension: partial.Dimension,
	})
	require.NoError(t, err)
	_, err = backend.db.Exec(`UPDATE index_generations
		SET state = 'building', completed_at = NULL, activated_at = NULL WHERE id = ?`, partial.GenerationID)
	require.NoError(t, err)
	require.NoError(t, backend.Close())

	_, err = generateQualificationFixture(t.Context(), dir, 513, 8, 37, false)
	require.ErrorContains(t, err, "refusing to overwrite")
	manifest, err := generateQualificationFixture(t.Context(), dir, 513, 8, 37, true)
	require.NoError(t, err)
	assert.Equal(t, int64(200), manifest.ResumedFromChunks)
	assert.Equal(t, int64(513), manifest.Chunks)
	assert.Equal(t, int64(462), manifest.Messages)

	mainStore, err = store.OpenForTest(filepath.Join(dir, "msgvault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mainStore.Close() })
	var messages, bodies, ftsRows int64
	require.NoError(t, mainStore.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	require.NoError(t, mainStore.DB().QueryRow(`SELECT COUNT(*) FROM message_bodies`).Scan(&bodies))
	require.NoError(t, mainStore.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts`).Scan(&ftsRows))
	assert.Equal(t, manifest.Messages, messages)
	assert.Equal(t, manifest.Messages, bodies)
	assert.Equal(t, manifest.Messages, ftsRows)
}

func TestGenerateSQLiteAcceleratorQualificationFixture(t *testing.T) {
	dir := strings.TrimSpace(os.Getenv("MSGVAULT_ANN_GENERATE_DIR"))
	if dir == "" {
		t.Skip("set MSGVAULT_ANN_GENERATE_DIR to generate a qualification fixture")
	}
	chunks := qualificationEnvInt64(t, "MSGVAULT_ANN_CHUNKS", 2_700_000)
	dimension := int(qualificationEnvInt64(t, "MSGVAULT_ANN_DIMENSION", 1024))
	batchSize := int(qualificationEnvInt64(t, "MSGVAULT_ANN_BATCH_SIZE", qualificationBatchSize))
	resume := qualificationEnvBool(t, "MSGVAULT_ANN_RESUME", false)
	stopHeartbeat := qualificationHeartbeat(t, "fixture generation")
	defer stopHeartbeat()
	manifest, err := generateQualificationFixture(t.Context(), dir, chunks, dimension, batchSize, resume)
	require.NoError(t, err)
	encoded, err := json.Marshal(manifest)
	require.NoError(t, err)
	t.Logf("QUALIFICATION_FIXTURE=%s", encoded)
}

func TestSQLiteAcceleratorQualification(t *testing.T) {
	dir := strings.TrimSpace(os.Getenv("MSGVAULT_ANN_QUALIFY_DIR"))
	if dir == "" {
		t.Skip("set MSGVAULT_ANN_QUALIFY_DIR to run the qualification harness")
	}
	stopHeartbeat := qualificationHeartbeat(t, "accelerator qualification")
	defer stopHeartbeat()
	manifest := readQualificationManifest(t, dir)
	mainStore, err := store.Open(filepath.Join(dir, "msgvault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mainStore.Close() })

	backend := openQualificationBackend(t, dir, mainStore, manifest, "auto")
	buildStarted := time.Now()
	status, err := backend.Accelerator(t.Context(), vector.GenerationID(manifest.GenerationID))
	require.NoError(t, err)
	prepareRecoveryOK := false
	if status == nil {
		interruptCtx, cancelPrepare := context.WithCancel(t.Context())
		_, interruptErr := backend.PrepareAccelerator(interruptCtx, vector.GenerationID(manifest.GenerationID), OptimizeOptions{
			Progress: func(progress OptimizeProgress) {
				if progress.IndexedCount > 0 {
					cancelPrepare()
				}
			},
		})
		cancelPrepare()
		require.ErrorIs(t, interruptErr, context.Canceled, "intentional prepare interruption")
		status, err = backend.Accelerator(t.Context(), vector.GenerationID(manifest.GenerationID))
		require.NoError(t, err)
		require.NotNil(t, status)
		require.Equal(t, AcceleratorBuilding, status.State)
		require.Positive(t, status.IndexedCount, "interrupted preparation must retain committed progress")
		prepareRecoveryOK = true
	}
	if status == nil || status.State != AcceleratorReady {
		lastReported := int64(-100_000)
		plan, prepareErr := backend.PrepareAccelerator(t.Context(), vector.GenerationID(manifest.GenerationID), OptimizeOptions{
			Progress: func(progress OptimizeProgress) {
				if progress.IndexedCount-lastReported >= 100_000 || progress.IndexedCount == progress.TotalCount {
					lastReported = progress.IndexedCount
					t.Logf("accelerator copy: %d/%d", progress.IndexedCount, progress.TotalCount)
				}
			},
		})
		require.NoError(t, prepareErr)
		require.True(t, plan.Applicable, "accelerator plan: %s", plan.Reason)
		require.NoError(t, RunAcceleratorWorker(t.Context(), backend.path, vector.GenerationID(manifest.GenerationID), plan.Threads))
		_, err = backend.PublishAccelerator(t.Context(), vector.GenerationID(manifest.GenerationID))
		require.NoError(t, err)
		prepareRecoveryOK = true
	}
	buildDuration := time.Since(buildStarted)

	exact := openQualificationBackend(t, dir, mainStore, manifest, "exact")
	warmIterations := int(qualificationEnvInt64(t, "MSGVAULT_ANN_WARM_ITERATIONS", 20))
	coldIterations := int(qualificationEnvInt64(t, "MSGVAULT_ANN_COLD_ITERATIONS", 5))
	cases := qualificationCases(manifest.Chunks)
	report := qualificationReport{
		Fixture: manifest, BuildMS: buildDuration.Milliseconds(),
		DiskBytes: qualificationDiskBytes(t, dir), ProcessPeakRSSKB: qualificationPeakRSSKB(t),
		WarmIterations: warmIterations, ColdIterations: coldIterations,
		ColdDefinition:  "first complete Backend.Search after reopening vectors.db; OS page cache retained",
		SyntheticRecall: true,
		Thresholds: qualificationThresholds{
			RecallAt10: 0.95, WarmP95MS: 2000, ProcessColdP95MS: 10000, CancellationMS: 2000,
		},
		ThresholdsPassed:  true,
		PrepareRecoveryOK: prepareRecoveryOK,
	}

	for _, testCase := range cases {
		queryVec := qualificationVector(testCase.Ordinal, manifest.Dimension)
		exactHits, exactErr := exact.Search(t.Context(), vector.GenerationID(manifest.GenerationID), queryVec, 10, testCase.Filter)
		require.NoErrorf(t, exactErr, "exact oracle %s", testCase.Name)
		annHits, metadata, annErr := backend.SearchWithMetadata(t.Context(), vector.GenerationID(manifest.GenerationID), queryVec, 10, testCase.Filter)
		require.NoErrorf(t, annErr, "accelerated search %s", testCase.Name)

		warm := measureQualification(t, warmIterations, func() error {
			_, _, searchErr := backend.SearchWithMetadata(t.Context(), vector.GenerationID(manifest.GenerationID), queryVec, 10, testCase.Filter)
			return searchErr
		})
		fused := measureQualification(t, warmIterations, func() error {
			_, _, searchErr := backend.FusedSearch(t.Context(), vector.FusedRequest{
				FTSTerms: []string{"benchmark"}, QueryVec: queryVec,
				Generation: vector.GenerationID(manifest.GenerationID), KPerSignal: 50,
				Limit: 10, RRFK: 60, Filter: testCase.Filter,
			})
			return searchErr
		})
		cold := measureQualification(t, coldIterations, func() error {
			coldBackend, openErr := Open(t.Context(), Options{
				Path: filepath.Join(dir, "vectors.db"), MainPath: filepath.Join(dir, "msgvault.db"),
				MainDB: mainStore.DB(), Dimension: manifest.Dimension, AcceleratorMode: "auto",
			})
			if openErr != nil {
				return openErr
			}
			_, _, searchErr := coldBackend.SearchWithMetadata(t.Context(), vector.GenerationID(manifest.GenerationID), queryVec, 10, testCase.Filter)
			closeErr := coldBackend.Close()
			if searchErr != nil {
				return searchErr
			}
			return closeErr
		})
		caseResult := qualificationCaseResult{
			Name: testCase.Name, RecallAt10: qualificationRecall(exactHits, annHits, 10),
			Accelerator: metadata.Accelerator, CandidateCount: metadata.CandidateCount,
			PoolSaturated: metadata.PoolSaturated,
			WarmP50MS:     percentileDurationMS(warm, 0.50), WarmP95MS: percentileDurationMS(warm, 0.95),
			ProcessColdP50MS: percentileDurationMS(cold, 0.50), ProcessColdP95MS: percentileDurationMS(cold, 0.95),
			FusedWarmP50MS: percentileDurationMS(fused, 0.50), FusedWarmP95MS: percentileDurationMS(fused, 0.95),
		}
		report.Cases = append(report.Cases, caseResult)
		if caseResult.RecallAt10 < report.Thresholds.RecallAt10 ||
			caseResult.WarmP95MS > report.Thresholds.WarmP95MS ||
			caseResult.FusedWarmP95MS > report.Thresholds.WarmP95MS ||
			caseResult.ProcessColdP95MS > report.Thresholds.ProcessColdP95MS {
			report.ThresholdsPassed = false
		}
	}

	cancellation, cancellationErr := qualificationActiveCancellation(
		t.Context(), backend, vector.GenerationID(manifest.GenerationID),
		qualificationVector(1, manifest.Dimension),
	)
	report.Cancellation = cancellation
	report.CancellationOK = errors.Is(cancellationErr, context.Canceled) &&
		cancellation.ReturnMS <= report.Thresholds.CancellationMS &&
		cancellation.CancelDelayMS+cancellation.ReturnMS < cancellation.NaturalMS
	if !report.CancellationOK {
		report.ThresholdsPassed = false
	}

	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	t.Logf("QUALIFICATION_REPORT=%s", encoded)
	require.True(t, report.ThresholdsPassed, "qualification thresholds failed; inspect QUALIFICATION_REPORT")
}

func qualificationActiveCancellation(
	parent context.Context,
	backend *Backend,
	generationID vector.GenerationID,
	queryVec []float32,
) (qualificationCancellation, error) {
	const (
		minimumNaturalDuration = 40 * time.Millisecond
		maximumCandidates      = 100_000
	)
	result := qualificationCancellation{Candidates: 2_000}
	var natural time.Duration
	for result.Candidates <= maximumCandidates {
		first, err := qualificationAcceleratedDuration(parent, backend, generationID, queryVec, result.Candidates)
		if err != nil {
			return result, err
		}
		second, err := qualificationAcceleratedDuration(parent, backend, generationID, queryVec, result.Candidates)
		if err != nil {
			return result, err
		}
		natural = min(first, second)
		if natural >= minimumNaturalDuration || result.Candidates == maximumCandidates {
			break
		}
		result.Candidates = min(result.Candidates*5, maximumCandidates)
	}
	if natural < minimumNaturalDuration {
		return result, fmt.Errorf("accelerated cancellation workload completed too quickly: %s", natural)
	}
	result.NaturalMS = float64(natural) / float64(time.Millisecond)
	delay := natural / 4
	result.CancelDelayMS = float64(delay) / float64(time.Millisecond)

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	cancelledAt := make(chan time.Time, 1)
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			at := time.Now()
			cancel()
			cancelledAt <- at
		case <-ctx.Done():
			cancelledAt <- time.Now()
		}
	}()
	previousCeiling := backend.annWorkCeiling
	backend.annWorkCeiling = result.Candidates
	_, _, err := backend.SearchWithMetadata(ctx, generationID, queryVec, result.Candidates, vector.Filter{})
	backend.annWorkCeiling = previousCeiling
	at := <-cancelledAt
	result.ReturnMS = float64(time.Since(at)) / float64(time.Millisecond)
	return result, err
}

func qualificationAcceleratedDuration(
	ctx context.Context,
	backend *Backend,
	generationID vector.GenerationID,
	queryVec []float32,
	candidates int,
) (time.Duration, error) {
	previousCeiling := backend.annWorkCeiling
	backend.annWorkCeiling = candidates
	started := time.Now()
	_, _, err := backend.SearchWithMetadata(ctx, generationID, queryVec, candidates, vector.Filter{})
	elapsed := time.Since(started)
	backend.annWorkCeiling = previousCeiling
	return elapsed, err
}

func generateQualificationFixture(ctx context.Context, dir string, chunks int64, dimension, batchSize int, resume bool) (qualificationManifest, error) {
	if chunks <= 0 || dimension <= 0 || batchSize <= 0 {
		return qualificationManifest{}, errors.New("chunks, dimension, and batch size must be positive")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return qualificationManifest{}, fmt.Errorf("create fixture directory: %w", err)
	}
	for _, name := range []string{"msgvault.db", "vectors.db", qualificationManifestName} {
		_, err := os.Stat(filepath.Join(dir, name))
		if resume {
			if name == qualificationManifestName && err == nil {
				return qualificationManifest{}, fmt.Errorf("refusing to resume completed fixture with %s", filepath.Join(dir, name))
			}
			if name != qualificationManifestName && os.IsNotExist(err) {
				return qualificationManifest{}, fmt.Errorf("cannot resume fixture without %s", filepath.Join(dir, name))
			}
		} else if err == nil {
			return qualificationManifest{}, fmt.Errorf("refusing to overwrite existing fixture file %s", filepath.Join(dir, name))
		}
		if err != nil && !os.IsNotExist(err) {
			return qualificationManifest{}, fmt.Errorf("inspect fixture file %s: %w", name, err)
		}
	}
	started := time.Now()
	mainStore, err := store.OpenForTest(filepath.Join(dir, "msgvault.db"))
	if err != nil {
		return qualificationManifest{}, err
	}
	defer func() { _ = mainStore.Close() }()
	if !resume {
		if err := mainStore.InitSchemaContext(ctx); err != nil {
			return qualificationManifest{}, fmt.Errorf("initialize archive schema: %w", err)
		}
		if err := seedQualificationParents(ctx, mainStore); err != nil {
			return qualificationManifest{}, err
		}
	}
	backend, err := Open(ctx, Options{
		Path: filepath.Join(dir, "vectors.db"), MainPath: filepath.Join(dir, "msgvault.db"),
		MainDB: mainStore.DB(), Dimension: dimension,
	})
	if err != nil {
		return qualificationManifest{}, err
	}
	defer func() { _ = backend.Close() }()
	qualificationConfig := vector.Config{Embeddings: vector.EmbeddingsConfig{Model: qualificationModel, Dimension: dimension}}
	fingerprint := qualificationConfig.GenerationFingerprint()
	var generationID vector.GenerationID
	var resumedFrom, messageCount int64
	if resume {
		generationID, resumedFrom, messageCount, err = inspectQualificationResume(
			ctx, mainStore, backend, chunks, dimension, batchSize, fingerprint,
		)
	} else {
		generationID, err = backend.CreateGeneration(ctx, qualificationModel, dimension, fingerprint)
	}
	if err != nil {
		return qualificationManifest{}, err
	}

	batch := make([]vector.Chunk, 0, batchSize)
	ordinals := make([]int64, 0, batchSize)
	messageIDs := make([]int64, 0, batchSize)
	var maxBatchBytes int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := insertQualificationMessages(ctx, mainStore, messageIDs, generationID, resume); err != nil {
			return err
		}
		vectors := qualificationVectors(ordinals, dimension, runtime.GOMAXPROCS(0))
		for i := range batch {
			batch[i].Vector = vectors[i]
		}
		if err := backend.Upsert(ctx, generationID, batch); err != nil {
			return fmt.Errorf("upsert qualification vector batch: %w", err)
		}
		maxBatchBytes = max(maxBatchBytes, int64(len(batch)*dimension*4))
		batch = batch[:0]
		ordinals = ordinals[:0]
		messageIDs = messageIDs[:0]
		return nil
	}
	for ordinal := resumedFrom + 1; ordinal <= chunks; ordinal++ {
		messageID, chunkIndex := qualificationChunkIdentity(ordinal)
		batch = append(batch, vector.Chunk{
			MessageID: messageID, ChunkIndex: chunkIndex,
			SourceCharLen: 128, ChunkCharStart: chunkIndex * 64, ChunkCharEnd: chunkIndex*64 + 64,
		})
		ordinals = append(ordinals, ordinal)
		if chunkIndex == 0 {
			messageIDs = append(messageIDs, messageID)
			messageCount++
		}
		// Upsert publishes the complete chunk set for each message. Keep a
		// two-chunk message in one batch even when the nominal boundary falls
		// between its chunks; this raises the bounded batch by at most one row.
		if len(batch) >= batchSize && ordinal%10 != 9 {
			if err := flush(); err != nil {
				return qualificationManifest{}, err
			}
		}
	}
	if err := flush(); err != nil {
		return qualificationManifest{}, err
	}
	if err := backend.ActivateGeneration(ctx, generationID, true); err != nil {
		return qualificationManifest{}, fmt.Errorf("activate qualification generation: %w", err)
	}
	manifest := qualificationManifest{
		Version: 1, Chunks: chunks, Messages: messageCount, Dimension: dimension,
		GenerationID: int64(generationID), MaxBatchBytes: maxBatchBytes,
		GeneratedMS: time.Since(started).Milliseconds(), ResumedFromChunks: resumedFrom, Synthetic: true,
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return qualificationManifest{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, qualificationManifestName), append(encoded, '\n'), 0o644); err != nil {
		return qualificationManifest{}, fmt.Errorf("write qualification manifest: %w", err)
	}
	return manifest, nil
}

func inspectQualificationResume(
	ctx context.Context,
	mainStore *store.Store,
	backend *Backend,
	targetChunks int64,
	dimension, batchSize int,
	fingerprint string,
) (vector.GenerationID, int64, int64, error) {
	var generations int64
	if err := backend.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM index_generations`).Scan(&generations); err != nil {
		return 0, 0, 0, fmt.Errorf("count qualification generations: %w", err)
	}
	if generations != 1 {
		return 0, 0, 0, fmt.Errorf("resume requires exactly one generation, found %d", generations)
	}
	var generationID vector.GenerationID
	var model, storedFingerprint, state string
	var storedDimension int
	var messageCount, embeddingCount int64
	if err := backend.db.QueryRowContext(ctx, `
		SELECT id, model, dimension, fingerprint, state, message_count, embedding_count
		  FROM index_generations`).Scan(
		&generationID, &model, &storedDimension, &storedFingerprint, &state, &messageCount, &embeddingCount,
	); err != nil {
		return 0, 0, 0, fmt.Errorf("inspect qualification generation: %w", err)
	}
	if model != qualificationModel || storedDimension != dimension || storedFingerprint != fingerprint || state != string(vector.GenerationBuilding) {
		return 0, 0, 0, fmt.Errorf(
			"resume fixture mismatch: model=%q dimension=%d fingerprint=%q state=%q",
			model, storedDimension, storedFingerprint, state,
		)
	}
	if embeddingCount > targetChunks {
		return 0, 0, 0, fmt.Errorf("resume fixture has %d chunks, above target %d", embeddingCount, targetChunks)
	}
	wantMessages := qualificationMessageCount(embeddingCount)
	if messageCount != wantMessages {
		return 0, 0, 0, fmt.Errorf("resume generation message count=%d, want %d for %d chunks", messageCount, wantMessages, embeddingCount)
	}
	if embeddingCount > 0 {
		var rows, minID, maxID int64
		if err := backend.db.QueryRowContext(ctx, `
			SELECT COUNT(*), MIN(embedding_id), MAX(embedding_id)
			  FROM embeddings WHERE generation_id = ?`, int64(generationID)).Scan(&rows, &minID, &maxID); err != nil {
			return 0, 0, 0, fmt.Errorf("inspect qualification vector prefix: %w", err)
		}
		if rows != embeddingCount || minID != 1 || maxID != embeddingCount {
			return 0, 0, 0, fmt.Errorf(
				"resume vectors are not a contiguous prefix: rows=%d min=%d max=%d metadata=%d",
				rows, minID, maxID, embeddingCount,
			)
		}
		wantMessageID, wantChunkIndex := qualificationChunkIdentity(embeddingCount)
		var messageID int64
		var chunkIndex int
		if err := backend.db.QueryRowContext(ctx, `
			SELECT message_id, chunk_index FROM embeddings
			 WHERE generation_id = ? AND embedding_id = ?`, int64(generationID), embeddingCount).Scan(&messageID, &chunkIndex); err != nil {
			return 0, 0, 0, fmt.Errorf("inspect qualification vector tail: %w", err)
		}
		if messageID != wantMessageID || chunkIndex != wantChunkIndex {
			return 0, 0, 0, fmt.Errorf(
				"resume vector tail mismatch: message=%d chunk=%d, want message=%d chunk=%d",
				messageID, chunkIndex, wantMessageID, wantChunkIndex,
			)
		}
	}

	var messages, bodies, ftsRows, invalidMessages int64
	if err := mainStore.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&messages); err != nil {
		return 0, 0, 0, fmt.Errorf("count qualification messages: %w", err)
	}
	if err := mainStore.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM message_bodies`).Scan(&bodies); err != nil {
		return 0, 0, 0, fmt.Errorf("count qualification bodies: %w", err)
	}
	if err := mainStore.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages_fts`).Scan(&ftsRows); err != nil {
		return 0, 0, 0, fmt.Errorf("count qualification FTS rows: %w", err)
	}
	if err := mainStore.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM messages
		 WHERE id % 10 = 0
		    OR source_message_id != printf('qualification-%d', id)
		    OR embed_gen != ?`, int64(generationID)).Scan(&invalidMessages); err != nil {
		return 0, 0, 0, fmt.Errorf("validate qualification messages: %w", err)
	}
	maxMessages := min(qualificationMessageCount(targetChunks), wantMessages+int64(batchSize))
	if messages < wantMessages || messages > maxMessages || bodies != messages || ftsRows != messages || invalidMessages != 0 {
		return 0, 0, 0, fmt.Errorf(
			"resume main archive mismatch: messages=%d bodies=%d fts=%d invalid=%d, expected %d..%d",
			messages, bodies, ftsRows, invalidMessages, wantMessages, maxMessages,
		)
	}
	return generationID, embeddingCount, messageCount, nil
}

func qualificationMessageCount(chunks int64) int64 {
	return chunks - chunks/10
}

func seedQualificationParents(ctx context.Context, mainStore *store.Store) error {
	tx, err := mainStore.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for id := int64(1); id <= 3; id++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sources (id, source_type, identifier, display_name) VALUES (?, 'gmail', ?, ?)`, id, fmt.Sprintf("qualification-%d@example.test", id), fmt.Sprintf("Qualification %d", id)); err != nil {
			return fmt.Errorf("seed source %d: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title) VALUES (?, ?, ?, 'email_thread', ?)`, id, id, fmt.Sprintf("qualification-%d", id), fmt.Sprintf("Qualification %d", id)); err != nil {
			return fmt.Errorf("seed conversation %d: %w", id, err)
		}
	}
	return tx.Commit()
}

func insertQualificationMessages(ctx context.Context, mainStore *store.Store, messageIDs []int64, generationID vector.GenerationID, ignoreExisting bool) error {
	tx, err := mainStore.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	insertPrefix := "INSERT INTO"
	if ignoreExisting {
		insertPrefix = "INSERT OR IGNORE INTO"
	}
	messageStmt, err := tx.PrepareContext(ctx, insertPrefix+` messages
		(id, conversation_id, source_id, source_message_id, message_type, sent_at, subject, snippet,
		 size_estimate, deleted_at, embed_gen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = messageStmt.Close() }()
	bodyStmt, err := tx.PrepareContext(ctx, insertPrefix+` message_bodies (message_id, body_text) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = bodyStmt.Close() }()
	ftsStmt, err := tx.PrepareContext(ctx, insertPrefix+` messages_fts
		(rowid, message_id, subject, body, from_addr, to_addr, cc_addr) VALUES (?, ?, ?, ?, '', '', '')`)
	if err != nil {
		return err
	}
	defer func() { _ = ftsStmt.Close() }()
	for _, messageID := range messageIDs {
		sourceID := qualificationSourceID(messageID)
		messageType := "email"
		if messageID%5 == 0 {
			messageType = "sms"
		}
		subject := "benchmark qualification"
		body := "benchmark synthetic vector search corpus"
		if messageID%1000 == 1 {
			subject += " needle"
			body += " needle"
		}
		var deletedAt any
		if messageID%97 == 0 {
			deletedAt = "2026-01-01 00:00:00"
		}
		if _, err := messageStmt.ExecContext(ctx, messageID, sourceID, sourceID,
			fmt.Sprintf("qualification-%d", messageID), messageType,
			"2026-01-01 00:00:00", subject, body, 128, deletedAt, int64(generationID)); err != nil {
			return fmt.Errorf("insert qualification message %d: %w", messageID, err)
		}
		if _, err := bodyStmt.ExecContext(ctx, messageID, body); err != nil {
			return fmt.Errorf("insert qualification body %d: %w", messageID, err)
		}
		if _, err := ftsStmt.ExecContext(ctx, messageID, messageID, subject, body); err != nil {
			return fmt.Errorf("insert qualification FTS row %d: %w", messageID, err)
		}
	}
	return tx.Commit()
}

func qualificationChunkIdentity(ordinal int64) (int64, int) {
	if ordinal%10 == 0 {
		return ordinal - 1, 1
	}
	return ordinal, 0
}

func qualificationSourceID(messageID int64) int64 {
	if messageID%101 == 0 {
		return 3
	}
	if messageID%2 == 0 {
		return 2
	}
	return 1
}

func qualificationVector(ordinal int64, dimension int) []float32 {
	clusterState := uint64(ordinal/64) + 0x9e3779b97f4a7c15
	pointState := uint64(ordinal) + 0xd1b54a32d192ed03
	result := make([]float32, dimension)
	var norm float64
	for i := range result {
		clusterState = qualificationXorshift(clusterState)
		pointState = qualificationXorshift(pointState)
		base := float64(int32(clusterState>>32)) / float64(math.MaxInt32)
		jitter := float64(int32(pointState>>32)) / float64(math.MaxInt32)
		value := base + 0.025*jitter
		result[i] = float32(value)
		norm += value * value
	}
	scale := float32(1 / math.Sqrt(norm))
	for i := range result {
		result[i] *= scale
	}
	return result
}

func qualificationVectors(ordinals []int64, dimension, workers int) [][]float32 {
	result := make([][]float32, len(ordinals))
	workers = min(max(workers, 1), len(ordinals))
	if workers == 0 {
		return result
	}

	jobs := make(chan int)
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			for index := range jobs {
				result[index] = qualificationVector(ordinals[index], dimension)
			}
		}()
	}
	for index := range ordinals {
		jobs <- index
	}
	close(jobs)
	group.Wait()
	return result
}

func qualificationXorshift(value uint64) uint64 {
	value ^= value << 13
	value ^= value >> 7
	value ^= value << 17
	return value
}

func qualificationCases(chunks int64) []qualificationCase {
	deletedOrdinal := int64(97)
	if chunks < deletedOrdinal {
		deletedOrdinal = 1
	}
	middle := max(int64(1), chunks/2)
	if middle%10 == 0 {
		middle--
	}
	multi := int64(10)
	if chunks < multi {
		multi = 1
	}
	selective := int64(101)
	for selective <= chunks && selective%10 == 0 {
		selective += 101
	}
	if selective > chunks {
		selective = 1
	}
	return []qualificationCase{
		{Name: "unfiltered", Ordinal: 1},
		{Name: "broad_message_type", Ordinal: middle, Filter: vector.Filter{MessageTypes: []string{"email"}}},
		{Name: "selective_source", Ordinal: selective, Filter: vector.Filter{SourceIDs: []int64{3}}},
		{Name: "deleted_tombstone", Ordinal: deletedOrdinal},
		{Name: "multi_chunk", Ordinal: multi},
	}
}

func openQualificationBackend(t *testing.T, dir string, mainStore *store.Store, manifest qualificationManifest, mode string) *Backend {
	t.Helper()
	backend, err := Open(t.Context(), Options{
		Path: filepath.Join(dir, "vectors.db"), MainPath: filepath.Join(dir, "msgvault.db"),
		MainDB: mainStore.DB(), Dimension: manifest.Dimension, AcceleratorMode: mode,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	return backend
}

func readQualificationManifest(t *testing.T, dir string) qualificationManifest {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(dir, qualificationManifestName))
	require.NoError(t, err)
	var manifest qualificationManifest
	require.NoError(t, json.Unmarshal(contents, &manifest))
	require.Equal(t, 1, manifest.Version)
	return manifest
}

func qualificationEnvInt64(t *testing.T, name string, defaultValue int64) int64 {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	require.NoErrorf(t, err, "parse %s", name)
	require.Positivef(t, parsed, "%s", name)
	return parsed
}

func qualificationEnvBool(t *testing.T, name string, defaultValue bool) bool {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseBool(value)
	require.NoErrorf(t, err, "parse %s", name)
	return parsed
}

func measureQualification(t *testing.T, iterations int, operation func() error) []time.Duration {
	t.Helper()
	durations := make([]time.Duration, 0, iterations)
	for range iterations {
		started := time.Now()
		err := operation()
		durations = append(durations, time.Since(started))
		require.NoError(t, err)
	}
	return durations
}

func percentileDurationMS(values []time.Duration, percentile float64) float64 {
	ordered := append([]time.Duration(nil), values...)
	slices.Sort(ordered)
	index := int(math.Ceil(percentile*float64(len(ordered)))) - 1
	index = min(max(index, 0), len(ordered)-1)
	return float64(ordered[index]) / float64(time.Millisecond)
}

func qualificationRecall(want, got []vector.Hit, limit int) float64 {
	wantIDs := make(map[int64]struct{}, min(limit, len(want)))
	for _, hit := range want[:min(limit, len(want))] {
		wantIDs[hit.MessageID] = struct{}{}
	}
	if len(wantIDs) == 0 {
		return 1
	}
	matched := 0
	for _, hit := range got[:min(limit, len(got))] {
		if _, ok := wantIDs[hit.MessageID]; ok {
			matched++
		}
	}
	return float64(matched) / float64(len(wantIDs))
}

func qualificationDiskBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk qualification fixture: %w", walkErr)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect qualification fixture entry: %w", err)
		}
		total += info.Size()
		return nil
	})
	require.NoError(t, err)
	return total
}

func qualificationPeakRSSKB(t *testing.T) int64 {
	t.Helper()
	contents, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Logf("peak RSS unavailable on %s: %v", runtime.GOOS, err)
		return 0
	}
	for line := range strings.SplitSeq(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "VmHWM:" {
			value, parseErr := strconv.ParseInt(fields[1], 10, 64)
			if parseErr == nil {
				return value
			}
		}
	}
	return 0
}

func qualificationHeartbeat(t *testing.T, operation string) func() {
	t.Helper()
	done := make(chan struct{})
	started := time.Now()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				t.Logf("%s still running (elapsed %s)", operation, time.Since(started).Round(time.Second))
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}
