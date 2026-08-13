package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/documentindex"
	"go.kenn.io/msgvault/internal/documentindex/mistral"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

const (
	documentFullReconcileJob         = "document-index-full-reconcile"
	documentExtractionJob            = "document-index-extract"
	documentFullReconcileCron        = "43 3 * * 0"
	documentDerivativeRecoveryWindow = 7 * 24 * time.Hour
	documentDerivativeGCBatch        = 1000
	documentRoleRepairBatch          = 1000
)

type scheduledDocumentDeps struct {
	newProcessor    func(*documentindex.DocumentsConfig, []string) (mistral.Processor, error)
	openAttachments func(*store.Store) (documentindex.DocumentAttachmentOpener, func() error, error)
	dataDirectory   string
}

func configureDocumentReconcileJob(
	ctx context.Context,
	sched *scheduler.Scheduler,
	st *store.Store,
	enabled bool,
) error {
	if !enabled {
		return st.UnregisterAttachmentChangeConsumer(ctx, documentindex.DocumentAttachmentConsumerKey)
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

func configureDocumentExtractionJob(
	sched *scheduler.Scheduler,
	st *store.Store,
	documentsConfig documentindex.DocumentsConfig,
	deps scheduledDocumentDeps,
) error {
	if !documentsConfig.Enabled || documentsConfig.Schedule == "" {
		return nil
	}
	if deps.newProcessor == nil || deps.openAttachments == nil || deps.dataDirectory == "" {
		return errors.New("scheduled document extraction dependencies are incomplete")
	}
	return sched.AddJob(scheduler.Job{
		Name: documentExtractionJob, Schedule: documentsConfig.Schedule,
		Run: func(ctx context.Context) (runErr error) {
			manifest, err := loadDocumentCapabilityManifest(documentsConfig.CapabilityManifest)
			if err != nil {
				return err
			}
			allowedMediaTypes, profile, err := documentProfileForConfig(&documentsConfig, manifest)
			if err != nil {
				return err
			}
			if _, err = st.RepairHistoricalAttachmentRolesBatch(ctx, documentRoleRepairBatch); err != nil {
				return fmt.Errorf("repair historical attachment roles before scheduled extraction: %w", err)
			}
			status, err := st.GetDocumentIndexStatus(ctx, profile.ID)
			if err != nil {
				return err
			}
			if !status.ProfileEnabled || !status.ExactConsent {
				return errors.New("scheduled document extraction requires exact provider consent")
			}
			limit, err := documentsConfig.MaxDocumentsWithinRunBudget(10_000)
			if err != nil {
				return err
			}
			processor, err := deps.newProcessor(&documentsConfig, allowedMediaTypes)
			if err != nil {
				return err
			}
			attachments, closeAttachments, err := deps.openAttachments(st)
			if err != nil {
				return err
			}
			defer func() { runErr = errors.Join(runErr, closeAttachments()) }()
			_, err = executeDocumentBuild(
				ctx, st, attachments, processor, &documentsConfig, manifest,
				allowedMediaTypes, profile, limit, "documents-daemon", deps.dataDirectory,
				documentBuildIncremental, nil,
			)
			return err
		},
	})
}
