package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.kenn.io/msgvault/internal/blobstore"
	"go.kenn.io/msgvault/internal/packer"
	"go.kenn.io/msgvault/internal/repacker"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

const (
	automaticAttachmentBytes  = int64(256 << 20)
	attachmentMaintenanceJob  = "attachment-maintenance"
	attachmentMaintenanceCron = "17 3 * * *"
	importMboxCommand         = "import-mbox"
)

// attachmentMaintenance coordinates daemon-owned attachment maintenance. Its
// callers already hold the daemon operation gate; these methods must not try
// to acquire it again.
type attachmentMaintenance struct {
	store          *store.Store
	blob           *blobstore.Store
	attachmentsDir string
	logger         *slog.Logger
}

// pack performs one packer pass with the requested soft raw-byte budget.
func (m *attachmentMaintenance) pack(ctx context.Context, maxBytes int64) (packer.Stats, error) {
	return packer.Run(ctx, m.store, m.attachmentsDir, packer.Options{MaxBytes: maxBytes})
}

// repack performs one physical-GC pass through the daemon's shared blob-store
// cache with the requested soft live-raw-byte budget.
func (m *attachmentMaintenance) repack(ctx context.Context, maxBytes int64) (repacker.Stats, error) {
	return repacker.Run(ctx, m.store, m.blob, m.attachmentsDir, repacker.Options{MaxBytes: maxBytes})
}

// runAutomaticPack performs one bounded maintenance pass. Errors remain
// visible to schedulers, while callers following a successful ingest can log
// or stream the warning and deliberately preserve the ingest result.
func (m *attachmentMaintenance) runAutomaticPack(ctx context.Context, emitWarning func(string) error) error {
	stats, err := m.pack(ctx, automaticAttachmentBytes)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			m.log().Info("automatic attachment maintenance canceled")
			return err
		}

		m.logAutomaticPackSummary("automatic attachment maintenance progress", stats)
		const retry = "run `msgvault pack-attachments` to retry"
		m.log().Warn("automatic attachment maintenance failed",
			"error", err,
			"retry", retry)
		if emitWarning != nil {
			warning := fmt.Sprintf("Automatic attachment maintenance failed: %v; %s.\n", err, retry)
			if emitErr := emitWarning(warning); emitErr != nil {
				m.log().Warn("failed to emit automatic attachment maintenance warning",
					"error", emitErr)
			}
		}
		return err
	}

	m.logAutomaticPackSummary("automatic attachment maintenance complete", stats)
	return nil
}

func (m *attachmentMaintenance) logAutomaticPackSummary(message string, stats packer.Stats) {
	m.log().Info(message,
		"max_bytes", automaticAttachmentBytes,
		"packs_sealed", stats.PacksSealed,
		"blobs_packed", stats.BlobsPacked,
		"bytes_packed", stats.BytesPacked,
		"packs_adopted", stats.PacksAdopted,
		"packs_removed", stats.PacksRemoved,
		"packs_quarantined", stats.PacksQuarantined,
		"packs_unreadable", stats.PacksUnreadable,
		"blobs_deferred_oversized", stats.BlobsDeferredOversized,
		"packs_deferred_oversized", stats.PacksDeferredOversized,
		"records_dropped", stats.RecordsDropped,
		"mappings_pruned", stats.MappingsPruned,
		"blobs_missing", stats.BlobsMissing,
		"blobs_corrupt", stats.BlobsCorrupt,
		"loose_swept", stats.LooseSwept,
		"loose_orphans_removed", stats.LooseOrphansRemoved,
		"budget_exhausted", stats.BudgetExhausted)
}

func (m *attachmentMaintenance) runAutomaticRepack(ctx context.Context, emitWarning func(string) error) error {
	stats, err := m.repack(ctx, automaticAttachmentBytes)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			m.log().Info("automatic attachment repack canceled")
			return err
		}
		m.logAutomaticRepackSummary("automatic attachment repack progress", stats)
		const retry = "run `msgvault repack-attachments` to retry"
		m.log().Warn("automatic attachment repack failed", "error", err, "retry", retry)
		if emitWarning != nil {
			warning := fmt.Sprintf("Automatic attachment repack failed: %v; %s.\n", err, retry)
			if emitErr := emitWarning(warning); emitErr != nil {
				m.log().Warn("failed to emit automatic attachment repack warning", "error", emitErr)
			}
		}
		return err
	}
	m.logAutomaticRepackSummary("automatic attachment repack complete", stats)
	return nil
}

