package documentindex

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
)

const DocumentAttachmentConsumerKey = "document-index/v1"

type DocumentOccurrenceCatalog interface {
	RegisterAttachmentChangeConsumer(ctx context.Context, consumerKey string) (store.AttachmentChangeConsumer, bool, error)
	CompleteAttachmentChangeReconciliation(ctx context.Context, consumerKey string, baselineSequence int64) error
	ListDocumentAttachmentIDsAfter(ctx context.Context, afterID int64, limit int) ([]int64, error)
	ReconcileDocumentOccurrence(ctx context.Context, attachmentID int64, sourceSequence int64) (store.DocumentOccurrence, bool, error)
	ReconcileMissingDocumentOccurrences(ctx context.Context, limit int) (int, error)
	ListAttachmentChanges(ctx context.Context, consumerKey string, limit int) ([]store.AttachmentChange, error)
	AdvanceAttachmentChangeConsumer(ctx context.Context, consumerKey string, sequence int64) error
}

type ReconcilerConfig struct {
	AttachmentPageSize int
	ChangePageSize     int
}

type ReconcileResult struct {
	ConsumerCreated     bool
	FullScanCompleted   bool
	AttachmentsExamined int
	EligibleOccurrences int
	MissingRemoved      int
	ChangesConsumed     int
}

type Reconciler struct {
	catalog DocumentOccurrenceCatalog
	config  ReconcilerConfig
}

func NewReconciler(catalog DocumentOccurrenceCatalog, config ReconcilerConfig) (*Reconciler, error) {
	if catalog == nil || config.AttachmentPageSize <= 0 || config.AttachmentPageSize > 10_000 ||
		config.ChangePageSize <= 0 || config.ChangePageSize > 10_000 {
		return nil, errors.New("document reconciler configuration is invalid")
	}
	return &Reconciler{catalog: catalog, config: config}, nil
}

// Reconcile performs the full registration protocol on first enablement and
// then drains the durable journal. Full-scan source sequences use the captured
// baseline, so replayed later events always supersede bootstrap observations.
func (r *Reconciler) Reconcile(ctx context.Context) (ReconcileResult, error) {
	return r.reconcile(ctx, false)
}

// FullReconcile is the periodic correctness backstop. Normal cycles consume
// only the journal plus a bounded orphan check; callers schedule this explicit
// O(all attachments) scan at a lower cadence.
func (r *Reconciler) FullReconcile(ctx context.Context) (ReconcileResult, error) {
	return r.reconcile(ctx, true)
}

func (r *Reconciler) reconcile(ctx context.Context, forceFull bool) (ReconcileResult, error) {
	consumer, created, err := r.catalog.RegisterAttachmentChangeConsumer(ctx, DocumentAttachmentConsumerKey)
	if err != nil {
		return ReconcileResult{}, err
	}
	result := ReconcileResult{ConsumerCreated: created}
	scanSequence := consumer.LastSequence
	if !consumer.ReconciliationComplete {
		scanSequence = consumer.BaselineSequence
	}
	if !consumer.ReconciliationComplete || forceFull {
		if err := r.fullScan(ctx, scanSequence, &result); err != nil {
			return result, err
		}
		result.FullScanCompleted = true
	}
	if !consumer.ReconciliationComplete {
		if err := r.catalog.CompleteAttachmentChangeReconciliation(
			ctx, consumer.ConsumerKey, consumer.BaselineSequence,
		); err != nil {
			return result, err
		}
	}
	if err := r.removeMissing(ctx, &result); err != nil {
		return result, err
	}
	if err := r.replay(ctx, &result); err != nil {
		return result, err
	}
	return result, nil
}

func (r *Reconciler) fullScan(
	ctx context.Context,
	baseline int64,
	result *ReconcileResult,
) error {
	var afterID int64
	for {
		ids, err := r.catalog.ListDocumentAttachmentIDsAfter(ctx, afterID, r.config.AttachmentPageSize)
		if err != nil {
			return err
		}
		for _, attachmentID := range ids {
			_, eligible, err := r.catalog.ReconcileDocumentOccurrence(ctx, attachmentID, baseline)
			if err != nil {
				return fmt.Errorf("reconcile document attachment %d: %w", attachmentID, err)
			}
			result.AttachmentsExamined++
			if eligible {
				result.EligibleOccurrences++
			}
			afterID = attachmentID
		}
		if len(ids) < r.config.AttachmentPageSize {
			break
		}
	}
	return nil
}

func (r *Reconciler) removeMissing(ctx context.Context, result *ReconcileResult) error {
	for {
		removed, err := r.catalog.ReconcileMissingDocumentOccurrences(ctx, r.config.AttachmentPageSize)
		if err != nil {
			return err
		}
		result.MissingRemoved += removed
		if removed < r.config.AttachmentPageSize {
			break
		}
	}
	return nil
}

func (r *Reconciler) replay(ctx context.Context, result *ReconcileResult) error {
	for {
		changes, err := r.catalog.ListAttachmentChanges(
			ctx, DocumentAttachmentConsumerKey, r.config.ChangePageSize,
		)
		if err != nil {
			return err
		}
		if len(changes) == 0 {
			return nil
		}
		for _, change := range changes {
			if err := r.applyChange(ctx, change); err != nil {
				return err
			}
			if err := r.catalog.AdvanceAttachmentChangeConsumer(
				ctx, DocumentAttachmentConsumerKey, change.Sequence,
			); err != nil {
				return fmt.Errorf("advance document attachment change %d: %w", change.Sequence, err)
			}
			result.ChangesConsumed++
		}
	}
}

func (r *Reconciler) applyChange(ctx context.Context, change store.AttachmentChange) error {
	ids := make([]int64, 0, 2)
	if change.OldAttachmentID != nil {
		ids = append(ids, *change.OldAttachmentID)
	}
	if change.NewAttachmentID != nil &&
		(change.OldAttachmentID == nil || *change.NewAttachmentID != *change.OldAttachmentID) {
		ids = append(ids, *change.NewAttachmentID)
	}
	for _, attachmentID := range ids {
		if attachmentID <= 0 {
			return fmt.Errorf("attachment change %d has invalid attachment coordinates", change.Sequence)
		}
		if _, _, err := r.catalog.ReconcileDocumentOccurrence(ctx, attachmentID, change.Sequence); err != nil {
			return fmt.Errorf("replay attachment change %d for attachment %d: %w",
				change.Sequence, attachmentID, err)
		}
	}
	return nil
}
