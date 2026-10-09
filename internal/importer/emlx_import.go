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
func ImportEmlxDir(
	ctx context.Context, st *store.Store, rootDir string, opts EmlxImportOptions,
) (*EmlxImportSummary, error) {
	return importEmlxDir(ctx, st, rootDir, opts, defaultEmlxImportIO(opts))
}

// emlxRun carries one invocation's progress across mailboxes.
type emlxRun struct {
	st                *store.Store
	syncID            int64
	absRoot           string
	opts              EmlxImportOptions
	log               *slog.Logger
	summary           *EmlxImportSummary
	cp                store.Checkpoint
	occurrences       *emlxOccurrenceImporter
	checkpointBlocked bool
	lastMbox          int
	lastPath          string
	lastFile          string
}

// reportSoft counts an error that leaves the run able to complete. Retryable
// files stay pending and are revisited on the next import.
func (r *emlxRun) reportSoft(msg string, err error) {
	r.summary.Errors++
	r.cp.ErrorsCount++
	r.log.Warn(msg, "error", err)
}

// reportHard counts an archive failure that fails the run.
func (r *emlxRun) reportHard(msg string, err error) {
	r.reportSoft(msg, err)
	r.summary.HardErrors = true
}

func (r *emlxRun) saveCheckpoint() error {
	return saveEmlxCheckpoint(r.st, r.syncID, r.absRoot, r.lastMbox, r.lastPath, r.lastFile, &r.cp)
}

func importEmlxDir(
	ctx context.Context, st *store.Store, rootDir string, opts EmlxImportOptions, io emlxImportIO,
) (retSummary *EmlxImportSummary, retErr error) {
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
	run := &emlxRun{absRoot: absRoot, opts: opts, log: log, summary: summary}
	if !opts.NoResume && !opts.FullReconcile {
		if err := run.resumeCounters(st, src.ID); err != nil {
			return nil, err
		}
	}
	syncID, err := execution.StartSyncContext(ownershipCtx, "import-emlx", "")
	if err != nil {
		return nil, fmt.Errorf("start sync: %w", err)
	}
	st = st.ScopedToSync(src.ID, syncID)
	run.st, run.syncID = st, syncID
	// Fatal returns must finish the started run; cancellation keeps its checkpoint
	// available for the next invocation.
	defer func() {
		if retErr == nil || errors.Is(retErr, context.Canceled) || errors.Is(retErr, context.DeadlineExceeded) {
			return
		}
		if err := st.FailSyncContext(ownershipCtx, syncID, retErr.Error()); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("fail sync: %w", err))
		}
	}()
	rootPrefix := emlxDigest(absRoot) + "/"
	if opts.FullReconcile {
		// Invalidation precedes even empty/failed discovery; no unvisited old
		// receipt can claim this reconciliation completed.
		if err := st.InvalidateEmlxRootContext(ctx, src.ID, rootPrefix); err != nil {
			return summary, fmt.Errorf("invalidate EMLX root: %w", err)
		}
	}
	mailboxes, err := emlx.DiscoverMailboxes(absRoot)
	if err != nil {
		if len(mailboxes) == 0 {
			return nil, fmt.Errorf("discover mailboxes: %w", err)
		}
		reportDiscoveryErrors(run, err)
	}
	summary.MailboxesTotal = len(mailboxes)
	run.occurrences = &emlxOccurrenceImporter{
		st: st, sourceID: src.ID, syncID: syncID, root: absRoot, prefix: rootPrefix,
		policy: emlxPolicy(st, opts), opts: opts, io: io, log: log,
	}
	for mboxIdx, mb := range mailboxes {
		if ctx.Err() != nil {
			break
		}
		run.importMailbox(ctx, src.ID, mboxIdx, mb)
	}
	return run.finish(ctx, src.ID)
}

// resumeCounters carries the totals of an interrupted run on the same root.
// Every run revisits every file, so receipts, not a position, decide what is
// complete and the processed count restarts. Errors count failed attempts.
func (r *emlxRun) resumeCounters(st *store.Store, sourceID int64) error {
	active, err := st.GetLatestCheckpointedSyncByType(sourceID, "import-emlx")
	if errors.Is(err, store.ErrSyncRunNotFound) || active == nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check resumable sync: %w", err)
	}
	if !active.CursorBefore.Valid || active.CursorBefore.String == "" {
		return nil
	}
	var ecp emlxCheckpoint
	if err := json.Unmarshal([]byte(active.CursorBefore.String), &ecp); err != nil || ecp.RootDir != r.absRoot {
		return nil //nolint:nilerr // An unreadable or foreign checkpoint only means no counters to carry.
	}
	r.cp.MessagesAdded = active.MessagesAdded
	r.cp.MessagesUpdated = active.MessagesUpdated
	r.cp.ErrorsCount = active.ErrorsCount
	r.summary.WasResumed = true
	return nil
}

