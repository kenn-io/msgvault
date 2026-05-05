package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wesm/msgvault/internal/mbox"
	"github.com/wesm/msgvault/internal/store"
)

type MboxImportOptions struct {
	// SourceType is the sources.source_type value (e.g. "mbox" or "hey").
	SourceType string

	// Identifier is the sources.identifier (e.g. "you@hey.com").
	Identifier string

	// Labels, if non-empty, are applied to all imported messages.
	Labels []string

	// NoResume forces a fresh import even if an active sync run exists for the source.
	NoResume bool

	// CheckpointInterval controls how often (in messages) to persist progress.
	// If zero, a default of 200 is used.
	CheckpointInterval int

	// AttachmentsDir controls where attachments are written.
	// If empty, attachments are not written to disk (but messages are still imported).
	AttachmentsDir string

	// MaxMessageBytes limits the maximum size of a single message read from the MBOX.
	// If zero, a default of 128 MiB is used.
	MaxMessageBytes int64

	// IngestFunc allows callers (tests) to override message ingestion. If nil,
	// the default ingestRawEmail is used.
	IngestFunc func(ctx context.Context, st *store.Store, sourceID int64, identifier string, attachmentsDir string, labelIDs []int64, sourceMsgID string, rawHash string, msg *mbox.Message, log *slog.Logger) error

	// Logger is optional; defaults to slog.Default().
	Logger *slog.Logger
}

type MboxImportSummary struct {
	SourceID       int64
	WasResumed     bool
	ResumedOffset  int64
	FinalOffset    int64
	Duration       time.Duration
	BytesProcessed int64

	MessagesProcessed int64
	MessagesAdded     int64
	MessagesUpdated   int64
	MessagesSkipped   int64
	LabelsUpdated     int64
	Errors            int64
	HardErrors        bool
}

type mboxCheckpoint struct {
	File   string `json:"file"`
	Offset int64  `json:"offset"`
	Seq    int64  `json:"seq,omitempty"`
}

const defaultMaxMboxMessageBytes int64 = 128 << 20 // 128 MiB

