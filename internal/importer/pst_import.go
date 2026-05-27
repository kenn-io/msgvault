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
	"strings"
	"time"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	pstreader "go.kenn.io/msgvault/internal/pst"
	"go.kenn.io/msgvault/internal/store"
)

// PstImportOptions configures a PST import operation.
type PstImportOptions struct {
	// SourceType is the sources.source_type value. Defaults to "pst".
	SourceType string

	// Identifier is the email address for this source (required).
	Identifier string

	// SkipFolders is a list of folder names to skip (case-insensitive).
	// E.g. []string{"Deleted Items", "Junk Email", "Trash"}.
	SkipFolders []string

	// NoResume forces a fresh import even if an active sync run exists.
	NoResume bool

	// CheckpointInterval controls how often (in messages) to save progress.
	// Defaults to 200.
	CheckpointInterval int

	// AttachmentsDir controls where attachment files are written.
	// Empty string disables disk storage (messages still imported).
	AttachmentsDir string

	// MaxMessageBytes limits the total byte size (body + attachments) read
	// per message. Defaults to 128 MiB.
	MaxMessageBytes int64

	// IngestFunc allows tests to override message ingestion.
	IngestFunc func(
		ctx context.Context, st *store.Store,
		sourceID int64, identifier, attachmentsDir string,
		labelIDs []int64, sourceMsgID, rawHash string,
		raw []byte, fallbackDate time.Time,
		log *slog.Logger,
	) error

	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// PstImportSummary reports the results of a PST import.
type PstImportSummary struct {
	WasResumed bool
	Duration   time.Duration

	FoldersTotal    int
	FoldersImported int

	MessagesProcessed int64
	MessagesAdded     int64
	MessagesUpdated   int64
	MessagesSkipped   int64
	Errors            int64
	HardErrors        bool
}

// pstCheckpoint tracks resume state for PST imports. ArchiveID is the
// content-based fingerprint from pstArchiveFingerprint and is the
// authoritative identity check during resume — path/inode comparisons
// can match a replaced file (same path, same inode after copy-over)
// whose bytes have changed, which would silently skip messages at the
// resumed offset.
type pstCheckpoint struct {
	File        string `json:"file"`
	ArchiveID   string `json:"archive_id,omitempty"`
	FolderIndex int    `json:"folder_index"`
	FolderPath  string `json:"folder_path"`
	MsgIndex    int64  `json:"msg_index"`
}

const defaultMaxPstMessageBytes int64 = 128 << 20 // 128 MiB

// ImportPst imports all email messages from a PST file into the msgvault database.
//
// Folder structure is preserved as labels. Non-email items (calendar, contacts,
// tasks) are skipped automatically. The import is resumable: if interrupted,
// rerunning with the same arguments continues from where it left off.
func ImportPst(ctx context.Context, st *store.Store, pstPath string, opts PstImportOptions) (*PstImportSummary, error) {
	if opts.SourceType == "" {
		opts.SourceType = "pst"
	}
	if opts.Identifier == "" {
		return nil, errors.New("identifier is required")
	}
	if opts.CheckpointInterval <= 0 {
		opts.CheckpointInterval = 200
	}
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = defaultMaxPstMessageBytes
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
	summary := &PstImportSummary{}

	absPath, err := filepath.Abs(pstPath)
	if err != nil {
		return nil, fmt.Errorf("abs path: %w", err)
	}
	cpFile := absPath
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		cpFile = resolved
	}

	// Per-archive fingerprint namespaces source_message_id so importing
	// multiple PST files into the same source can't collide on EntryIDs
	// (PST EntryIDs are only unique within a single archive). Derived from
	// the PST header bytes so the fingerprint is stable across re-imports of
	// the same file regardless of path, preserving idempotence.
	archiveID, err := pstArchiveFingerprint(absPath)
	if err != nil {
		return nil, fmt.Errorf("fingerprint pst: %w", err)
	}

	// Build skip-folder set (case-insensitive).
	skipFolders := make(map[string]bool, len(opts.SkipFolders))
	for _, f := range opts.SkipFolders {
		skipFolders[strings.ToLower(f)] = true
	}

	// Get or create source.
	src, err := st.GetOrCreateSource(opts.SourceType, opts.Identifier)
	if err != nil {
		return nil, fmt.Errorf("get/create source: %w", err)
	}

	// Set display name to the PST filename so it appears in list-accounts / get_stats.
	pstBase := filepath.Base(absPath)
	if !src.DisplayName.Valid || src.DisplayName.String == "" {
		if err := st.UpdateSourceDisplayName(src.ID, pstBase); err != nil {
			log.Warn("failed to set source display name", "error", err)
		} else {
			src.DisplayName.Valid = true
			src.DisplayName.String = pstBase
		}
	}

	// Resume or start sync.
	var (
		syncID int64
		cp     store.Checkpoint
		resume pstCheckpoint
	)

	if !opts.NoResume {
		active, err := st.GetActiveSync(src.ID)
		if err != nil {
			return nil, fmt.Errorf("check active sync: %w", err)
		}
		if active != nil {
			syncID = active.ID
			cp.MessagesProcessed = active.MessagesProcessed
			cp.MessagesAdded = active.MessagesAdded
			cp.MessagesUpdated = active.MessagesUpdated
			cp.ErrorsCount = active.ErrorsCount
			if active.CursorBefore.Valid && active.CursorBefore.String != "" {
				var saved pstCheckpoint
				if err := json.Unmarshal([]byte(active.CursorBefore.String), &saved); err == nil {
					sameFile := saved.File == absPath || saved.File == cpFile
					if !sameFile && saved.File != "" {
						if curInfo, err := os.Stat(absPath); err == nil {
							if cpInfo, err := os.Stat(saved.File); err == nil && os.SameFile(curInfo, cpInfo) {
								sameFile = true
							}
						}
					}
					// If the checkpoint records an ArchiveID and it
					// differs from the current file's fingerprint, the
					// PST at this path has been replaced (e.g.,
					// re-exported from the mail client). Path/inode
					// equality is not sufficient — a copy-over can keep
					// the same path and inode while the byte content
					// changes, which would otherwise resume at an offset
					// that points into different messages and silently
					// skip everything before it. Treat as a user-
					// initiated restart: drop the offsets, log loudly,
					// and re-import from the beginning.
					archiveChanged := saved.ArchiveID != "" && saved.ArchiveID != archiveID
					if sameFile && archiveChanged {
						log.Warn("pst archive fingerprint changed since last checkpoint; restarting from the beginning",
							"file", absPath,
							"saved_archive_id", saved.ArchiveID,
							"current_archive_id", archiveID,
						)
						summary.WasResumed = false
					} else if sameFile {
						resume = saved
						summary.WasResumed = true
						log.Info("resuming pst import",
							"file", absPath,
							"folder_index", resume.FolderIndex,
							"msg_index", resume.MsgIndex,
						)
					} else if saved.File != "" {
						return nil, fmt.Errorf("active pst import is for %q, not %q; rerun with --no-resume", saved.File, absPath)
					}
				}
			}
		}
	}

	if syncID == 0 {
		syncID, err = st.StartSync(src.ID, "import-pst")
		if err != nil {
			return nil, fmt.Errorf("start sync: %w", err)
		}
	}

	failSync := func(msg string) {
		if fsErr := st.FailSync(syncID, msg); fsErr != nil {
			log.Warn("failed to record sync failure", "error", fsErr)
		}
	}

	// Save initial checkpoint only for new syncs; resuming preserves the existing cursor.
	if !summary.WasResumed {
		if err := savePstCheckpoint(st, syncID, cpFile, archiveID, 0, "", 0, &cp); err != nil {
			log.Warn("failed to save initial checkpoint", "error", err)
		}
	}

	// Open PST file.
	pstFile, err := pstreader.Open(absPath)
	if err != nil {
		failSync(err.Error())
		return nil, fmt.Errorf("open pst: %w", err)
	}
	defer func() { _ = pstFile.Close() }()

	// Collect all email folders in traversal order.
	type folderRecord struct {
		Entry  pstreader.FolderEntry
		Folder *pstlib.Folder
	}
	var folders []folderRecord

	if err := pstFile.WalkFolders(func(entry pstreader.FolderEntry, folder *pstlib.Folder) error {
		if skipFolders[strings.ToLower(entry.Name)] {
			log.Debug("skipping folder", "path", entry.Path)
			return nil
		}
		// Only collect folders that may contain messages.
		if folder.MessageCount > 0 {
			f := *folder // copy value
			folders = append(folders, folderRecord{Entry: entry, Folder: &f})
		}
		return nil
	}); err != nil {
		failSync(err.Error())
		return nil, fmt.Errorf("walk folders: %w", err)
	}

	summary.FoldersTotal = len(folders)

	// Validate resume folder still matches. Check whenever FolderPath is set,
	// including index 0, so a path change in the first folder is caught.
	if summary.WasResumed && resume.FolderPath != "" {
		if resume.FolderIndex >= len(folders) {
			log.Warn("resume folder index out of range; restarting from beginning",
				"saved_index", resume.FolderIndex,
				"folder_count", len(folders),
			)
			resume.FolderIndex = 0
			resume.MsgIndex = 0
		} else if folders[resume.FolderIndex].Entry.Path != resume.FolderPath {
			log.Warn("resume folder path mismatch; restarting from beginning",
				"saved_path", resume.FolderPath,
				"actual_path", folders[resume.FolderIndex].Entry.Path,
			)
			resume.FolderIndex = 0
			resume.MsgIndex = 0
		}
	}

	// Batching constants (same as MBOX/EMLX importers).
	const (
		batchSize  = 200
		batchBytes = 32 << 20 // 32 MiB
	)

	type pendingPstMessage struct {
		Raw          []byte
		RawHash      string
		SourceMsgID  string
		FallbackDate time.Time
		LabelID      int64
		FolderIndex  int
		FolderPath   string
		MsgIndex     int64
	}

	var (
		pending           []pendingPstMessage
		pendingBytes      int64
		checkpointBlocked bool
		hardErrors        bool
		currentMsgIdx     int64
	)

	saveCp := func(fi int, fp string, mi int64) {
		if err := savePstCheckpoint(st, syncID, cpFile, archiveID, fi, fp, mi, &cp); err != nil {
			cp.ErrorsCount++
			summary.Errors++
			log.Warn("failed to save checkpoint", "error", err)
		}
	}

	flushPending := func() (stop bool) {
		if len(pending) == 0 {
			return false
		}

		ids := make([]string, len(pending))
		for i, p := range pending {
			ids[i] = p.SourceMsgID
		}

		existingWithRaw, errWithRaw := st.MessageExistsWithRawBatch(src.ID, ids)
		if errWithRaw != nil {
			cp.ErrorsCount++
			summary.Errors++
			log.Warn("existence check failed", "error", errWithRaw)
		}

		existingAny, errAny := st.MessageExistsBatch(src.ID, ids)
		if errAny != nil {
			cp.ErrorsCount++
			summary.Errors++
			log.Warn("existence check (any) failed", "error", errAny)
		}

		for _, p := range pending {
			if ctx.Err() != nil {
				saveCp(p.FolderIndex, p.FolderPath, p.MsgIndex)
				summary.Duration = time.Since(start)
				return true
			}

			cp.MessagesProcessed++
			summary.MessagesProcessed++

			// Deduplicate: if exists with raw, just ensure labels are applied.
			if errWithRaw == nil {
				if msgID, exists := existingWithRaw[p.SourceMsgID]; exists {
					summary.MessagesSkipped++
					if p.LabelID != 0 {
						if err := st.AddMessageLabels(msgID, []int64{p.LabelID}); err != nil {
							log.Warn("add labels to existing message", "error", err)
						}
					}
					if !checkpointBlocked && cp.MessagesProcessed%int64(opts.CheckpointInterval) == 0 {
						saveCp(p.FolderIndex, p.FolderPath, p.MsgIndex)
					}
					continue
				}
			} else {
				// Fall back to individual check.
				one, err := st.MessageExistsWithRawBatch(src.ID, []string{p.SourceMsgID})
				if err == nil {
					if msgID, exists := one[p.SourceMsgID]; exists {
						summary.MessagesSkipped++
						if p.LabelID != 0 {
							_ = st.AddMessageLabels(msgID, []int64{p.LabelID})
						}
						continue
					}
				}
			}

			alreadyExists := false
			if errAny == nil {
				_, alreadyExists = existingAny[p.SourceMsgID]
			}

			lblIDs := []int64{}
			if p.LabelID != 0 {
				lblIDs = []int64{p.LabelID}
			}

			if err := ingestFn(ctx, st, src.ID, opts.Identifier, opts.AttachmentsDir,
				lblIDs, p.SourceMsgID, p.RawHash, p.Raw, p.FallbackDate, log,
			); err != nil {
				cp.ErrorsCount++
				summary.Errors++
				log.Warn("failed to ingest message", "source_msg_id", p.SourceMsgID, "error", err)
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

			if !checkpointBlocked && cp.MessagesProcessed%int64(opts.CheckpointInterval) == 0 {
				saveCp(p.FolderIndex, p.FolderPath, p.MsgIndex)
			}
		}

		clear(pending)
		pending = pending[:0]
		pendingBytes = 0
		// Reset checkpoint blocking so future successful batches can checkpoint.
		// checkpointBlocked is set when an ingest error occurs within a batch;
		// once the batch completes, we allow checkpointing again.
		checkpointBlocked = false
		return false
	}

	// Process each folder.
	labelCache := make(map[string]int64) // path → label ID

	for fi, fr := range folders {
		if ctx.Err() != nil {
			break
		}

		// Resume: skip folders we've already completed.
		if summary.WasResumed && fi < resume.FolderIndex {
			continue
		}
		currentMsgIdx = 0

		entry := fr.Entry
		folder := fr.Folder

		log.Debug("processing folder", "path", entry.Path, "count", entry.MsgCount)

		// Ensure label for this folder.
		labelID, ok := labelCache[entry.Path]
		if !ok {
			lid, err := st.EnsureLabel(src.ID, entry.Path, entry.Name, "user")
			if err != nil {
				cp.ErrorsCount++
				summary.Errors++
				log.Warn("ensure label failed", "path", entry.Path, "error", err)
				lid = 0
			}
			labelCache[entry.Path] = lid
			labelID = lid
		}

		msgIter, err := folder.GetMessageIterator()
		if err != nil {
			// ErrMessagesNotFound is expected for empty folders.
			cp.ErrorsCount++
			summary.Errors++
			log.Warn("get message iterator failed", "path", entry.Path, "error", err)
			continue
		}

		summary.FoldersImported++

		for msgIter.Next() {
			if ctx.Err() != nil {
				break
			}

			currentMsgIdx++

			// Resume: skip messages already processed in the resumed folder.
			if summary.WasResumed && fi == resume.FolderIndex && currentMsgIdx <= resume.MsgIndex {
				continue
			}

			msg := msgIter.Value()

			entry := pstreader.ExtractMessage(msg, folder.Name)
			if entry == nil {
				// Not an email (calendar, contact, task, etc.) — skip silently.
				continue
			}

			attachments, err := pstreader.ReadAttachments(msg, opts.MaxMessageBytes)
			if err != nil {
				cp.ErrorsCount++
				summary.Errors++
				log.Warn("read attachments failed", "entry_id", entry.EntryID, "error", err)
				// Continue without attachments rather than skipping the message.
				attachments = nil
			}

			raw, err := pstreader.BuildRFC5322(entry, attachments)
			if err != nil {
				cp.ErrorsCount++
				summary.Errors++
				log.Warn("build RFC5322 failed", "entry_id", entry.EntryID, "error", err)
				continue
			}

			if opts.MaxMessageBytes > 0 && int64(len(raw)) > opts.MaxMessageBytes {
				cp.ErrorsCount++
				summary.Errors++
				log.Warn("message exceeds size limit; skipping",
					"entry_id", entry.EntryID,
					"size", len(raw),
					"limit", opts.MaxMessageBytes,
				)
				continue
			}

			sum := sha256.Sum256(raw)
			rawHash := hex.EncodeToString(sum[:])
			// PST EntryIDs are unique only within a single PST file, so a bare
			// "pst-<EntryID>" key would collide across archives sharing one
			// source. Prefix with the archive fingerprint to keep dedup
			// idempotent for re-imports of the same file while isolating
			// different files imported under the same identifier.
			sourceMsgID := fmt.Sprintf("pst-%s-%s", archiveID, entry.EntryID)

			fallbackDate := entry.SentAt
			if fallbackDate.IsZero() {
				fallbackDate = entry.ReceivedAt
			}
			if fallbackDate.IsZero() {
				fallbackDate = entry.CreationTime
			}

			pending = append(pending, pendingPstMessage{
				Raw:          raw,
				RawHash:      rawHash,
				SourceMsgID:  sourceMsgID,
				FallbackDate: fallbackDate,
				LabelID:      labelID,
				FolderIndex:  fi,
				FolderPath:   fr.Entry.Path,
				MsgIndex:     currentMsgIdx,
			})
			pendingBytes += int64(len(raw))

			if len(pending) >= batchSize || pendingBytes >= batchBytes {
				if stop := flushPending(); stop {
					summary.HardErrors = hardErrors
					return summary, nil
				}
			}
		}

		if err := msgIter.Err(); err != nil {
			cp.ErrorsCount++
			summary.Errors++
			log.Warn("message iterator error", "path", fr.Entry.Path, "error", err)
		}
	}

	// Flush any remaining messages.
	if stop := flushPending(); stop {
		summary.HardErrors = hardErrors
		return summary, nil
	}

	summary.Duration = time.Since(start)
	summary.HardErrors = hardErrors

	finalMsg := fmt.Sprintf("folders:%d messages:%d", summary.FoldersImported, summary.MessagesProcessed)
	if hardErrors {
		if err := st.FailSync(syncID, fmt.Sprintf("completed with %d errors", cp.ErrorsCount)); err != nil {
			return summary, fmt.Errorf("fail sync: %w", err)
		}
		return summary, nil
	}

	if err := st.CompleteSync(syncID, finalMsg); err != nil {
		return summary, fmt.Errorf("complete sync: %w", err)
	}

	return summary, nil
}

// pstArchiveFingerprint returns a short stable identifier for a PST file,
// derived from a SHA-256 over its first 4 KiB. The PST header in that range
// embeds counters (next BID, next NID, root NIDs) that differ across files
// even when generated from the same template, so the prefix uniquely
// distinguishes archives. Re-importing the same bytes yields the same
// fingerprint regardless of path, preserving idempotence.
func pstArchiveFingerprint(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	var buf [4096]byte
	n, err := io.ReadFull(f, buf[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", err
	}

	sum := sha256.Sum256(buf[:n])
	return hex.EncodeToString(sum[:6]), nil
}

func savePstCheckpoint(st *store.Store, syncID int64, file, archiveID string, folderIndex int, folderPath string, msgIndex int64, cp *store.Checkpoint) error {
	b, err := json.Marshal(pstCheckpoint{
		File:        file,
		ArchiveID:   archiveID,
		FolderIndex: folderIndex,
		FolderPath:  folderPath,
		MsgIndex:    msgIndex,
	})
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	cp.PageToken = string(b)
	return st.UpdateSyncCheckpoint(syncID, cp)
}
