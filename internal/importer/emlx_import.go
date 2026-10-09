package importer

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"go.kenn.io/msgvault/internal/emlx"
	"go.kenn.io/msgvault/internal/rederive"
	"go.kenn.io/msgvault/internal/remoteimage"
	"go.kenn.io/msgvault/internal/store"
)

// EmlxImportOptions configures an Apple Mail .emlx directory import.
type EmlxImportOptions struct {
	// SourceType is the sources.source_type value.
	// Defaults to "apple-mail".
	SourceType string

	// Identifier is the sources.identifier (e.g. "you@gmail.com").
	Identifier string

	// NoResume ignores run progress; completed occurrence receipts remain usable.
	NoResume bool

	// FullReconcile invalidates this root before discovery and forces completion.
	FullReconcile bool

	// CheckpointInterval controls how often (in messages) to persist
	// progress. Defaults to 200.
	CheckpointInterval int

	// AttachmentsDir controls where attachments are written.
	// Empty means no disk storage.
	AttachmentsDir string
	// RemoteImages is nil unless remote image archiving was explicitly enabled.
	RemoteImages *remoteimage.Fetcher

	// MaxMessageBytes limits the maximum .emlx file size to read.
	// Defaults to 128 MiB.
	MaxMessageBytes int64

	// IngestFunc overrides message ingestion (for tests). If nil,
	// the default IngestRawMessage is used.
	IngestFunc func(
		ctx context.Context, st *store.Store,
		sourceID int64, identifier, attachmentsDir string,
		labelIDs []int64, sourceMsgID, rawHash string,
		raw []byte, fallbackDate time.Time,
		log *slog.Logger,
	) error

	// Logger is optional; defaults to slog.Default().
	Logger *slog.Logger
}

// EmlxImportSummary reports the results of an emlx import.
type EmlxImportSummary struct {
	SourceID          int64
	WasResumed        bool
	Duration          time.Duration
	MailboxesTotal    int
	MailboxesImported int
	MessagesProcessed int64
	MessagesAdded     int64
	MessagesUpdated   int64
	MessagesSkipped   int64
	FilesUnchanged    int64

	// PartialFiles counts *.partial.emlx files parsed. Their bodies are
	// complete; attachment parts are either restored from Apple Mail's
	// sibling Attachments/ directory or left uncached.
	PartialFiles int64

	// AttachmentsRestored counts attachment parts this run added to the
	// archive from the Attachments/ directory. Parts already archived by an
	// earlier run are not counted again.
	AttachmentsRestored int64

	Errors     int64
	HardErrors bool
}

type emlxCheckpoint struct {
	Phase        string `json:"phase,omitempty"`
	ReplyAfterID int64  `json:"reply_after_id,omitzero"`
	RootDir      string `json:"root_dir"`
	MailboxIndex int    `json:"mailbox_index"`
	MailboxPath  string `json:"mailbox_path,omitempty"`
	LastFile     string `json:"last_file"`
}

const defaultMaxEmlxBytes int64 = 128 << 20 // 128 MiB

// ImportEmlxDir archives new/changed filesystem occurrences and cheaply revisits
// completed ones. Missing local cache entries never delete archived messages.
func ImportEmlxDir(ctx context.Context, st *store.Store, rootDir string, opts EmlxImportOptions) (*EmlxImportSummary, error) {
	return importEmlxDir(ctx, st, rootDir, opts, defaultEmlxImportIO(opts))
}

