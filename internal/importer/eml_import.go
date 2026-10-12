package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/msgvault/internal/eml"
	"go.kenn.io/msgvault/internal/rederive"
	"go.kenn.io/msgvault/internal/remoteimage"
	"go.kenn.io/msgvault/internal/store"
)

const defaultMaxEMLMessageBytes int64 = 128 << 20

// EMLImportOptions configures an import from a tree of .mailbox directories.
type EMLImportOptions struct {
	// NotifyImports requires the daemon's separate global import opt-in.
	NotifyImports      bool
	SourceType         string
	Identifier         string
	NoResume           bool
	CheckpointInterval int
	AttachmentsDir     string
	// RemoteImages is nil unless remote image archiving was explicitly enabled.
	RemoteImages    *remoteimage.Fetcher
	MaxMessageBytes int64
	// IngestFunc overrides message ingestion for focused importer tests.
	// Nil uses IngestRawMessage.
	IngestFunc func(
		ctx context.Context, st *store.Store,
		sourceID int64, identifier, attachmentsDir string,
		labelIDs []int64, sourceMsgID, rawHash string,
		raw []byte, fallbackDate time.Time,
		log *slog.Logger,
	) error
	Logger *slog.Logger
}

// EMLImportSummary reports the result of a directory tree import.
type EMLImportSummary struct {
	// ImportRunID identifies this logical native import, including recovery.
	ImportRunID       string `json:"import_run_id,omitempty"`
	SourceID          int64
	WasResumed        bool
	Duration          time.Duration
	MailboxesTotal    int
	MailboxesImported int
	MessagesProcessed int64
	MessagesAdded     int64
	MessagesUpdated   int64
	MessagesSkipped   int64
	Errors            int64
	HardErrors        bool
}

type emlCheckpoint struct {
	RootDir      string `json:"root_dir"`
	MailboxIndex int    `json:"mailbox_index"`
	MailboxPath  string `json:"mailbox_path,omitempty"`
	LastFile     string `json:"last_file,omitempty"`
}

// ImportEMLDir imports raw RFC 5322 messages from MailMate-style .mailbox
// directories. Exact raw bytes provide the stable, source-scoped dedup key.
func ImportEMLDir(
	ctx context.Context,
	st *store.Store,
	root string,
	opts EMLImportOptions,
) (*EMLImportSummary, error) {
	return importRawDirectory(ctx, st, root, opts, rawDirectoryLayout{
		sourceType: "eml",
		discover: func(root string) ([]directoryMailbox, error) {
			boxes, err := eml.DiscoverMailboxes(root)
			result := make([]directoryMailbox, len(boxes))
			for i, box := range boxes {
				result[i] = directoryMailbox(box)
			}
			return result, err
		},
		read: readEMLFile,
	})
}

type directoryMailbox struct {
	Path  string
	Label string
	Files []string
}

type rawDirectoryLayout struct {
	sourceType string
	discover   func(string) ([]directoryMailbox, error)
	labels     func(string) []string
	read       func(string, int64) ([]byte, error)
}

