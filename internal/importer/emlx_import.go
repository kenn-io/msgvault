package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/wesm/msgvault/internal/emlx"
	"github.com/wesm/msgvault/internal/store"
)

// EmlxImportOptions configures an Apple Mail .emlx directory import.
type EmlxImportOptions struct {
	// SourceType is the sources.source_type value.
	// Defaults to "apple-mail".
	SourceType string

	// Identifier is the sources.identifier (e.g. "you@gmail.com").
	Identifier string

	// NoResume forces a fresh import even if a prior run exists.
	NoResume bool

	// CheckpointInterval controls how often (in messages) to persist
	// progress. Defaults to 200.
	CheckpointInterval int

	// AttachmentsDir controls where attachments are written.
	// Empty means no disk storage.
	AttachmentsDir string

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
	Errors            int64
	HardErrors        bool
}

type emlxCheckpoint struct {
	RootDir      string `json:"root_dir"`
	MailboxIndex int    `json:"mailbox_index"`
	MailboxPath  string `json:"mailbox_path,omitempty"`
	LastFile     string `json:"last_file"`
}

const defaultMaxEmlxBytes int64 = 128 << 20 // 128 MiB

// ImportEmlxDir imports .emlx files from an Apple Mail directory tree.
//
// Messages are deduplicated by content hash (sha256 of raw MIME).
// When the same message appears in multiple mailboxes, the first
// occurrence is fully ingested; subsequent occurrences add their
// mailbox label to the existing message.
func ImportEmlxDir(
	ctx context.Context, st *store.Store,
	rootDir string, opts EmlxImportOptions,
) (*EmlxImportSummary, error) {
	if opts.SourceType == "" {
		opts.SourceType = "apple-mail"
	}
	if opts.Identifier == "" {
		return nil, fmt.Errorf("identifier is required")
	}
	if opts.CheckpointInterval <= 0 {
		opts.CheckpointInterval = 200
	}
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = defaultMaxEmlxBytes
	}
	ingestFn := opts.IngestFunc
	if ingestFn == nil {
		ingestFn = IngestRawMessage
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	start := time.Now()
	summary := &EmlxImportSummary{}

	// Discover mailboxes.
	mailboxes, err := emlx.DiscoverMailboxes(rootDir)
	if err != nil {
		return nil, fmt.Errorf("discover mailboxes: %w", err)
	}
	summary.MailboxesTotal = len(mailboxes)
	if len(mailboxes) == 0 {
		summary.Duration = time.Since(start)
		return summary, nil
	}

	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("abs path: %w", err)
	}

	src, err := st.GetOrCreateSource(opts.SourceType, opts.Identifier)
	if err != nil {
		return nil, fmt.Errorf("get/create source: %w", err)
	}
	summary.SourceID = src.ID

	// Resume support.
	var (
		syncID     int64
		cp         store.Checkpoint
		startMbox  int
		startAfter string // skip files <= this name within the start mailbox
	)

	if !opts.NoResume {
		active, err := st.GetActiveSync(src.ID)
		if err != nil {
			return nil, fmt.Errorf("check active sync: %w", err)
		}
		if active != nil {
			if active.CursorBefore.Valid &&
				active.CursorBefore.String != "" {
				var ecp emlxCheckpoint
				if err := json.Unmarshal(
					[]byte(active.CursorBefore.String), &ecp,
				); err == nil {
					if ecp.RootDir != absRoot {
						return nil, fmt.Errorf(
							"active emlx import is for a different directory (%q), not %q; rerun with --no-resume to start fresh",
							ecp.RootDir, absRoot,
						)
					}
					if ecp.MailboxIndex < 0 ||
						ecp.MailboxIndex >= len(mailboxes) {
						return nil, fmt.Errorf(
							"checkpoint mailbox index %d out of range (%d mailboxes); rerun with --no-resume to start fresh",
							ecp.MailboxIndex, len(mailboxes),
						)
					}
					// Validate mailbox path if present (added in later versions).
					if ecp.MailboxPath != "" &&
						mailboxes[ecp.MailboxIndex].Path != ecp.MailboxPath {
						return nil, fmt.Errorf(
							"mailbox at index %d changed (%q -> %q); rerun with --no-resume to start fresh",
							ecp.MailboxIndex, ecp.MailboxPath,
							mailboxes[ecp.MailboxIndex].Path,
						)
					}
					syncID = active.ID
					cp.MessagesProcessed = active.MessagesProcessed
					cp.MessagesAdded = active.MessagesAdded
					cp.MessagesUpdated = active.MessagesUpdated
					cp.ErrorsCount = active.ErrorsCount
					startMbox = ecp.MailboxIndex
					startAfter = ecp.LastFile
					// Legacy checkpoints stored bare filenames
					// (e.g. "1.emlx"); resolve against the actual
					// file list to find the full path. This handles
					// partitioned V10 layouts where files may be in
					// different Messages/ subdirectories.
					if startAfter != "" &&
						!filepath.IsAbs(startAfter) {
						resolved, resolveErr := resolveCheckpointFile(
							startAfter,
							mailboxes[ecp.MailboxIndex].Files,
						)
						if resolveErr != nil {
							return nil, resolveErr
						}
						startAfter = resolved
					}
					summary.WasResumed = true
					log.Info("resuming emlx import",
						"root", absRoot,
						"mailbox_index", startMbox,
						"last_file", startAfter,
						"processed", cp.MessagesProcessed,
					)
				}
			}
		}
	}

	if syncID == 0 {
		syncID, err = st.StartSync(src.ID, "import-emlx")
		if err != nil {
			return nil, fmt.Errorf("start sync: %w", err)
		}
	}

	hardErrors := false

	type pendingEmlxMsg struct {
		Raw       []byte
		RawHash   string
		SourceMsg string
		LabelIDs  []int64
		Fallback  time.Time
		MboxIdx   int
		MboxPath  string
		FileName  string
	}

	const (
		batchSize  = 200
		batchBytes = 32 << 20 // 32 MiB
	)

	var pending []pendingEmlxMsg
	var pendingBytes int64
	pendingIdx := make(map[string]int) // SourceMsg → index in pending
	lastCpMbox := startMbox
	lastCpMboxPath := ""
	if startMbox < len(mailboxes) {
		lastCpMboxPath = mailboxes[startMbox].Path
	}
	lastCpFile := startAfter
	checkpointBlocked := false

	// Save initial checkpoint.
	if err := saveEmlxCheckpoint(
		st, syncID, absRoot, startMbox, lastCpMboxPath,
		startAfter, &cp,
	); err != nil {
		cp.ErrorsCount++
		summary.Errors++
		log.Warn("failed to save initial checkpoint", "error", err)
	}

	flushPending := func() (bool, error) {
		if len(pending) == 0 {
			return false, nil
		}

		ids := make([]string, len(pending))
		for i, p := range pending {
			ids[i] = p.SourceMsg
		}

		existingWithRaw, err := st.MessageExistsWithRawBatch(src.ID, ids)
		batchOK := err == nil
		if err != nil {
			cp.ErrorsCount++
			summary.Errors++
			log.Warn("existence check failed", "error", err)
		}

		existingAny, err := st.MessageExistsBatch(src.ID, ids)
		anyOK := err == nil
		if err != nil {
			cp.ErrorsCount++
			summary.Errors++
			log.Warn("existence check failed (any)", "error", err)
		}

		for _, p := range pending {
			if err := ctx.Err(); err != nil {
				summary.Duration = time.Since(start)
				if err := saveEmlxCheckpoint(
					st, syncID, absRoot, lastCpMbox,
					lastCpMboxPath, lastCpFile, &cp,
				); err != nil {
					log.Warn("checkpoint save failed", "error", err)
				}
				return true, nil
			}

			cp.MessagesProcessed++
			summary.MessagesProcessed++

			// Check if fully exists (with raw).
			exists := false
			if batchOK {
				msgID, ok := existingWithRaw[p.SourceMsg]
				if ok {
					exists = true
					// Add labels from this mailbox to the existing message.
					if len(p.LabelIDs) > 0 {
						if err := st.AddMessageLabels(
							msgID, p.LabelIDs,
						); err != nil {
							log.Warn("failed to add labels to existing message",
								"message_id", msgID, "error", err,
							)
						}
					}
				}
			} else {
				one, err := st.MessageExistsWithRawBatch(
					src.ID, []string{p.SourceMsg},
				)
				if err != nil {
					cp.ErrorsCount++
					summary.Errors++
				} else if msgID, ok := one[p.SourceMsg]; ok {
					exists = true
					if len(p.LabelIDs) > 0 {
						if err := st.AddMessageLabels(
							msgID, p.LabelIDs,
						); err != nil {
							log.Warn("failed to add labels",
								"message_id", msgID, "error", err,
							)
						}
					}
				}
			}

			if exists {
				summary.MessagesSkipped++
				if !checkpointBlocked {
					lastCpMbox = p.MboxIdx
					lastCpMboxPath = p.MboxPath
					lastCpFile = p.FileName
					checkpointIfDue(
						&cp, summary, opts.CheckpointInterval,
						st, syncID, absRoot, lastCpMbox,
						lastCpMboxPath, lastCpFile, log,
					)
				}
				continue
			}

			alreadyExists := false
			if anyOK {
				_, alreadyExists = existingAny[p.SourceMsg]
			}

			if err := ingestFn(
				ctx, st, src.ID, opts.Identifier,
				opts.AttachmentsDir, p.LabelIDs,
				p.SourceMsg, p.RawHash,
				p.Raw, p.Fallback, log,
			); err != nil {
				cp.ErrorsCount++
				summary.Errors++
				log.Warn("failed to ingest message",
					"source_msg", p.SourceMsg,
					"file", p.FileName,
					"error", err,
				)
				checkpointBlocked = true
				hardErrors = true
				continue
			}

			if alreadyExists {
				cp.MessagesUpdated++
				summary.MessagesUpdated++
			} else {
				cp.MessagesAdded++
				summary.MessagesAdded++
			}

			if !checkpointBlocked {
				lastCpMbox = p.MboxIdx
				lastCpMboxPath = p.MboxPath
				lastCpFile = p.FileName
				checkpointIfDue(
					&cp, summary, opts.CheckpointInterval,
					st, syncID, absRoot, lastCpMbox,
					lastCpMboxPath, lastCpFile, log,
				)
			}
		}

		clear(pending)
		pending = pending[:0]
		pendingBytes = 0
		clear(pendingIdx)
		return false, nil
	}

	for mboxIdx := startMbox; mboxIdx < len(mailboxes); mboxIdx++ {
		mb := mailboxes[mboxIdx]

		labelID, err := st.EnsureLabel(
			src.ID, mb.Label, mb.Label, "user",
		)
		if err != nil {
			cp.ErrorsCount++
			summary.Errors++
			log.Warn("failed to ensure label",
				"label", mb.Label, "error", err,
			)
			continue
		}
		labelIDs := []int64{labelID}

		log.Info("importing mailbox",
			"label", mb.Label,
			"files", len(mb.Files),
			"index", mboxIdx,
		)

		for _, filePath := range mb.Files {
			if ctx.Err() != nil {
				break
			}

			// Resume: skip files already processed.
			if mboxIdx == startMbox && startAfter != "" {
				if filePath <= startAfter {
					continue
				}
			}

			// Check file size before reading to avoid OOM on oversized files.
			fi, statErr := os.Stat(filePath)
			if statErr != nil {
				cp.ErrorsCount++
				summary.Errors++
				log.Warn("failed to stat .emlx",
					"file", filePath, "error", statErr,
				)
				continue
			}
			if fi.Size() > opts.MaxMessageBytes {
				cp.ErrorsCount++
				summary.Errors++
				log.Warn("file exceeds size limit",
					"file", filePath,
					"size", fi.Size(),
					"limit", opts.MaxMessageBytes,
				)
				continue
			}

			msg, err := emlx.ParseFile(filePath)
			if err != nil {
				cp.ErrorsCount++
				summary.Errors++
				log.Warn("failed to parse .emlx",
					"file", filePath, "error", err,
				)
				continue
			}

			sum := sha256.Sum256(msg.Raw)
			rawHash := hex.EncodeToString(sum[:])
			sourceMsgID := "emlx-" + rawHash

			var fallbackDate time.Time
			if !msg.PlistDate.IsZero() {
				fallbackDate = msg.PlistDate
			}

			if idx, dup := pendingIdx[sourceMsgID]; dup {
				// Same content from another mailbox (or duplicate file
				// within the same mailbox); merge labels, deduplicating
				// to avoid unique constraint violations in message_labels.
				existing := pending[idx].LabelIDs
				for _, lid := range labelIDs {
					found := false
					for _, eid := range existing {
						if eid == lid {
							found = true
							break
						}
					}
					if !found {
						existing = append(existing, lid)
					}
				}
				pending[idx].LabelIDs = existing
			} else {
				pendingIdx[sourceMsgID] = len(pending)
				pending = append(pending, pendingEmlxMsg{
					Raw:       msg.Raw,
					RawHash:   rawHash,
					SourceMsg: sourceMsgID,
					LabelIDs:  labelIDs,
					Fallback:  fallbackDate,
					MboxIdx:   mboxIdx,
					MboxPath:  mb.Path,
					FileName:  filePath,
				})
				pendingBytes += int64(len(msg.Raw))
			}

			if len(pending) >= batchSize || pendingBytes >= batchBytes {
				stop, err := flushPending()
				if err != nil {
					return summary, err
				}
				if stop {
					return summary, nil
				}
			}
		}

		// Flush remaining for this mailbox.
		if stop, err := flushPending(); err != nil {
			return summary, err
		} else if stop {
			return summary, nil
		}

		summary.MailboxesImported++

		if ctx.Err() != nil {
			break
		}
	}

	summary.Duration = time.Since(start)
	summary.HardErrors = hardErrors

	// Final checkpoint.
	if err := saveEmlxCheckpoint(
		st, syncID, absRoot, lastCpMbox, lastCpMboxPath,
		lastCpFile, &cp,
	); err != nil {
		cp.ErrorsCount++
		summary.Errors++
		log.Warn("failed to save final checkpoint", "error", err)
	}

	// If cancelled, leave the sync run as "running" so resume works.
	if ctx.Err() != nil {
		return summary, nil
	}

	if hardErrors {
		if err := st.FailSync(syncID, fmt.Sprintf(
			"completed with %d errors", cp.ErrorsCount,
		)); err != nil {
			return summary, fmt.Errorf("fail sync: %w", err)
		}
		return summary, nil
	}

	finalMsg := fmt.Sprintf(
		"mailboxes:%d messages:%d",
		summary.MailboxesImported, summary.MessagesAdded,
	)
	if cp.ErrorsCount > 0 {
		finalMsg = fmt.Sprintf(
			"mailboxes:%d messages:%d errors:%d",
			summary.MailboxesImported, summary.MessagesAdded,
			cp.ErrorsCount,
		)
	}
	if err := st.CompleteSync(syncID, finalMsg); err != nil {
		return summary, fmt.Errorf("complete sync: %w", err)
	}

	return summary, nil
}