func reportDiscoveryErrors(run *emlxRun, err error) {
	discoveryErr, ok := errors.AsType[*emlx.DiscoveryError](err)
	if !ok {
		run.reportSoft("partial mailbox discovery", err)
		return
	}
	for _, e := range discoveryErr.Errors {
		run.reportSoft("partial mailbox discovery", e)
	}
}

func (r *emlxRun) importMailbox(ctx context.Context, sourceID int64, mboxIdx int, mb emlx.Mailbox) {
	labelID, err := r.st.EnsureLabel(sourceID, mb.Label, mb.Label, "user")
	if err != nil {
		r.reportSoft("failed to ensure label", fmt.Errorf("label %q: %w", mb.Label, err))
		return
	}
	r.log.Info("importing mailbox", "label", mb.Label, "files", len(mb.Files), "index", mboxIdx)
	for start := 0; start < len(mb.Files) && ctx.Err() == nil; start += emlxChunkSize {
		files := mb.Files[start:min(start+emlxChunkSize, len(mb.Files))]
		chunk, err := r.occurrences.prefetch(ctx, files, mb.Label, labelID)
		if err != nil {
			if ctx.Err() == nil {
				r.reportHard("failed to read EMLX receipts", err)
				r.checkpointBlocked = true
			}
			continue
		}
		for i, file := range files {
			if ctx.Err() != nil {
				break
			}
			r.importFile(ctx, chunk, i, mboxIdx, mb.Path, file)
		}
	}
	r.summary.MailboxesImported++
}

func (r *emlxRun) importFile(ctx context.Context, chunk *emlxChunk, i, mboxIdx int, mboxPath, file string) {
	outcome, err := r.occurrences.process(ctx, chunk, i)
	if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return
	}
	r.cp.MessagesProcessed++
	r.summary.MessagesProcessed++
	if err != nil {
		err = fmt.Errorf("occurrence %q: %w", file, err)
		if isEmlxRetryable(err) {
			r.reportSoft("EMLX file remains pending", err)
		} else {
			r.reportHard("failed to import EMLX file", err)
		}
		r.checkpointBlocked = true
	}
	r.summary.PartialFiles += outcome.partial
	r.summary.AttachmentsRestored += outcome.restored
	switch outcome.kind {
	case emlxOutcomeUnchanged:
		r.summary.FilesUnchanged++
		r.summary.MessagesSkipped++
	case emlxOutcomeSkipped:
		r.summary.MessagesSkipped++
	case emlxOutcomeAdded:
		r.summary.MessagesAdded++
		r.cp.MessagesAdded++
	case emlxOutcomeUpdated:
		r.summary.MessagesUpdated++
		r.cp.MessagesUpdated++
	case emlxOutcomeNone:
	}
	if !r.checkpointBlocked {
		r.lastMbox, r.lastPath, r.lastFile = mboxIdx, mboxPath, file
	}
	if r.cp.MessagesProcessed%int64(r.opts.CheckpointInterval) == 0 {
		if err := r.saveCheckpoint(); err != nil {
			r.reportSoft("failed to save checkpoint", err)
		}
	}
}

// finish records the run's outcome. Soft errors still resolve replies and
// complete the run; hard errors fail it.
func (r *emlxRun) finish(ctx context.Context, sourceID int64) (*EmlxImportSummary, error) {
	if ctx.Err() != nil {
		if err := r.saveCheckpoint(); err != nil {
			r.log.Warn("checkpoint save failed", "error", err)
		}
		return r.summary, ctx.Err()
	}
	if err := r.saveCheckpoint(); err != nil {
		r.reportSoft("failed to save final checkpoint", err)
	}
	if r.summary.HardErrors {
		if err := r.st.FailSync(r.syncID, fmt.Sprintf("completed with %d errors", r.cp.ErrorsCount)); err != nil {
			return r.summary, fmt.Errorf("fail sync: %w", err)
		}
		return r.summary, nil
	}
	// Reply resolution restarts because new old-date parents can precede an old
	// reply cursor even when all historical occurrence content was skipped.
	saveReplies := func(after int64) error {
		return saveEmlxCheckpointPhase(r.st, r.syncID, r.absRoot, r.lastMbox, r.lastPath, r.lastFile,
			&r.cp, "email-replies", after)
	}
	if err := saveReplies(0); err != nil {
		return r.summary, err
	}
	if err := r.st.ResolveEmailReplyParentsContext(ctx, sourceID, 0, saveReplies); err != nil {
		return r.summary, fmt.Errorf("resolve email replies: %w", err)
	}
	final := fmt.Sprintf("mailboxes:%d messages:%d", r.summary.MailboxesImported, r.summary.MessagesAdded)
	if r.cp.ErrorsCount > 0 {
		final += fmt.Sprintf(" errors:%d", r.cp.ErrorsCount)
	}
	if err := r.st.CompleteSyncContext(ctx, r.syncID, final); err != nil {
		return r.summary, fmt.Errorf("complete sync: %w", err)
	}
	return r.summary, nil
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
