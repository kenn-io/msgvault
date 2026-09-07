package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/msgvault/internal/eml"
	"go.kenn.io/msgvault/internal/store"
)

const defaultMaxEMLMessageBytes int64 = 128 << 20

// EMLImportOptions configures an import from a tree of .mailbox directories.
type EMLImportOptions struct {
	SourceType         string
	Identifier         string
	NoResume           bool
	CheckpointInterval int
	AttachmentsDir     string
	MaxMessageBytes    int64
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
	if opts.Identifier == "" {
		return nil, errors.New("identifier is required")
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
		ingestFn = IngestRawMessage
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

	var (
		syncID     int64
		checkpoint store.Checkpoint
		startBox   int
	)
	if !opts.NoResume {
		resumable, err := st.GetLatestCheckpointedSyncByType(source.ID, "import-"+layout.sourceType)
		if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
			return nil, fmt.Errorf("find resumable directory import: %w", err)
		}
		if resumable != nil {
			var saved emlCheckpoint
			if err := json.Unmarshal([]byte(resumable.CursorBefore.String), &saved); err != nil {
				return nil, fmt.Errorf("decode directory checkpoint: %w", err)
			}
			if saved.RootDir != absRoot {
				return nil, fmt.Errorf(
					"saved directory import is for a different directory (%q), not %q; rerun with --no-resume to start fresh",
					saved.RootDir, absRoot,
				)
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
	syncID, err = execution.StartSyncContext(ownershipCtx, "import-"+layout.sourceType, "")
	if err != nil {
		return nil, fmt.Errorf("start directory import: %w", err)
	}
	st = st.ScopedToSync(source.ID, syncID)

	// A fatal return must not leave the run stuck as 'running'; intentional
	// cancellation keeps 'running' so the checkpoint stays resumable.
	failSync := func(msg string) {
		if fsErr := st.FailSync(syncID, msg); fsErr != nil {
			log.Warn("failed to record sync failure", "error", fsErr)
		}
	}

	lastBox := startBox
	lastPath := mailboxes[startBox].Path
	lastFile := ""
	if err := saveEMLCheckpoint(st, syncID, absRoot, lastBox, lastPath, lastFile, &checkpoint); err != nil {
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
					labelIDs = append(labelIDs, id)
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
					if err := saveEMLCheckpoint(st, syncID, absRoot, lastBox, lastPath, lastFile, &checkpoint); err != nil {
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
	if err := saveEMLCheckpoint(st, syncID, absRoot, lastBox, lastPath, lastFile, &checkpoint); err != nil {
		failSync(err.Error())
		return nil, fmt.Errorf("save final directory checkpoint: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	if hardErrors {
		if err := st.FailSync(syncID, fmt.Sprintf("completed with %d errors", checkpoint.ErrorsCount)); err != nil {
			return nil, fmt.Errorf("fail directory import: %w", err)
		}
		return summary, nil
	}
	if err := st.CompleteSync(syncID, fmt.Sprintf("mailboxes:%d messages:%d", summary.MailboxesImported, summary.MessagesAdded)); err != nil {
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
	})
	if err != nil {
		return err
	}
	checkpoint.PageToken = string(cursor)
	return st.UpdateSyncCheckpoint(syncID, checkpoint)
}
