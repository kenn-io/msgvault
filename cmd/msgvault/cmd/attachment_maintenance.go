package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

const (
	automaticAttachmentBytes  = int64(256 << 20)
	attachmentMaintenanceJob  = "attachment-maintenance"
	attachmentMaintenanceCron = "17 3 * * *"
	// attachmentPackJob packs blobs that scheduled syncs left loose. A pass
	// re-reads the whole pack catalog, so it runs a few times a day rather
	// than after every sync. Bounded follow-ups drain any remaining backlog.
	attachmentPackJob  = "attachment-pack"
	attachmentPackCron = "41 */6 * * *"
	importMboxCommand  = "import-mbox"
)

// attachmentMaintenance coordinates daemon-owned attachment maintenance. Its
// callers already hold the daemon operation gate; these methods must not try
// to acquire it again.
type attachmentMaintenance struct {
	store               *store.Store
	maintainer          *packstore.Maintainer
	coordinator         *packstore.Coordinator
	blob                *attachmentstore.Store
	logger              *slog.Logger
	packCreationEnabled bool
	attachmentsDir      string
	// packPending records that a scheduled sync wrote loose blobs that no
	// pack pass has drained yet. Startup requests one scan of existing blobs.
	packPending atomic.Bool
}

func newAttachmentMaintenance(
	s *store.Store,
	attachmentsDir string,
	logger *slog.Logger,
	packCreationEnabled bool,
) (*attachmentMaintenance, error) {
	layout, err := packstore.NewLayout(attachmentsDir, packstore.LayoutOptions{
		Staging: packstore.StagingSameDirectory,
	})
	if err != nil {
		return nil, fmt.Errorf("create attachment maintenance layout: %w", err)
	}
	coordinator := packstore.NewCoordinator()
	maintainer, err := packstore.NewMaintainer(store.NewMaintenancePackCatalog(s, layout.Root()), layout, packstore.MaintainerOptions{
		Coordinator: coordinator,
	})
	if err != nil {
		return nil, fmt.Errorf("create attachment maintainer: %w", err)
	}
	return &attachmentMaintenance{
		store:               s,
		maintainer:          maintainer,
		coordinator:         coordinator,
		blob:                attachmentstore.Wrap(maintainer.Store()),
		logger:              logger,
		packCreationEnabled: packCreationEnabled,
		attachmentsDir:      attachmentsDir,
	}, nil
}

func (m *attachmentMaintenance) close() error {
	if m == nil || m.maintainer == nil {
		return nil
	}
	if err := m.maintainer.Close(); err != nil {
		return fmt.Errorf("close attachment maintainer: %w", err)
	}
	return nil
}

// pack performs one packer pass with the requested soft raw-byte budget.
func (m *attachmentMaintenance) pack(ctx context.Context, maxBytes int64) (packstore.PackStats, error) {
	if !m.packCreationEnabled {
		return packstore.PackStats{}, errors.New(
			"attachment pack creation is disabled by [data].loose_attachments",
		)
	}
	stats, err := m.maintainer.Pack(ctx, packstore.PackOptions{MaxBytes: maxBytes})
	if err != nil {
		return stats, fmt.Errorf("pack attachments: %w", err)
	}
	return stats, nil
}

// repack performs one physical-GC pass through the daemon's shared blob-store
// cache with the requested soft live-raw-byte budget.
func (m *attachmentMaintenance) repack(ctx context.Context, maxBytes int64) (packstore.RepackStats, error) {
	if !m.packCreationEnabled {
		return packstore.RepackStats{}, errors.New(
			"attachment pack creation is disabled by [data].loose_attachments",
		)
	}
	stats, err := m.maintainer.Repack(ctx, packstore.RepackOptions{MaxBytes: maxBytes})
	if err != nil {
		return stats, fmt.Errorf("repack attachments: %w", err)
	}
	return stats, nil
}

func (m *attachmentMaintenance) unpack(ctx context.Context) (packstore.UnpackStats, error) {
	stats, err := m.maintainer.Unpack(ctx)
	if err != nil {
		return stats, fmt.Errorf("unpack attachments: %w", err)
	}
	return stats, nil
}

// runAutomaticPack performs one bounded maintenance pass. Errors remain
// visible to schedulers, while callers following a successful ingest can log
// or stream the warning and deliberately preserve the ingest result.
func (m *attachmentMaintenance) runAutomaticPack(ctx context.Context, emitWarning func(string) error) error {
	if !m.packCreationEnabled {
		m.log().Debug("automatic attachment packing disabled")
		return nil
	}
	start := time.Now()
	stats, err := m.pack(ctx, automaticAttachmentBytes)
	duration := time.Since(start)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			m.log().Info("automatic attachment maintenance canceled")
			return err
		}

		m.logAutomaticPackSummary("automatic attachment maintenance progress", stats, duration)
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

	m.logAutomaticPackSummary("automatic attachment maintenance complete", stats, duration)
	if stats.BudgetExhausted {
		m.markPackPending()
	}
	return nil
}