// ImportMbox imports a single MBOX file into the msgvault database.
//
// This is intended for services like HEY.com that provide an export in MBOX
// format but do not expose IMAP/POP. The importer stores the raw MIME,
// parsed bodies, participants, recipients, and (optionally) attachments.
func ImportMbox(ctx context.Context, st *store.Store, mboxPath string, opts MboxImportOptions) (*MboxImportSummary, error) {
	if opts.SourceType == "" {
		opts.SourceType = "mbox"
	}
	if opts.Identifier == "" {
		return nil, fmt.Errorf("identifier is required")
	}
	if opts.CheckpointInterval <= 0 {
		opts.CheckpointInterval = 200
	}
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = defaultMaxMboxMessageBytes
	}
	ingestFn := opts.IngestFunc
	if ingestFn == nil {
		ingestFn = ingestRawEmail
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	start := time.Now()
	summary := &MboxImportSummary{}

	absPath, err := filepath.Abs(mboxPath)
	if err != nil {
		return nil, fmt.Errorf("abs path: %w", err)
	}
	cpFile := absPath
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		cpFile = resolved
	}
	// Ensure / get the source.
	src, err := st.GetOrCreateSource(opts.SourceType, opts.Identifier)
	if err != nil {
		return nil, fmt.Errorf("get/create source: %w", err)
	}
	summary.SourceID = src.ID

	// Create or resume the sync run for this source.
	var (
		syncID int64
		cp     store.Checkpoint
		offset int64
		seq    int64
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
				var mcp mboxCheckpoint
				if err := json.Unmarshal([]byte(active.CursorBefore.String), &mcp); err == nil {
					sameFile := false
					if mcp.File == absPath || mcp.File == cpFile {
						sameFile = true
					} else if mcp.File != "" {
						if curInfo, err := os.Stat(absPath); err == nil {
							if cpInfo, err := os.Stat(mcp.File); err == nil && os.SameFile(curInfo, cpInfo) {
								sameFile = true
							}
						}
					}

					if sameFile && mcp.Offset > 0 {
						offset = mcp.Offset
						seq = mcp.Seq
						summary.WasResumed = true
						summary.ResumedOffset = offset
						log.Info("resuming mbox import", "file", absPath, "offset", offset, "processed", cp.MessagesProcessed)
					} else if mcp.File != "" && !sameFile {
						return nil, fmt.Errorf("active mbox import is for a different file (%q), not %q; rerun with --no-resume to start fresh", mcp.File, absPath)
					}
				}
			}
		}
	}

	if syncID == 0 {
		syncID, err = st.StartSync(src.ID, "import-mbox")
		if err != nil {
			return nil, fmt.Errorf("start sync: %w", err)
		}
	}

	failSync := func(msg string) {
		if fsErr := st.FailSync(syncID, msg); fsErr != nil {
			log.Warn("failed to record sync failure", "error", fsErr)
		}
	}

	// Save an initial checkpoint so the active sync always records which file it's importing,
	// even if the run is interrupted before the first periodic checkpoint.
	if err := saveMboxCheckpoint(st, syncID, cpFile, offset, seq, &cp); err != nil {
		cp.ErrorsCount++
		summary.Errors++
		log.Warn("failed to save initial checkpoint", "error", err)
	}

	// Ensure labels (once). Deduplicate to avoid PK violations.
	var labelIDs []int64
	seen := make(map[string]bool)
	for _, lbl := range opts.Labels {
		lbl = strings.TrimSpace(lbl)
		if lbl == "" || seen[lbl] {
			continue
		}
		seen[lbl] = true
		labelID, err := st.EnsureLabel(src.ID, lbl, lbl, "user")
		if err != nil {
			failSync(err.Error())
			return nil, fmt.Errorf("ensure label %q: %w", lbl, err)
		}
		labelIDs = append(labelIDs, labelID)
	}

	// Open file and (if resuming) seek.
	f, err := os.Open(absPath)
	if err != nil {
		failSync(err.Error())
		return nil, fmt.Errorf("open mbox: %w", err)
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		failSync(err.Error())
		return nil, fmt.Errorf("stat mbox: %w", err)
	}
	if offset > fi.Size() {
		resumeErr := fmt.Errorf("resume offset %d is beyond end of file (size %d) for %q; rerun with --no-resume to start fresh", offset, fi.Size(), absPath)
		failSync(resumeErr.Error())
		return nil, resumeErr
	}
	if offset > 0 && offset == fi.Size() {
		log.Info("resume offset at end of file; no work to do", "file", absPath, "offset", offset, "size", fi.Size())
	}

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			failSync(err.Error())
			return nil, fmt.Errorf("seek mbox: %w", err)
		}
	}

	if offset == 0 && cp.MessagesProcessed == 0 {
		if err := mbox.Validate(f, 8<<20); err != nil {
			failSync(err.Error())
			return summary, err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			failSync(err.Error())
			return nil, fmt.Errorf("seek mbox: %w", err)
		}
	}

	r := mbox.NewReaderWithMaxMessageBytes(f, opts.MaxMessageBytes)

	// If we resumed at a saved offset, the reader's logical Offset() should now be that.
	// However, if the offset lands in the middle of a line (corrupt checkpoint), the
	// reader will scan to the next separator.
	var lastCheckpointOffset = offset
	var lastCheckpointSeq = seq
	checkpointBlocked := false
	hardErrors := false

	type pendingMboxMessage struct {
		Msg        *mbox.Message
		RawHash    string
		SourceMsg  string
		Seq        int64
		NextOffset int64
	}

	const (
		existsCheckBatchSize  = 200
		existsCheckBatchBytes = 32 << 20 // 32 MiB
	)

	var pending []pendingMboxMessage
	var pendingBytes int64

	msgSeq := seq

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
				summary.FinalOffset = lastCheckpointOffset
				summary.Duration = time.Since(start)
				if err := saveMboxCheckpoint(st, syncID, cpFile, lastCheckpointOffset, lastCheckpointSeq, &cp); err != nil {
					cp.ErrorsCount++
					summary.Errors++
					log.Warn("failed to save checkpoint", "error", err)
				}
				return true, nil
			}

			cp.MessagesProcessed++
			summary.MessagesProcessed++
			summary.BytesProcessed += int64(len(p.Msg.Raw))

			exists := false
			if batchOK {
				_, exists = existingWithRaw[p.SourceMsg]
			} else {
				one, err := st.MessageExistsWithRawBatch(src.ID, []string{p.SourceMsg})
				if err != nil {
					cp.ErrorsCount++
					summary.Errors++
					log.Warn("existence check failed; attempting ingest anyway", "error", err)
				} else {
					_, exists = one[p.SourceMsg]
					// Merge into existingWithRaw so label update can find the message ID.
					if existingWithRaw == nil {
						existingWithRaw = make(map[string]int64)
					}
					for k, v := range one {
						existingWithRaw[k] = v
					}
				}
			}

			if exists {
				summary.MessagesSkipped++

				// Add labels to existing message (same pattern as emlx importer).
				if len(labelIDs) > 0 {
					if msgID, ok := existingWithRaw[p.SourceMsg]; ok && msgID > 0 {
						if err := st.AddMessageLabels(msgID, labelIDs); err != nil {
							log.Warn("failed to add labels to existing message",
								"source_message_id", p.SourceMsg, "error", err)
						} else {
							summary.LabelsUpdated++
						}
					}
				}

				// Update checkpoint offset even when skipping so resumption progresses.
				if !checkpointBlocked {
					lastCheckpointOffset = p.NextOffset
					lastCheckpointSeq = p.Seq
				}
				if cp.MessagesProcessed%int64(opts.CheckpointInterval) == 0 {
					if err := saveMboxCheckpoint(st, syncID, cpFile, lastCheckpointOffset, lastCheckpointSeq, &cp); err != nil {
						cp.ErrorsCount++
						summary.Errors++
						log.Warn("failed to save checkpoint", "error", err)
					}
				}
				continue
			}

			alreadyExists := false
			if anyOK {
				_, alreadyExists = existingAny[p.SourceMsg]
			} else {
				one, err := st.MessageExistsBatch(src.ID, []string{p.SourceMsg})
				if err != nil {
					cp.ErrorsCount++
					summary.Errors++
					log.Warn("existence check failed (any); counting as added", "error", err)
				} else {
					_, alreadyExists = one[p.SourceMsg]
				}
			}

			if err := ingestFn(ctx, st, src.ID, opts.Identifier, opts.AttachmentsDir, labelIDs, p.SourceMsg, p.RawHash, p.Msg, log); err != nil {
				cp.ErrorsCount++
				summary.Errors++
				log.Warn("failed to ingest message", "source_msg", p.SourceMsg, "next_offset", p.NextOffset, "error", err)
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
				lastCheckpointOffset = p.NextOffset
				lastCheckpointSeq = p.Seq
			}

			if cp.MessagesProcessed%int64(opts.CheckpointInterval) == 0 {
				if err := saveMboxCheckpoint(st, syncID, cpFile, lastCheckpointOffset, lastCheckpointSeq, &cp); err != nil {
					cp.ErrorsCount++
					summary.Errors++
					log.Warn("failed to save checkpoint", "error", err)
				}
			}
		}

		clear(pending)
		pending = pending[:0]
		pendingBytes = 0
		return false, nil
	}

	for {
		if err := ctx.Err(); err != nil {
			summary.FinalOffset = lastCheckpointOffset
			summary.Duration = time.Since(start)
			// Record best-effort checkpoint before returning.
			if err := saveMboxCheckpoint(st, syncID, cpFile, lastCheckpointOffset, lastCheckpointSeq, &cp); err != nil {
				cp.ErrorsCount++
				summary.Errors++
				log.Warn("failed to save checkpoint", "error", err)
			}
			return summary, nil
		}

		msg, err := r.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			nextOffset := r.NextFromOffset()
			if !checkpointBlocked {
				lastCheckpointOffset = nextOffset
			}
			cp.ErrorsCount++
			summary.Errors++
			log.Warn("mbox read error", "error", err, "next_offset", nextOffset)
			continue
		}

		// Compute stable IDs:
		// - rawHash: sha256(raw MIME), used for thread fallback and dedup
		// - sourceMsgID: based on stable message-level identity (rawHash + seq)
		//   so that re-importing the same mailbox after file changes (e.g. new
		//   export with appended messages) deduplicates unchanged messages.
		msgSeq++
		sum := sha256.Sum256(msg.Raw)
		rawHash := hex.EncodeToString(sum[:])
		nextOffset := r.NextFromOffset()
		sourceMsgID := fmt.Sprintf("mbox-%s-%d", rawHash, msgSeq)
		pending = append(pending, pendingMboxMessage{
			Msg:        msg,
			RawHash:    rawHash,
			SourceMsg:  sourceMsgID,
			Seq:        msgSeq,
			NextOffset: nextOffset,
		})
		pendingBytes += int64(len(msg.Raw))

		if len(pending) >= existsCheckBatchSize || pendingBytes >= existsCheckBatchBytes {
			stop, err := flushPending()
			if err != nil {
				return summary, err
			}
			if stop {
				return summary, nil
			}
		}
	}

	if stop, err := flushPending(); err != nil {
		return summary, err
	} else if stop {
		return summary, nil
	}

	finalOffset := r.Offset()
	if checkpointBlocked {
		finalOffset = lastCheckpointOffset
	}
	summary.FinalOffset = finalOffset
	summary.Duration = time.Since(start)

	// Final checkpoint and mark sync complete.
	if err := saveMboxCheckpoint(st, syncID, cpFile, finalOffset, lastCheckpointSeq, &cp); err != nil {
		cp.ErrorsCount++
		summary.Errors++
		log.Warn("failed to save checkpoint", "error", err)
	}

	summary.HardErrors = hardErrors
	if hardErrors {
		if err := st.FailSync(syncID, fmt.Sprintf("completed with %d errors", cp.ErrorsCount)); err != nil {
			return summary, fmt.Errorf("fail sync: %w", err)
		}
		return summary, nil
	}

	finalMsg := fmt.Sprintf("offset:%d", r.Offset())
	if cp.ErrorsCount > 0 {
		finalMsg = fmt.Sprintf("offset:%d errors:%d", r.Offset(), cp.ErrorsCount)
	}
	if err := st.CompleteSync(syncID, finalMsg); err != nil {
		return summary, fmt.Errorf("complete sync: %w", err)
	}

	return summary, nil
}

func saveMboxCheckpoint(st *store.Store, syncID int64, file string, offset int64, seq int64, cp *store.Checkpoint) error {
	b, err := json.Marshal(mboxCheckpoint{File: file, Offset: offset, Seq: seq})
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	cp.PageToken = string(b)
	return st.UpdateSyncCheckpoint(syncID, cp)
}

func parseFromLineDate(fromLine string) (time.Time, bool) {
	if t, ok := mbox.ParseFromSeparatorDateStrict(fromLine); ok {
		return t.UTC(), true
	}
	return time.Time{}, false
}

func ingestRawEmail(
	ctx context.Context, st *store.Store,
	sourceID int64, identifier, attachmentsDir string,
	labelIDs []int64, sourceMsgID, rawHash string,
	msg *mbox.Message, log *slog.Logger,
) error {
	var fallbackDate time.Time
	if t, ok := parseFromLineDate(msg.FromLine); ok {
		fallbackDate = t
	}
	return IngestRawMessage(
		ctx, st, sourceID, identifier, attachmentsDir,
		labelIDs, sourceMsgID, rawHash,
		msg.Raw, fallbackDate, log,
	)
}