// importRawDirectory owns the shared source lease, checkpoint, and ingestion
// lifecycle for directory-based raw MIME formats.
func importRawDirectory(ctx context.Context, st *store.Store, root string,
	opts EMLImportOptions, layout rawDirectoryLayout,
) (retSummary *EMLImportSummary, retErr error) {
	// This native file dispatcher owns offline provenance. A Store supplied
	// by another caller cannot turn a file import into a live provider arrival.
	st = st.WithIngestContext(store.IngestContext{Mode: store.IngestBackfill})
	if opts.Identifier == "" {
		return nil, errors.New("identifier is required")
	}
	if err := st.ValidateNativeImportChoice(ctx, layout.sourceType, opts.NotifyImports); err != nil {
		return nil, err
	}
	if opts.SourceType == "" {
		opts.SourceType = layout.sourceType
	}
	if opts.CheckpointInterval <= 0 {
		opts.CheckpointInterval = 200
	}
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = defaultMaxEMLMessageBytes
	}
	ingestFn := opts.IngestFunc
	if ingestFn == nil {
		ingestFn = rawMessageIngester(opts.RemoteImages, opts.NotifyImports)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	started := time.Now()
	mailboxes, err := layout.discover(root)
	if err != nil {
		return nil, fmt.Errorf("discover directory mailboxes: %w", err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve directory root: %w", err)
	}
	summary := &EMLImportSummary{MailboxesTotal: len(mailboxes)}

	source, err := st.GetOrCreateSource(opts.SourceType, opts.Identifier)
	if err != nil {
		return nil, fmt.Errorf("get or create directory source: %w", err)
	}
	summary.SourceID = source.ID
	ownershipCtx := context.WithoutCancel(ctx)
	execution, err := st.AcquireSyncExecutionContext(ownershipCtx, source.ID)
	if err != nil {
		return nil, fmt.Errorf("acquire sync execution: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, execution.Release())
	}()
	rederive.Heal(ctx, slog.Default(), st, source)

	var (
		syncID     int64
		checkpoint store.Checkpoint
		startBox   int
	)
	// The native root and parser format identify the operation without putting
	// paths into event payloads. A changed request needs an explicit fresh run.
	requestHash := sha256.Sum256([]byte(layout.sourceType + "\x00" + absRoot))
	requestFingerprint := hex.EncodeToString(requestHash[:])
	var recoveredSyncID int64
	if !opts.NoResume {
		resumable, err := st.GetLatestCheckpointedSyncByType(source.ID, "import-"+layout.sourceType)
		if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
			return nil, fmt.Errorf("find resumable directory import: %w", err)
		}
		var saved emlCheckpoint
		var checkpointDecodeErr error
		// Check the native checkpoint identity before validating the durable
		// logical attempt fingerprint, so a different directory gets actionable
		// checkpoint guidance instead of an opaque import-run error.
		if resumable != nil {
			checkpointDecodeErr = json.Unmarshal([]byte(resumable.CursorBefore.String), &saved)
			if checkpointDecodeErr == nil && saved.RootDir != absRoot {
				return nil, fmt.Errorf(
					"saved directory import is for a different directory (%q), not %q; rerun with --no-resume to start fresh",
					saved.RootDir, absRoot,
				)
			}
		}
		recoveredSyncID, err = st.FindResumableImportAttempt(ownershipCtx, source.ID, layout.sourceType, requestFingerprint)
		if err != nil {
			return nil, fmt.Errorf("find resumable directory import: %w", err)
		}
		summary.WasResumed = recoveredSyncID != 0
		if resumable != nil {
			if recoveredSyncID == 0 && opts.NotifyImports {
				return nil, errors.New("legacy import checkpoint is silent; use --no-resume to start a new notifying run")
			}
			if checkpointDecodeErr != nil {
				return nil, fmt.Errorf("decode directory checkpoint: %w", checkpointDecodeErr)
			}
			if saved.MailboxIndex < 0 || saved.MailboxIndex >= len(mailboxes) {
				return nil, fmt.Errorf("directory checkpoint mailbox index %d is out of range", saved.MailboxIndex)
			}
			if saved.MailboxPath != "" && mailboxes[saved.MailboxIndex].Path != saved.MailboxPath {
				return nil, fmt.Errorf("directory mailbox tree changed at checkpoint index %d", saved.MailboxIndex)
			}
			checkpoint.MessagesProcessed = resumable.MessagesProcessed
			checkpoint.MessagesAdded = resumable.MessagesAdded
			checkpoint.MessagesUpdated = resumable.MessagesUpdated
			// Errors belong to this generation: the full rescan below retries
			// every failed file and counts any failures that remain.
			// Resume rescans the full tree. Raw-content IDs make successful
			// files idempotent, while a rescan also finds files inserted or
			// changed before the last lexical checkpoint.
			startBox = 0
			summary.WasResumed = true
		}
	}
	syncID, err = execution.StartSyncWithRequestContext(ownershipCtx, "import-"+layout.sourceType, "", requestFingerprint)
	if err != nil {
		return nil, fmt.Errorf("start directory import: %w", err)
	}
	st = st.ScopedToSync(source.ID, syncID)
	// Native control writes retain the execution fence after ingestion policy
	// is revoked. They only record progress or terminal failure, never content.
	progressStore := st.WithIngestContext(store.IngestContext{Mode: store.IngestBackfill})

	// A fatal return must not leave the run stuck as 'running'; intentional
	// cancellation keeps 'running' so the checkpoint stays resumable.
	failSync := func(msg string) {
		if fsErr := progressStore.FailSync(syncID, msg); fsErr != nil {
			log.Warn("failed to record sync failure", "error", fsErr)
		}
	}

	// Durable native run policy precedes even the initial progress checkpoint.
	// The parser layout supplies trusted format independently of SourceType.
	logicalRun, err := st.BeginNativeImportAttempt(ownershipCtx, source.ID, layout.sourceType, opts.NotifyImports, syncID, recoveredSyncID)
	if err != nil {
		failSync(err.Error())
		return nil, fmt.Errorf("begin directory import: %w", err)
	}
	summary.ImportRunID = logicalRun.ID
	importView, err := st.WithImportRun(ownershipCtx, logicalRun.ID)
	if err != nil {
		failSync(err.Error())
		return nil, fmt.Errorf("validate directory import: %w", err)
	}
	st = importView

	lastBox := startBox
	lastPath := mailboxes[startBox].Path
	lastFile := ""
	if err := saveEMLCheckpoint(progressStore, syncID, absRoot, lastBox, lastPath, lastFile, &checkpoint); err != nil {
		failSync(err.Error())
		return nil, fmt.Errorf("save initial directory checkpoint: %w", err)
	}

	hardErrors := false
	checkpointBlocked := false
	flagLabelIDs := make(map[string]int64)
	for boxIndex := startBox; boxIndex < len(mailboxes); boxIndex++ {
		mailbox := mailboxes[boxIndex]
		labelID, err := st.EnsureLabel(source.ID, mailbox.Label, mailbox.Label, "user")
		if err != nil {
			failSync(err.Error())
			return nil, fmt.Errorf("ensure directory label %q: %w", mailbox.Label, err)
		}

		for _, filename := range mailbox.Files {
			if err := ctx.Err(); err != nil {
				break
			}
			checkpoint.MessagesProcessed++
			summary.MessagesProcessed++
			raw, err := layout.read(filename, opts.MaxMessageBytes)
			if err != nil {
				checkpoint.ErrorsCount++
				summary.Errors++
				hardErrors = true
				checkpointBlocked = true
				log.Warn("failed to read directory message", "file", filename, "error", err)
				continue
			}

			labelIDs := []int64{labelID}
			if layout.labels != nil {
				for _, label := range layout.labels(filename) {
					id, ok := flagLabelIDs[label]
					if !ok {
						id, err = st.EnsureLabel(source.ID, label, label, "system")
						if err != nil {
							failSync(err.Error())
							return nil, fmt.Errorf("ensure message flag label: %w", err)
						}
						flagLabelIDs[label] = id
					}
					// A folder named like a flag (for example TRASH) shares
					// the flag's label row; listing it twice would violate
					// the message_labels primary key.
					if id != labelID {
						labelIDs = append(labelIDs, id)
					}
				}
			}
			hash := sha256.Sum256(raw)
			rawHash := hex.EncodeToString(hash[:])
			sourceMessageID := layout.sourceType + "-" + rawHash
			existing, err := st.MessageExistsWithRawBatch(source.ID, []string{sourceMessageID})
			if err != nil {
				failSync(err.Error())
				return nil, fmt.Errorf("check existing directory message: %w", err)
			}
			if messageID, ok := existing[sourceMessageID]; ok {
				if err := st.AddMessageLabels(messageID, labelIDs); err != nil {
					failSync(err.Error())
					return nil, fmt.Errorf("add directory label to existing message: %w", err)
				}
				summary.MessagesSkipped++
			} else {
				existingAny, err := st.MessageExistsBatch(source.ID, []string{sourceMessageID})
				if err != nil {
					failSync(err.Error())
					return nil, fmt.Errorf("check directory message metadata: %w", err)
				}
				_, updating := existingAny[sourceMessageID]
				if err := ingestFn(
					ctx, st, source.ID, opts.Identifier, opts.AttachmentsDir,
					labelIDs, sourceMessageID, rawHash, raw, time.Time{}, log,
				); err != nil {
					checkpoint.ErrorsCount++
					summary.Errors++
					hardErrors = true
					checkpointBlocked = true
					log.Warn("failed to ingest directory message", "file", filename, "error", err)
					continue
				}
				if updating {
					checkpoint.MessagesUpdated++
					summary.MessagesUpdated++
				} else {
					checkpoint.MessagesAdded++
					summary.MessagesAdded++
				}
			}

			if !checkpointBlocked {
				lastBox = boxIndex
				lastPath = mailbox.Path
				lastFile = filename
				if checkpoint.MessagesProcessed%int64(opts.CheckpointInterval) == 0 {
					if err := saveEMLCheckpoint(progressStore, syncID, absRoot, lastBox, lastPath, lastFile, &checkpoint); err != nil {
						failSync(err.Error())
						return nil, fmt.Errorf("save directory checkpoint: %w", err)
					}
				}
			}
		}
		if ctx.Err() != nil {
			break
		}
		summary.MailboxesImported++
	}

	summary.Duration = time.Since(started)
	summary.HardErrors = hardErrors
	if err := saveEMLCheckpoint(progressStore, syncID, absRoot, lastBox, lastPath, lastFile, &checkpoint); err != nil {
		failSync(err.Error())
		return nil, fmt.Errorf("save final directory checkpoint: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	if hardErrors {
		if err := progressStore.FailSync(syncID, fmt.Sprintf("completed with %d errors", checkpoint.ErrorsCount)); err != nil {
			return nil, fmt.Errorf("fail directory import: %w", err)
		}
		return summary, nil
	}
	if err := progressStore.CompleteSync(syncID, fmt.Sprintf("mailboxes:%d messages:%d", summary.MailboxesImported, summary.MessagesAdded)); err != nil {
		return nil, fmt.Errorf("complete directory import: %w", err)
	}
	return summary, nil
}

func readEMLFile(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("message exceeds %d-byte limit", maxBytes)
	}
	return raw, nil
}

func saveEMLCheckpoint(
	st *store.Store,
	syncID int64,
	root string,
	mailboxIndex int,
	mailboxPath string,
	lastFile string,
	checkpoint *store.Checkpoint,
) error {
	cursor, err := json.Marshal(emlCheckpoint{
		RootDir: root, MailboxIndex: mailboxIndex,
		MailboxPath: mailboxPath, LastFile: lastFile,
	}, json.Deterministic(true))
	if err != nil {
		return err
	}
	checkpoint.PageToken = string(cursor)
	return st.UpdateSyncCheckpoint(syncID, checkpoint)
}