func (m *attachmentMaintenance) logAutomaticPackSummary(message string, stats packstore.PackStats, duration time.Duration) {
	m.log().Info(message,
		"duration", duration.Round(time.Millisecond),
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
	if !m.packCreationEnabled {
		m.log().Debug("automatic attachment repacking disabled")
		return nil
	}
	start := time.Now()
	stats, err := m.repack(ctx, automaticAttachmentBytes)
	duration := time.Since(start)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			m.log().Info("automatic attachment repack canceled")
			return err
		}
		m.logAutomaticRepackSummary("automatic attachment repack progress", stats, duration)
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
	m.logAutomaticRepackSummary("automatic attachment repack complete", stats, duration)
	return nil
}

func (m *attachmentMaintenance) logAutomaticRepackSummary(message string, stats packstore.RepackStats, duration time.Duration) {
	m.log().Info(message,
		"duration", duration.Round(time.Millisecond),
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
	m.packPending.Store(false)
	if err := m.runAutomaticPack(ctx, nil); err != nil {
		m.markPackPending()
		return err
	}
	if m.packPending.Load() {
		return scheduler.ErrReschedule
	}
	return m.runAutomaticRepack(ctx, nil)
}

// markPackPending records that loose blobs await the next pack pass.
func (m *attachmentMaintenance) markPackPending() {
	if m != nil {
		m.packPending.Store(true)
	}
}

// runPendingPack runs one automatic pack pass when a scheduled sync left new
// loose blobs since the last pass. A failed pass leaves the request pending;
// an exhausted byte budget queues another pass behind waiting work.
func (m *attachmentMaintenance) runPendingPack(ctx context.Context) error {
	if m == nil || !m.packPending.Swap(false) {
		return nil
	}
	if err := m.runAutomaticPack(ctx, nil); err != nil {
		m.packPending.Store(true)
		return err
	}
	if m.packPending.Load() {
		return scheduler.ErrReschedule
	}
	return nil
}

// looseBlobWrites reports the in-process count of loose blobs created under
// this maintenance's attachments directory.
func (m *attachmentMaintenance) looseBlobWrites() int64 {
	if m == nil || m.attachmentsDir == "" {
		return 0
	}
	return export.LooseBlobWrites(m.attachmentsDir)
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
	if err := runWithAttachmentMutation(ctx, maintenance, ingest); err != nil {
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
	if err := runWithAttachmentMutation(ctx, maintenance, remove); err != nil {
		return err
	}
	if maintenance != nil {
		_ = maintenance.runAutomaticRepack(ctx, emitWarning)
	}
	return nil
}

func runWithAttachmentMutation(
	ctx context.Context,
	maintenance *attachmentMaintenance,
	run func(context.Context) error,
) error {
	if maintenance == nil || maintenance.coordinator == nil {
		return run(ctx)
	}
	lease, err := maintenance.coordinator.AcquireMutation(ctx)
	if err != nil {
		return fmt.Errorf("acquire attachment mutation lease: %w", err)
	}
	return errors.Join(run(ctx), lease.Release())
}

// runScheduledSource distinguishes attachment-producing provider/SyncTech
// sources from calendar-only sources while preserving one shared wrapper.
// Packing never runs inline: a pack pass re-reads the whole pack catalog and
// excludes every ingest while it runs, so a scheduled sync only records that
// it wrote new loose blobs and the attachment-pack job packs them later.
func runScheduledSource(
	ctx context.Context,
	maintenance *attachmentMaintenance,
	attachmentProducing bool,
	run func(context.Context) error,
) error {
	if !attachmentProducing {
		return run(ctx)
	}
	before := maintenance.looseBlobWrites()
	err := runWithAttachmentMutation(ctx, maintenance, run)
	if maintenance != nil && maintenance.looseBlobWrites() != before {
		maintenance.markPackPending()
	}
	return err
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

func registerAttachmentPackJob(sched *scheduler.Scheduler, maintenance *attachmentMaintenance) error {
	// Recheck the catalog on the first tick after startup. In-memory write
	// counts cannot tell us what a previous daemon left loose.
	maintenance.markPackPending()
	return sched.AddJob(scheduler.Job{
		Name:     attachmentPackJob,
		Schedule: attachmentPackCron,
		Run:      maintenance.runPendingPack,
	})
}

func registerScheduledBeeperJob(
	sched *scheduler.Scheduler,
	schedule string,
	maintenance *attachmentMaintenance,
	run func(context.Context) error,
) error {
	// Every beeper store source (one per beeper AccountID) maps to this
	// singleton job name via api.SchedulerJobNameForSource.
	return sched.AddJob(scheduler.Job{
		Name:        api.BeeperJobName,
		Schedule:    schedule,
		Preemptible: true,
		Run: func(ctx context.Context) error {
			return runScheduledSource(ctx, maintenance, true, run)
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
	case "archive-remote-images",
		"backfill-beeper-media",
		"backfill-discord-media",
		"backfill-slack-media",
		"backfill-teams-media",
		"import",
		"import-maildir",
		"import-eml",
		"import-emlx",
		"import-gvoice",
		"import-imazing-csv",
		"import-imessage",
		importMboxCommand,
		"import-messenger",
		"import-pst",
		"import-slackdump",
		"import-synctech-sms",
		"import-whatsapp",
		"sync-beeper",
		"sync-discord",
		"sync-slack",
		"sync-synctech-sms",
		"sync-teams":
		return true
	default:
		return false
	}
}
