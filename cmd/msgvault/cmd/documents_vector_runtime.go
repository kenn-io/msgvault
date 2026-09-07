//go:build sqlite_vec || pgvector

package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	vectordocument "go.kenn.io/msgvault/internal/vector/document"
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

func nextDocumentVectorWorkerOwner() string {
	return "document-vector-" + uuid.NewString()
}

type checkpointingDocumentVectorWorker struct {
	worker       vectordocument.WorkerRunner
	checkpointer documentVectorBuildCheckpointer
	recorder     operations.Recorder
	scope        operations.PassScope
	fingerprint  string
	now          func() time.Time
}

type documentVectorBuildCheckpointer interface {
	CheckpointDocumentVectorBuildForFingerprint(ctx context.Context, generationID int64, fingerprint string, afterChunkID int64, exhausted bool, delta store.DocumentVectorUsageDelta, now time.Time) error
}

func (w checkpointingDocumentVectorWorker) Run(
	ctx context.Context, generationID vectordocument.GenerationID, limit int,
) (result vectordocument.RunResult, runErr error) {
	pass, terminal, err := beginCommandOperationPass(
		ctx, w.recorder, operations.KindDocumentEmbedding, w.scope,
	)
	if err != nil {
		return result, err
	}
	if terminal != nil {
		return documentVectorRunResultFromOperationRun(terminal)
	}
	defer func() {
		counters := documentEmbeddingCounters(result)
		pass.checkpoint(ctx, counters)
		pass.finish(ctx, counters, runErr)
	}()
	result, runErr = w.worker.Run(ctx, generationID, limit)
	if !result.Exhausted && result.AfterGenerationID == 0 {
		return result, runErr
	}
	delta := store.DocumentVectorUsageDelta{
		ProviderCalls: int64(result.ProviderCalls), ProviderDocuments: int64(result.ProviderDocuments),
		ProviderChunks: int64(result.ProviderChunks), ProviderInputChars: int64(result.ProviderInputChars),
	}
	checkpointErr := w.checkpointer.CheckpointDocumentVectorBuildForFingerprint(
		ctx, int64(generationID), w.fingerprint, result.AfterChunkID, result.Exhausted, delta, w.now(),
	)
	return result, errors.Join(runErr, checkpointErr)
}

func documentEmbeddingCounters(result vectordocument.RunResult) operations.InvocationCounters {
	return operations.InvocationCounters{
		Attempted: int64(result.Attempted), Succeeded: int64(result.Succeeded), Failed: int64(result.Failed),
	}
}

func documentVectorRunResultFromOperationRun(run *operations.Run) (vectordocument.RunResult, error) {
	if run == nil {
		return vectordocument.RunResult{}, errors.New("document embedding operation outcome is required")
	}
	counters, err := operations.InvocationCountersFromPublic(run.ID.Kind(), run.Counters)
	if err != nil {
		return vectordocument.RunResult{}, err
	}
	return vectordocument.RunResult{
		Attempted: int(counters.Attempted), Succeeded: int(counters.Succeeded), Failed: int(counters.Failed),
		Published: int(counters.Succeeded),
	}, operations.TerminalReplayOutcome(run)
}

func runConfiguredDocumentVectorGeneration(ctx context.Context, st *store.Store, generationID int64, limit int) (vectordocument.ReconcileResult, error) {
	if limit < 1 || limit > 1000 {
		return vectordocument.ReconcileResult{}, errors.New("document vector operation limit must be between 1 and 1000")
	}
	generation, err := st.GetDocumentVectorGeneration(ctx, generationID)
	if err != nil {
		return vectordocument.ReconcileResult{}, err
	}
	if generation.State == store.DocumentVectorGenerationRetired {
		backend, closeBackend, err := openDocumentVectorCleanupBackend(ctx, st, cfg.DatabaseDSN())
		if err != nil {
			return vectordocument.ReconcileResult{}, err
		}
		defer func() { _ = closeBackend() }()
		reconciler := vectordocument.NewReconciler(vectordocument.ReconcilerDeps{
			Ledger: st, Backend: backend, Now: func() time.Time { return time.Now().UTC() },
		})
		return reconciler.Run(ctx, vectordocument.GenerationID(generationID), limit)
	}
	vf, err := setupVectorFeatures(ctx, st, cfg.DatabaseDSN(), false)
	if err != nil {
		return vectordocument.ReconcileResult{}, err
	}
	if vf == nil || vf.DocumentBackend == nil || vf.SemanticClient == nil {
		return vectordocument.ReconcileResult{}, errors.New("document vector runtime is unavailable")
	}
	defer func() { _ = vf.Close() }()
	return runDocumentVectorWithFeatures(ctx, st, vf, generationID, limit,
		newOperationPassScope("cli:document-vector", operations.TriggerManual))
}

func openDocumentVectorCleanupBackend(ctx context.Context, st *store.Store, mainPath string) (vectordocument.Backend, func() error, error) {
	if store.IsPostgresURL(mainPath) {
		backend, err := pgvector.DocumentBackendForDB(st.DB())
		if err != nil {
			return nil, nil, fmt.Errorf("open pgvector document cleanup backend: %w", err)
		}
		return backend, func() error { return nil }, nil
	}
	vectorPath := cfg.Vector.DBPath
	if vectorPath == "" {
		vectorPath = filepath.Join(cfg.Data.DataDir, "vectors.db")
	}
	backend, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: vectorPath})
	if err != nil {
		return nil, nil, fmt.Errorf("open document cleanup backend: %w", err)
	}
	return backend.DocumentBackend(), backend.Close, nil
}

