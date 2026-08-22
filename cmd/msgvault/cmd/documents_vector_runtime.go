//go:build sqlite_vec || pgvector

package cmd

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	vectordocument "go.kenn.io/msgvault/internal/vector/document"
)

func nextDocumentVectorWorkerOwner() string {
	return "document-vector-" + uuid.NewString()
}

type checkpointingDocumentVectorWorker struct {
	worker       vectordocument.WorkerRunner
	checkpointer documentVectorBuildCheckpointer
	fingerprint  string
	now          func() time.Time
}

type documentVectorBuildCheckpointer interface {
	CheckpointDocumentVectorBuildForFingerprint(ctx context.Context, generationID int64, fingerprint string, afterChunkID int64, exhausted bool, delta store.DocumentVectorUsageDelta, now time.Time) error
}

func (w checkpointingDocumentVectorWorker) Run(ctx context.Context, generationID vectordocument.GenerationID, limit int) (vectordocument.RunResult, error) {
	result, runErr := w.worker.Run(ctx, generationID, limit)
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

func runConfiguredDocumentVectorGeneration(ctx context.Context, st *store.Store, generationID int64, limit int) (vectordocument.ReconcileResult, error) {
	if limit < 1 || limit > 1000 {
		return vectordocument.ReconcileResult{}, errors.New("document vector operation limit must be between 1 and 1000")
	}
	vf, err := setupVectorFeatures(ctx, st, cfg.DatabaseDSN(), false)
	if err != nil {
		return vectordocument.ReconcileResult{}, err
	}
	if vf == nil || vf.DocumentBackend == nil || vf.SemanticClient == nil {
		return vectordocument.ReconcileResult{}, errors.New("document vector runtime is unavailable")
	}
	defer func() { _ = vf.Close() }()
	return runDocumentVectorWithFeatures(ctx, st, vf, generationID, limit)
}

func runDocumentVectorWithFeatures(ctx context.Context, st *store.Store, vf *vectorFeatures, generationID int64, limit int) (vectordocument.ReconcileResult, error) {
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
		worker: worker, checkpointer: st, fingerprint: generation.Fingerprint, now: now,
	}
	reconciler := vectordocument.NewReconciler(vectordocument.ReconcilerDeps{
		Ledger: st, Worker: checkpointed, Backend: vf.DocumentBackend, Now: now,
	})
	return reconciler.Run(ctx, vectordocument.GenerationID(generationID), limit)
}

func runScheduledDocumentVectorGeneration(ctx context.Context, st *store.Store, vf *vectorFeatures, limit int) error {
	spec, err := desiredDocumentVectorSpec(ctx, st)
	if err != nil {
		return err
	}
	if err := requireDocumentVectorConsent(ctx, st, spec); err != nil {
		return err
	}
	retired, err := st.GetOldestRetiredDocumentVectorGeneration(ctx)
	if err != nil {
		return err
	}
	if retired != nil {
		_, err := runDocumentVectorWithFeatures(ctx, st, vf, retired.ID, limit)
		return err
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
		_, reconcileErr := runDocumentVectorWithFeatures(ctx, st, vf, building.ID, limit)
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
				_, reconcileErr := runDocumentVectorWithFeatures(ctx, st, vf, active.ID, limit)
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
	_, err = runDocumentVectorWithFeatures(ctx, st, vf, building.ID, limit)
	return err
}