// resolveCheckpointFile finds the full path in files whose basename
// matches the legacy bare filename from an older checkpoint. Returns
// the first matching full path so that no previously-unseen partition
// files are skipped. Returns an error if no match is found, since
// comparing a bare filename against absolute paths would produce
// incorrect resume behavior.
func resolveCheckpointFile(
	basename string, files []string,
) (string, error) {
	for _, f := range files {
		if filepath.Base(f) == basename {
			return f, nil
		}
	}
	return "", fmt.Errorf(
		"legacy checkpoint file %q not found in current mailbox; rerun with --no-resume to start fresh",
		basename,
	)
}

func saveEmlxCheckpoint(
	st *store.Store, syncID int64,
	rootDir string, mboxIdx int, mboxPath string,
	lastFile string, cp *store.Checkpoint,
) error {
	b, err := json.Marshal(emlxCheckpoint{
		RootDir:      rootDir,
		MailboxIndex: mboxIdx,
		MailboxPath:  mboxPath,
		LastFile:     lastFile,
	})
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	cp.PageToken = string(b)
	return st.UpdateSyncCheckpoint(syncID, cp)
}

func checkpointIfDue(
	cp *store.Checkpoint, summary *EmlxImportSummary,
	interval int,
	st *store.Store, syncID int64,
	rootDir string, mboxIdx int, mboxPath string,
	lastFile string, log *slog.Logger,
) {
	if cp.MessagesProcessed%int64(interval) != 0 {
		return
	}
	if err := saveEmlxCheckpoint(
		st, syncID, rootDir, mboxIdx, mboxPath, lastFile, cp,
	); err != nil {
		cp.ErrorsCount++
		summary.Errors++
		log.Warn("failed to save checkpoint", "error", err)
	}
}