func runDocumentVectorWithFeatures(
	ctx context.Context, st *store.Store, vf *vectorFeatures, generationID int64, limit int,
	scope operations.PassScope,
) (vectordocument.ReconcileResult, error) {
	limit = min(limit, max(1, vf.Cfg.Embeddings.BatchSize))
	generation, err := st.GetDocumentVectorGeneration(ctx, generationID)
	if err != nil {
		return vectordocument.ReconcileResult{}, err
	}
	cursor, err := st.GetDocumentVectorBuildCursor(ctx, generationID)
	if err != nil {
		return vectordocument.ReconcileResult{}, err
	}
	now := func() time.Time { return time.Now().UTC() }
	var afterGenerationID vectordocument.GenerationID
	if cursor > 0 {
		afterGenerationID = vectordocument.GenerationID(generationID)
	}
	worker := vectordocument.NewWorker(vectordocument.WorkerDeps{
		Ledger: st, Provider: vf.SemanticClient, Backend: vf.DocumentBackend,
		Owner: nextDocumentVectorWorkerOwner(), Dimension: generation.Dimension,
		MaxInputChars:       cfg.Vector.Embeddings.MaxInputChars,
		ContextualDocuments: cfg.Vector.Embeddings.EffectiveAPIFormat() == vector.APIFormatVoyageContextual,
		LeaseDuration:       2 * time.Minute, HeartbeatInterval: 20 * time.Second,
		RetryDelay: time.Minute, MaxAttempts: 5,
		AfterGenerationID: afterGenerationID, AfterChunkID: cursor, Now: now,
	})
	checkpointed := checkpointingDocumentVectorWorker{
		worker: worker, checkpointer: st, recorder: st, scope: scope,
		fingerprint: generation.Fingerprint, now: now,
	}
	reconciler := vectordocument.NewReconciler(vectordocument.ReconcilerDeps{
		Ledger: st, Worker: checkpointed, Backend: vf.DocumentBackend, Now: now,
	})
	return reconciler.Run(ctx, vectordocument.GenerationID(generationID), limit)
}

func runScheduledDocumentVectorGeneration(ctx context.Context, st *store.Store, vf *vectorFeatures, limit int) error {
	return st.WithDocumentVectorOperationLock(ctx, func() error {
		return runScheduledDocumentVectorGenerationLocked(ctx, st, vf, limit)
	})
}

func runScheduledDocumentVectorGenerationLocked(ctx context.Context, st *store.Store, vf *vectorFeatures, limit int) error {
	scope := newOperationPassScope("scheduled:document-vector", operations.TriggerScheduled)
	retired, err := st.GetOldestRetiredDocumentVectorGeneration(ctx)
	if err != nil {
		return err
	}
	if retired != nil {
		reconciler := vectordocument.NewReconciler(vectordocument.ReconcilerDeps{
			Ledger: st, Backend: vf.DocumentBackend, Now: func() time.Time { return time.Now().UTC() },
		})
		_, err := reconciler.Run(ctx, vectordocument.GenerationID(retired.ID), limit)
		return err
	}
	if vf.SemanticClient == nil {
		return nil
	}
	spec, err := desiredDocumentVectorSpec(ctx, st)
	if errors.Is(err, store.ErrDocumentVectorInvalidGenerationState) {
		return nil
	}
	if err != nil {
		return err
	}
	consented, err := hasDocumentVectorConsent(ctx, st, spec)
	if err != nil {
		return err
	}
	if !consented {
		return nil
	}
	building, err := st.GetBuildingDocumentVectorGeneration(ctx)
	if err != nil {
		return err
	}
	if building != nil && building.DocumentVectorGenerationSpec != spec {
		retired, retireErr := st.RetireDocumentVectorGeneration(ctx, building.ID, time.Now())
		if retireErr != nil {
			return retireErr
		}
		if !retired {
			return store.ErrDocumentVectorInvalidGenerationState
		}
		// Reconcile only the obsolete generation this pass. The next bounded
		// run creates the desired generation without exceeding one cleanup page.
		_, reconcileErr := runDocumentVectorWithFeatures(ctx, st, vf, building.ID, limit, scope)
		return reconcileErr
	}
	if building == nil {
		active, err := st.GetActiveDocumentVectorGeneration(ctx)
		if err != nil {
			return err
		}
		switch {
		case active == nil || active.DocumentVectorGenerationSpec != spec:
			generation, _, ensureErr := st.EnsureDocumentVectorGeneration(ctx, spec)
			if ensureErr != nil {
				return ensureErr
			}
			building = &generation
		default:
			status, statusErr := st.GetDocumentVectorGenerationStatus(ctx, active.ID, "", limit)
			if statusErr != nil {
				return statusErr
			}
			if status.CleanupPending > 0 {
				_, reconcileErr := runDocumentVectorWithFeatures(ctx, st, vf, active.ID, limit, scope)
				return reconcileErr
			}
			coverage, coverageErr := st.GetDocumentVectorCoverage(ctx, active.ID)
			if coverageErr != nil {
				return coverageErr
			}
			if coverage.Complete() {
				return nil
			}
			generation, rebuildErr := st.StartDocumentVectorRebuild(ctx, active.ID, spec, time.Now())
			if rebuildErr != nil {
				return rebuildErr
			}
			building = &generation
		}
	}
	_, err = runDocumentVectorWithFeatures(ctx, st, vf, building.ID, limit, scope)
	return err
}