func importEmlxDir(ctx context.Context, st *store.Store, rootDir string, opts EmlxImportOptions, io emlxImportIO) (retSummary *EmlxImportSummary, retErr error) {
	if opts.SourceType == "" {
		opts.SourceType = "apple-mail"
	}
	if opts.Identifier == "" {
		return nil, errors.New("identifier is required")
	}
	if opts.CheckpointInterval <= 0 {
		opts.CheckpointInterval = 200
	}
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = defaultMaxEmlxBytes
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	start := time.Now()
	summary := &EmlxImportSummary{}
	defer func() { summary.Duration = time.Since(start) }()
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("abs path: %w", err)
	}
	rootPrefix := emlxDigest(absRoot) + "/"
	src, err := st.GetOrCreateSource(opts.SourceType, opts.Identifier)
	if err != nil {
		return nil, fmt.Errorf("get/create source: %w", err)
	}
	summary.SourceID = src.ID
	ownershipCtx := context.WithoutCancel(ctx)
	execution, err := st.AcquireSyncExecutionContext(ownershipCtx, src.ID)
	if err != nil {
		return nil, fmt.Errorf("acquire sync execution: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	rederive.Heal(ctx, slog.Default(), st, src)
	var cp store.Checkpoint
	if !opts.NoResume && !opts.FullReconcile {
		active, err := st.GetLatestCheckpointedSyncByType(src.ID, "import-emlx")
		if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
			return nil, fmt.Errorf("check resumable sync: %w", err)
		}
		if active != nil && active.CursorBefore.Valid && active.CursorBefore.String != "" {
			var ecp emlxCheckpoint
			if err := json.Unmarshal([]byte(active.CursorBefore.String), &ecp); err == nil {
				if ecp.RootDir != absRoot {
					return nil, fmt.Errorf("active emlx import is for a different directory (%q), not %q; rerun with --no-resume or --full-reconcile", ecp.RootDir, absRoot)
				}
				if ecp.Phase != "" && ecp.Phase != "email-replies" {
					return nil, fmt.Errorf("unknown emlx checkpoint phase %q", ecp.Phase)
				}
				if ecp.ReplyAfterID < 0 {
					return nil, errors.New("invalid emlx reply checkpoint")
				}
				// Position is only progress reporting. Files can arrive before it, move,
				// or disappear. Receipts decide completion after every fresh discovery.
				cp.MessagesProcessed = active.MessagesProcessed
				cp.MessagesAdded = active.MessagesAdded
				cp.MessagesUpdated = active.MessagesUpdated
				cp.ErrorsCount = active.ErrorsCount
				summary.WasResumed = true
			}
		}
	}
	syncID, err := execution.StartSyncContext(ownershipCtx, "import-emlx", "")
	if err != nil {
		return nil, fmt.Errorf("start sync: %w", err)
	}
	st = st.ScopedToSync(src.ID, syncID)
	if opts.FullReconcile {
		if err := st.InvalidateEmlxRootContext(ctx, src.ID, rootPrefix); err != nil {
			return summary, fmt.Errorf("invalidate EMLX root: %w", err)
		}
	}
	report := func(err error) {
		summary.Errors++
		cp.ErrorsCount++
		summary.HardErrors = true
		log.Warn("incomplete emlx import", "error", err)
	}
	// Invalidation precedes even empty/failed discovery; no unvisited old receipt
	// can claim this reconciliation completed.
	mailboxes, err := emlx.DiscoverMailboxes(absRoot)
	if err != nil {
		discoveryErr, ok := errors.AsType[*emlx.DiscoveryError](err)
		if !ok {
			report(err)
		} else {
			for _, e := range discoveryErr.Errors {
				report(e)
			}
		}
	}
	if err != nil && len(mailboxes) == 0 {
		return nil, fmt.Errorf("discover mailboxes: %w", err)
	}
	summary.MailboxesTotal = len(mailboxes)
	policy := emlxPolicy(st, opts)
	checkpointBlocked := false
	lastMbox := 0
	lastPath, lastFile := "", ""
	// Work is deliberately sequential: one physical occurrence and fresh target
	// lookup at a time. No batch/hash map can outlive another target mutation.
	for mboxIdx, mb := range mailboxes {
		if err := ctx.Err(); err != nil {
			break
		}
		labelID, err := st.EnsureLabel(src.ID, mb.Label, mb.Label, "user")
		if err != nil {
			report(err)
			continue
		}
		log.Info("importing mailbox", "label", mb.Label, "files", len(mb.Files), "index", mboxIdx)
		for _, file := range mb.Files {
			if ctx.Err() != nil {
				break
			}
			cp.MessagesProcessed++
			summary.MessagesProcessed++
			outcome, err := processEmlxOccurrence(ctx, st, src.ID, absRoot, rootPrefix, file, mb.Label, []int64{labelID}, policy, opts, io, log)
			if err != nil {
				report(fmt.Errorf("occurrence %q: %w", file, err))
				checkpointBlocked = true
			}
			summary.PartialFiles += outcome.partial
			summary.AttachmentsRestored += outcome.restored
			switch outcome.kind {
			case "unchanged":
				summary.FilesUnchanged++
				summary.MessagesSkipped++
			case "skipped":
				summary.MessagesSkipped++
			case "added":
				summary.MessagesAdded++
				cp.MessagesAdded++
			case "updated":
				summary.MessagesUpdated++
				cp.MessagesUpdated++
			}
			if !checkpointBlocked {
				lastMbox, lastPath, lastFile = mboxIdx, mb.Path, file
			}
			if cp.MessagesProcessed%int64(opts.CheckpointInterval) == 0 {
				if err := saveEmlxCheckpoint(st, syncID, absRoot, lastMbox, lastPath, lastFile, &cp); err != nil {
					report(err)
				}
			}
		}
		summary.MailboxesImported++
	}
	if ctx.Err() != nil {
		if err := saveEmlxCheckpoint(st, syncID, absRoot, lastMbox, lastPath, lastFile, &cp); err != nil {
			log.Warn("checkpoint save failed", "error", err)
		}
		return summary, ctx.Err()
	}
	if err := saveEmlxCheckpoint(st, syncID, absRoot, lastMbox, lastPath, lastFile, &cp); err != nil {
		report(err)
	}
	if summary.HardErrors {
		if err := st.FailSync(syncID, fmt.Sprintf("completed with %d errors", cp.ErrorsCount)); err != nil {
			return summary, fmt.Errorf("fail sync: %w", err)
		}
		return summary, nil
	}
	// Reply resolution restarts because new old-date parents can precede an old
	// reply cursor even when all historical occurrence content was skipped.
	saveReplies := func(after int64) error {
		return saveEmlxCheckpointPhase(st, syncID, absRoot, lastMbox, lastPath, lastFile, &cp, "email-replies", after)
	}
	if err := saveReplies(0); err != nil {
		return summary, err
	}
	if err := st.ResolveEmailReplyParentsContext(ctx, src.ID, 0, saveReplies); err != nil {
		return summary, fmt.Errorf("resolve email replies: %w", err)
	}
	if err := st.CompleteSyncContext(ctx, syncID, fmt.Sprintf("mailboxes:%d messages:%d", summary.MailboxesImported, summary.MessagesAdded)); err != nil {
		return summary, fmt.Errorf("complete sync: %w", err)
	}
	return summary, nil
}

func saveEmlxCheckpoint(
	st *store.Store, syncID int64,
	rootDir string, mboxIdx int, mboxPath string,
	lastFile string, cp *store.Checkpoint,
) error {
	return saveEmlxCheckpointPhase(st, syncID, rootDir, mboxIdx, mboxPath, lastFile, cp, "", 0)
}

func saveEmlxCheckpointPhase(st *store.Store, syncID int64,
	rootDir string, mboxIdx int, mboxPath, lastFile string, cp *store.Checkpoint,
	phase string, replyAfterID int64,
) error {
	b, err := json.Marshal(emlxCheckpoint{
		Phase:        phase,
		ReplyAfterID: replyAfterID,
		RootDir:      rootDir,
		MailboxIndex: mboxIdx,
		MailboxPath:  mboxPath,
		LastFile:     lastFile,
	}, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	cp.PageToken = string(b)
	return st.UpdateSyncCheckpoint(syncID, cp)
}