func (m *attachmentMaintenance) logAutomaticRepackSummary(message string, stats repacker.Stats) {
	m.log().Info(message,
		"max_bytes", automaticAttachmentBytes,
		"mappings_pruned", stats.MappingsPruned,
		"packs_selected", stats.PacksSelected,
		"packs_rewritten", stats.PacksRewritten,
		"packs_sealed", stats.PacksSealed,
		"packs_removed", stats.PacksRemoved,
		"packs_deferred_oversized", stats.PacksDeferredOversized,
		"blobs_repacked", stats.BlobsRepacked,
		"bytes_repacked", stats.BytesRepacked,
		"budget_exhausted", stats.BudgetExhausted)
}

// daily runs the two bounded phases in order. A failed pack phase stops the
// job so the scheduler records the failure instead of obscuring it with a
// second maintenance result.
func (m *attachmentMaintenance) daily(ctx context.Context) error {
	if err := m.runAutomaticPack(ctx, nil); err != nil {
		return err
	}
	return m.runAutomaticRepack(ctx, nil)
}

func (m *attachmentMaintenance) log() *slog.Logger {
	if m != nil && m.logger != nil {
		return m.logger
	}
	return slog.Default()
}

// runAfterSuccessfulAttachmentIngest runs bounded maintenance only after a
// successful ingest. Maintenance and warning-stream failures are best-effort:
// neither may replace the successful ingest result.
func runAfterSuccessfulAttachmentIngest(
	ctx context.Context,
	maintenance *attachmentMaintenance,
	ingest func(context.Context) error,
	emitWarning func(string) error,
) error {
	if err := ingest(ctx); err != nil {
		return err
	}
	if maintenance != nil {
		_ = maintenance.runAutomaticPack(ctx, emitWarning)
	}
	return nil
}

// runAfterSuccessfulAttachmentRemoval runs bounded physical GC only after a
// successful removal. Repack and warning-stream failures never replace the
// already committed removal result.
func runAfterSuccessfulAttachmentRemoval(
	ctx context.Context,
	maintenance *attachmentMaintenance,
	remove func(context.Context) error,
	emitWarning func(string) error,
) error {
	if err := remove(ctx); err != nil {
		return err
	}
	if maintenance != nil {
		_ = maintenance.runAutomaticRepack(ctx, emitWarning)
	}
	return nil
}

// runScheduledSource distinguishes attachment-producing provider/SyncTech
// sources from calendar-only sources while preserving one shared wrapper.
func runScheduledSource(
	ctx context.Context,
	maintenance *attachmentMaintenance,
	attachmentProducing bool,
	run func(context.Context) error,
) error {
	if !attachmentProducing {
		return run(ctx)
	}
	return runAfterSuccessfulAttachmentIngest(ctx, maintenance, run, nil)
}

func registerAttachmentMaintenanceJob(sched *scheduler.Scheduler, maintenance *attachmentMaintenance) error {
	return sched.AddJob(scheduler.Job{
		Name:     attachmentMaintenanceJob,
		Schedule: attachmentMaintenanceCron,
		Run: func(ctx context.Context) error {
			return maintenance.daily(ctx)
		},
	})
}

// attachmentProducingCommand reports whether the first command word names a
// generic daemon CLI operation that can create loose attachments.
func attachmentProducingCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "backfill-teams-media",
		"import",
		"import-emlx",
		"import-gvoice",
		"import-imessage",
		importMboxCommand,
		"import-messenger",
		"import-pst",
		"import-synctech-sms",
		"import-whatsapp",
		"sync-synctech-sms",
		"sync-teams":
		return true
	default:
		return false
	}
}
