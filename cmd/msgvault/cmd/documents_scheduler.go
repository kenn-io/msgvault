package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/documentindex"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

const (
	documentFullReconcileJob         = "document-index-full-reconcile"
	documentFullReconcileCron        = "43 3 * * 0"
	documentDerivativeRecoveryWindow = 7 * 24 * time.Hour
	documentDerivativeGCBatch        = 1000
)

func configureDocumentReconcileJob(
	ctx context.Context,
	sched *scheduler.Scheduler,
	st *store.Store,
	enabled bool,
) error {
	if !enabled {
		consented, err := st.HasActiveDocumentProviderConsent(ctx)
		if err != nil {
			return err
		}
		if !consented {
			return st.UnregisterAttachmentChangeConsumer(ctx, documentindex.DocumentAttachmentConsumerKey)
		}
	}
	reconciler, err := documentindex.NewReconciler(st, documentindex.ReconcilerConfig{
		AttachmentPageSize: 1000,
		ChangePageSize:     1000,
	})
	if err != nil {
		return err
	}
	if err := bootstrapDocumentOccurrencesIfConsented(ctx, st); err != nil {
		return fmt.Errorf("bootstrap document reconciliation: %w", err)
	}
	return sched.AddJob(scheduler.Job{
		Name:     documentFullReconcileJob,
		Schedule: documentFullReconcileCron,
		Run: func(ctx context.Context) error {
			_, err := st.GetAttachmentChangeConsumer(ctx, documentindex.DocumentAttachmentConsumerKey)
			if errors.Is(err, store.ErrAttachmentChangeConsumerMissing) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("read document reconciliation registration: %w", err)
			}
			if _, err = reconciler.FullReconcile(ctx); err != nil {
				return err
			}
			for {
				result, gcErr := st.GarbageCollectDocumentDerivatives(
					ctx, time.Now().UTC().Add(-documentDerivativeRecoveryWindow),
					documentDerivativeGCBatch,
				)
				if gcErr != nil {
					return fmt.Errorf("garbage collect document derivatives: %w", gcErr)
				}
				if result.ExtractionsRemoved < documentDerivativeGCBatch {
					return nil
				}
			}
		},
	})
}
