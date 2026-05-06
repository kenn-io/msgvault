package fbmessenger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wesm/msgvault/internal/mime"
	"github.com/wesm/msgvault/internal/store"
)

// errLimitReached signals that ImportOptions.Limit tripped mid-thread so
// the caller should stop importing and not advance the per-thread
// checkpoint past a partially-imported thread.
var errLimitReached = errors.New("fbmessenger: import limit reached")

// fbmessengerCheckpoint is the JSON payload stored in
// sync_runs.cursor_before that records progress through a DYI import so
// a subsequent run can skip already-processed threads. RootDir is
// recorded to guard against a resume attempt against a different export
// directory; ThreadIndex and LastMessageIndex describe the most
// recent committed position.
type fbmessengerCheckpoint struct {
	RootDir          string `json:"root_dir"`
	ThreadIndex      int    `json:"thread_index"`
	LastMessageIndex int    `json:"last_message_index"`
}

// ImportOptions configures ImportDYI.
type ImportOptions struct {
	// Me is the importer's own identifier, required to be of the form
	// "<slug>@facebook.messenger". It is stored as the source identifier
	// and used to compute is_from_me.
	Me string
	// RootDir is the DYI export root directory.
	RootDir string
	// Format overrides auto-detection. One of "auto", "json", "html",
	// "both". Empty is treated as "auto".
	Format string
	// AttachmentsDir is the content-addressed attachment storage root.
	// When empty, attachments are not copied to disk but rows are still
	// written with empty storage_path.
	AttachmentsDir string
	// Limit caps the number of messages imported across the whole DYI
	// tree. 0 means no limit.
	Limit int
	// NoResume disables checkpoint-based resume.
	NoResume bool
	// CheckpointEvery is the number of messages between checkpoints.
	// Defaults to 200 when <= 0.
	CheckpointEvery int
	// Logger is the slog logger to use; a discard logger is used when nil.
	Logger *slog.Logger
}

// ImportSummary describes the outcome of a run.
type ImportSummary struct {
	Duration         time.Duration
	ThreadsProcessed int
	// ThreadsSkipped counts whole-thread skips for unparseable
	// message JSON/HTML files that caused the thread to be dropped.
	ThreadsSkipped int64
	// FilesSkipped counts unrecognized sibling files (e.g.
	// message_final.json) that were skipped without aborting the
	// surrounding thread.
	FilesSkipped         int64
	MessagesProcessed    int64
	MessagesAdded        int64
	MessagesSkipped      int64
	ParticipantsResolved int
	AttachmentsFound     int
	AttachmentsStored    int
	ReactionsAdded       int
	FromMeCount          int64
	Errors               int
	HardErrors           bool
	WasResumed           bool
}

// ImportDYI imports a Facebook Messenger DYI export into the store.
func ImportDYI(ctx context.Context, st *store.Store, opts ImportOptions) (*ImportSummary, error) {
	start := time.Now()
	summary := &ImportSummary{}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.CheckpointEvery <= 0 {
		opts.CheckpointEvery = 200
	}
	format := strings.ToLower(opts.Format)
	if format == "" {
		format = "auto"
	}
	switch format {
	case "auto", "json", "html", "both":
		// valid
	default:
		return nil, fmt.Errorf("fbmessenger: unknown --format %q (valid: auto, json, html, both)", format)
	}

	// Validate --me.
	if opts.Me == "" {
		return nil, fmt.Errorf("fbmessenger: --me is required")
	}
	if !strings.HasSuffix(opts.Me, "@"+Domain) {
		return nil, fmt.Errorf("fbmessenger: --me must be a <slug>@%s address, got %q", Domain, opts.Me)
	}

	// Check root dir exists early.
	if info, err := os.Stat(opts.RootDir); err != nil {
		return nil, fmt.Errorf("fbmessenger: root dir: %w", err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("fbmessenger: root is not a directory: %s", opts.RootDir)
	}

	// Resolve absolute root so the checkpoint is comparable across
	// relative invocations.
	absRoot, err := filepath.Abs(opts.RootDir)
	if err != nil {
		return nil, fmt.Errorf("fbmessenger: abs root: %w", err)
	}

	// Create source and start sync run.
	source, err := st.GetOrCreateSource("facebook_messenger", opts.Me)
	if err != nil {
		return nil, fmt.Errorf("fbmessenger: source: %w", err)
	}

	// Read any existing active-run checkpoint before calling
	// StartSync (which marks active runs failed). This mirrors the
	// pattern used by emlx_import / mbox_import.
	var (
		startThreadIdx int
		cp             store.Checkpoint
	)
	if !opts.NoResume {
		// Look for a resumable sync run. Try active (running) first,
		// then fall back to the latest checkpointed run (which includes
		// failed/interrupted runs whose checkpoint is still valid).
		prev, err := st.GetActiveSync(source.ID)
		if err != nil {
			return nil, fmt.Errorf("fbmessenger: check active sync: %w", err)
		}
		if prev == nil || !prev.CursorBefore.Valid || prev.CursorBefore.String == "" {
			prev, err = st.GetLatestCheckpointedSync(source.ID)
			if err != nil {
				return nil, fmt.Errorf("fbmessenger: check checkpointed sync: %w", err)
			}
		}
		if prev != nil && prev.CursorBefore.Valid && prev.CursorBefore.String != "" {
			var prior fbmessengerCheckpoint
			if err := json.Unmarshal([]byte(prev.CursorBefore.String), &prior); err == nil {
				if prior.RootDir != "" && prior.RootDir != absRoot {
					return nil, fmt.Errorf(
						"fbmessenger: active import is for a different root (%q), not %q; rerun with --no-resume to start fresh",
						prior.RootDir, absRoot,
					)
				}
				// Treat any well-formed checkpoint as resumable. A
				// checkpoint saved mid-way through the first thread
				// has ThreadIndex == 0; the outer loop still starts
				// at threadIdx=0 either way, but resuming lets us
				// carry cumulative counters forward and emit the
				// "resuming" log so a user-visible interrupt during
				// thread 0 is reflected in the summary.
				startThreadIdx = prior.ThreadIndex
				summary.WasResumed = true
				cp.MessagesProcessed = prev.MessagesProcessed
				cp.MessagesAdded = prev.MessagesAdded
				cp.MessagesUpdated = prev.MessagesUpdated
				cp.ErrorsCount = prev.ErrorsCount
				summary.MessagesProcessed = prev.MessagesProcessed
				summary.MessagesAdded = prev.MessagesAdded
				logger.Info("fbmessenger: resuming import",
					"root", absRoot,
					"thread_index", startThreadIdx,
					"processed", cp.MessagesProcessed,
				)
			}
		}
	}

	syncID, err := st.StartSync(source.ID, "import-messenger")
	if err != nil {
		return nil, fmt.Errorf("fbmessenger: start sync: %w", err)
	}
	var syncErr error
	defer func() {
		if syncErr != nil {
			_ = st.FailSync(syncID, syncErr.Error())
		} else {
			_ = st.CompleteSync(syncID, "")
		}
	}()

	// Pre-create label taxonomy.
	labelIDs := make(map[string]int64)
	parentLabelID, err := st.EnsureLabel(source.ID, "messenger", "Messenger", "folder")
	if err != nil {
		syncErr = err
		return nil, fmt.Errorf("fbmessenger: ensure parent label: %w", err)
	}
	labelIDs["Messenger"] = parentLabelID
	for _, pair := range sectionLabelNames() {
		lid, err := st.EnsureLabel(source.ID, pair.id, pair.name, "folder")
		if err != nil {
			syncErr = err
			return nil, fmt.Errorf("fbmessenger: ensure label %s: %w", pair.name, err)
		}
		labelIDs[pair.name] = lid
	}

	// Seed the self participant so even an empty import leaves a trace.
	meAddr := mime.Address{Name: "", Email: opts.Me, Domain: Domain}
	if _, err := st.EnsureParticipantsBatch([]mime.Address{meAddr}); err != nil {
		syncErr = err
		return nil, fmt.Errorf("fbmessenger: seed self participant: %w", err)
	}
	meLocal := StripDomain(opts.Me)

	// Discover threads.
	threads, err := Discover(absRoot)
	if err != nil {
		syncErr = err
		return nil, fmt.Errorf("fbmessenger: discover: %w", err)
	}

	// Guard against a checkpoint that points past the current list
	// (e.g. export shrunk); fall through to a full scan in that
	// case since the source_message_id upsert will dedupe.
	if startThreadIdx > len(threads) {
		logger.Warn("fbmessenger: checkpoint thread_index out of range; restarting full scan",
			"checkpoint", startThreadIdx, "threads", len(threads))
		startThreadIdx = 0
		summary.WasResumed = false
	}

	for threadIdx, td := range threads {
		if threadIdx < startThreadIdx {
			continue
		}
		if ctx.Err() != nil {
			syncErr = ctx.Err()
			return summary, ctx.Err()
		}
		if opts.Limit > 0 && summary.MessagesAdded >= int64(opts.Limit) {
			break
		}
		effective := td.Format
		// E2EE threads bypass the format filter entirely.
		if effective != "e2ee_json" {
			switch format {
			case "json":
				if effective == "html" {
					continue
				}
				effective = "json"
			case "html":
				if effective == "json" {
					continue
				}
				effective = "html"
			case "both":
				// Keep as-is; "both" threads get both parsed.
			case "auto":
				if effective == "both" {
					effective = "json"
				}
			}
		}

		summary.ThreadsProcessed++
		err := importThread(ctx, st, source.ID, td, effective, format, opts, labelIDs, meLocal, logger, summary, syncID, absRoot, threadIdx, &cp)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				syncErr = err
				return summary, err
			}
			if errors.Is(err, errLimitReached) {
				// Limit tripped mid-thread: do not advance the
				// checkpoint past this thread, so a subsequent
				// non-limited run re-scans it and picks up the
				// remaining messages via source_message_id dedup.
				break
			}
			summary.Errors++
			summary.HardErrors = true
			logger.Warn("fbmessenger: thread failed", "thread", td.Name, "err", err)
			// Don't advance the checkpoint on a hard thread error.
			// source_message_id dedup makes retry safe on the next
			// run, so a transient failure (DB lock, I/O blip) self-
			// heals. If the error is persistent the user will see
			// HardErrors=true and can rerun with --no-resume.
			continue
		}
		// Persist per-thread checkpoint so resume can skip fully
		// committed threads. Advance ThreadIndex to threadIdx+1
		// because this thread is now fully processed.
		if !opts.NoResume {
			_ = saveFbmessengerCheckpoint(st, syncID, absRoot, threadIdx+1, 0, &cp, summary)
		}
	}

	if err := st.RecomputeConversationStats(source.ID); err != nil {
		logger.Warn("fbmessenger: recompute stats", "err", err)
	}
	summary.Duration = time.Since(start)
	return summary, nil
}

// sectionLabelNames returns the section label name mapping.
type sectionLabel struct {
	section, id, name string
}

func sectionLabelNames() []sectionLabel {
	return []sectionLabel{
		{"inbox", "messenger:inbox", "Messenger / Inbox"},
		{"archived_threads", "messenger:archived", "Messenger / Archived"},
		{"filtered_threads", "messenger:filtered", "Messenger / Filtered"},
		{"message_requests", "messenger:requests", "Messenger / Requests"},
		{"e2ee_cutover", "messenger:e2ee", "Messenger / E2EE"},
	}
}

func sectionLabelName(section string) string {
	for _, p := range sectionLabelNames() {
		if p.section == section {
			return p.name
		}
	}
	return "Messenger / Other"
}

// importThread imports one ThreadDir per effective format.
func importThread(
	ctx context.Context,
	st *store.Store,
	sourceID int64,
	td ThreadDir,
	effective, requested string,
	opts ImportOptions,
	labelIDs map[string]int64,
	meLocal string,
	logger *slog.Logger,
	summary *ImportSummary,
	syncID int64,
	absRoot string,
	threadIdx int,
	cp *store.Checkpoint,
) error {
	// Decide which parsers to run.
	type parsedPair struct {
		thread *Thread
		prefix string // "" for json, "html_" for html under "both"
	}
	var toImport []parsedPair

	runJSON := func(prefix string) error {
		th, err := ParseJSONThread(opts.RootDir, td.Path)
		if err != nil {
			if errors.Is(err, ErrCorruptJSON) {
				summary.ThreadsSkipped++
				logger.Warn("fbmessenger: corrupt json, skipping", "thread", td.Name, "err", err)
				return nil
			}
			return err
		}
		if len(th.BadSiblings) > 0 {
			summary.FilesSkipped += int64(len(th.BadSiblings))
			logger.Warn("fbmessenger: skipping unrecognized sibling files",
				"thread", td.Name, "files", th.BadSiblings)
		}
		th.Section = td.Section
		toImport = append(toImport, parsedPair{thread: th, prefix: prefix})
		return nil
	}
	runHTML := func(prefix string) error {
		th, err := ParseHTMLThread(opts.RootDir, td.Path)
		if err != nil {
			return err
		}
		th.Section = td.Section
		toImport = append(toImport, parsedPair{thread: th, prefix: prefix})
		return nil
	}

	runE2EE := func() error {
		th, err := ParseE2EEJSONFile(opts.RootDir, td.FilePath)
		if err != nil {
			if errors.Is(err, ErrCorruptJSON) {
				summary.ThreadsSkipped++
				logger.Warn("fbmessenger: corrupt e2ee json, skipping", "thread", td.Name, "err", err)
				return nil
			}
			if errors.Is(err, ErrNotE2EEThread) {
				// Not a thread (e.g. a DYI metadata file Facebook
				// added that isn't in the allowlist). Silently skip.
				return nil
			}
			return err
		}
		th.Section = td.Section
		toImport = append(toImport, parsedPair{thread: th, prefix: ""})
		return nil
	}

	switch {
	case td.Format == "e2ee_json":
		if err := runE2EE(); err != nil {
			return err
		}
	case requested == "both" && td.Format == "both":
		if err := runJSON(""); err != nil {
			return err
		}
		if err := runHTML("html_"); err != nil {
			return err
		}
	case effective == "json":
		if err := runJSON(""); err != nil {
			return err
		}
	case effective == "html":
		if err := runHTML(""); err != nil {
			return err
		}
	}

	for _, pair := range toImport {
		if err := writeThreadToStore(ctx, st, sourceID, td, pair.thread, pair.prefix, opts, labelIDs, meLocal, logger, summary, syncID, absRoot, threadIdx, cp); err != nil {
			return err
		}
	}
	return nil
}

func writeThreadToStore(
	ctx context.Context,
	st *store.Store,
	sourceID int64,
	td ThreadDir,
	thread *Thread,
	prefix string,
	opts ImportOptions,
	labelIDs map[string]int64,
	meLocal string,
	logger *slog.Logger,
	summary *ImportSummary,
	syncID int64,
	absRoot string,
	threadIdx int,
	cp *store.Checkpoint,
) error {
	// Ensure conversation. Use section-qualified name so threads with
	// the same basename in different sections (e.g. inbox vs archived)
	// don't collide.
	threadKey := td.Section + "/" + td.Name
	convID, err := st.EnsureConversationWithType(sourceID, threadKey, thread.ConvType, thread.Title)
	if err != nil {
		return fmt.Errorf("ensure conversation: %w", err)
	}

	// Build participant address map for this thread.
	addrs := make([]mime.Address, 0, len(thread.Participants)+1)
	addrs = append(addrs, mime.Address{Name: "", Email: opts.Me, Domain: Domain})
	for _, p := range thread.Participants {
		addrs = append(addrs, Address(p.Name))
	}
	partIDs, err := st.EnsureParticipantsBatch(addrs)
	if err != nil {
		return fmt.Errorf("ensure participants: %w", err)
	}
	summary.ParticipantsResolved += len(partIDs)

	// Map display name → participant ID and email for this thread.
	nameToID := make(map[string]int64)
	nameToEmail := make(map[string]string)
	for _, p := range thread.Participants {
		addr := Address(p.Name)
		nameToID[p.Name] = partIDs[addr.Email]
		nameToEmail[p.Name] = addr.Email
	}
	meID := partIDs[opts.Me]
	// Ensure all participants (incl. self) are linked to the conversation.
	_ = st.EnsureConversationParticipant(convID, meID, "member")
	for _, pid := range nameToID {
		_ = st.EnsureConversationParticipant(convID, pid, "member")
	}

	// Determine label IDs for this thread.
	parentLabelID := labelIDs["Messenger"]
	sectionLabelID := labelIDs[sectionLabelName(td.Section)]

	// Raw bytes format tag.
	rawFormat := "fbmessenger_" + thread.Format

	// Iterate messages.
	for mi, m := range thread.Messages {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if opts.Limit > 0 && summary.MessagesAdded >= int64(opts.Limit) {
			return errLimitReached
		}
		summary.MessagesProcessed++

		// Resolve sender.
		senderName := m.SenderName
		senderID := sql.NullInt64{}
		isFromMe := false
		if senderName != "" {
			if id, ok := nameToID[senderName]; ok {
				senderID = sql.NullInt64{Int64: id, Valid: true}
			} else {
				// Participant not in the thread's participants list;
				// synthesize.
				addr := Address(senderName)
				m2, err := st.EnsureParticipantsBatch([]mime.Address{addr})
				if err == nil {
					if id, ok := m2[addr.Email]; ok {
						senderID = sql.NullInt64{Int64: id, Valid: true}
						nameToID[senderName] = id
						nameToEmail[senderName] = addr.Email
						_ = st.EnsureConversationParticipant(convID, id, "member")
					}
				}
			}
		}
		if senderName != "" {
			if Slug(senderName) == meLocal {
				isFromMe = true
				senderID = sql.NullInt64{Int64: meID, Valid: true}
				summary.FromMeCount++
			}
		}

		// Build the source_message_id. Section-qualified to avoid
		// collisions across sections with the same thread basename.
		srcMsgID := fmt.Sprintf("%s__%s%d", threadKey, prefix, m.Index)

		// If we couldn't resolve a sender this run but a previous import
		// of the same message already recorded one, preserve it — the
		// UpsertMessage UPDATE path unconditionally overwrites sender_id
		// with the incoming value, which would nullify valid prior data.
		// Also rehydrate the sender's display name and email so
		// downstream writes to message_recipients("from") and FTS don't
		// clobber the prior display-name / from-address with empty
		// strings derived from an unresolved current-run senderName.
		if !senderID.Valid {
			var priorID sql.NullInt64
			var priorName, priorEmail sql.NullString
			var priorIsFromMe bool
			// COALESCE onto message_recipients.display_name because the
			// seeded self (--me) participant is created with an empty
			// display_name; falling back to what we wrote on the prior
			// import preserves the sender label for self-authored rows.
			err := st.DB().QueryRow(
				`SELECT m.sender_id, m.is_from_me,
					COALESCE(NULLIF(p.display_name, ''),
						(SELECT mr.display_name FROM message_recipients mr
						 WHERE mr.message_id = m.id AND mr.recipient_type = 'from'
						 LIMIT 1)),
					p.email_address
				 FROM messages m
				 LEFT JOIN participants p ON p.id = m.sender_id
				 WHERE m.source_id = ? AND m.source_message_id = ?`,
				sourceID, srcMsgID,
			).Scan(&priorID, &priorIsFromMe, &priorName, &priorEmail)
			if err == nil && priorID.Valid {
				senderID = priorID
				isFromMe = priorIsFromMe
				if priorIsFromMe {
					summary.FromMeCount++
				}
				if priorName.Valid && priorName.String != "" {
					senderName = priorName.String
					nameToID[senderName] = priorID.Int64
					if priorEmail.Valid {
						nameToEmail[senderName] = priorEmail.String
					}
				}
				// Re-link the rehydrated participant into the
				// conversation. Older DBs affected by the
				// pre-fix synthesized-sender bug have a valid
				// messages.sender_id but no matching
				// conversation_participants row; this repairs
				// them on re-import.
				_ = st.EnsureConversationParticipant(convID, priorID.Int64, "member")
			}
		}

		snippet := buildSnippet(m.Body)
		msgRow := &store.Message{
			ConversationID:  convID,
			SourceID:        sourceID,
			SourceMessageID: srcMsgID,
			MessageType:     "fbmessenger",
			SentAt:          sql.NullTime{Time: m.SentAt, Valid: !m.SentAt.IsZero()},
			ReceivedAt:      sql.NullTime{Time: m.SentAt, Valid: !m.SentAt.IsZero()},
			SenderID:        senderID,
			IsFromMe:        isFromMe,
			Snippet:         sql.NullString{String: snippet, Valid: snippet != ""},
			SizeEstimate:    int64(len(m.Body)),
			HasAttachments:  len(m.Attachments) > 0,
			AttachmentCount: len(m.Attachments),
		}
		messageID, err := st.UpsertMessage(msgRow)
		if err != nil {
			summary.Errors++
			summary.MessagesSkipped++
			logger.Warn("fbmessenger: upsert message", "err", err)
			continue
		}
		summary.MessagesAdded++

		// Body.
		bodyText := sql.NullString{String: m.Body, Valid: m.Body != ""}
		if err := st.UpsertMessageBody(messageID, bodyText, sql.NullString{}); err != nil {
			summary.Errors++
			logger.Warn("fbmessenger: upsert body", "err", err)
		}

		// Raw bytes — store once per thread (first message only) to avoid bloat.
		if mi == 0 && len(thread.RawBytes) > 0 {
			if err := st.UpsertMessageRawWithFormat(messageID, thread.RawBytes, rawFormat); err != nil {
				logger.Warn("fbmessenger: upsert raw", "err", err)
			}
		}

		// Recipients: from = sender, to = other participants.
		// Only rewrite "from" when we have a valid sender — otherwise on
		// re-import we would clobber a previously-recorded sender with an
		// empty row.
		if senderID.Valid {
			fromIDs := []int64{senderID.Int64}
			fromNames := []string{senderName}
			if err := st.ReplaceMessageRecipients(messageID, "from", fromIDs, fromNames); err != nil {
				logger.Warn("fbmessenger: replace from recipients", "err", err)
			}
		}

		var toIDs []int64
		var toNames []string
		seenPID := make(map[int64]bool)
		if senderID.Valid {
			seenPID[senderID.Int64] = true
		}
		sortedNames := make([]string, 0, len(nameToID))
		for name := range nameToID {
			sortedNames = append(sortedNames, name)
		}
		sort.Strings(sortedNames)
		for _, name := range sortedNames {
			pid := nameToID[name]
			if seenPID[pid] {
				continue
			}
			seenPID[pid] = true
			toIDs = append(toIDs, pid)
			toNames = append(toNames, name)
		}
		// If message is from someone else, ensure self is in "to".
		if !isFromMe && !seenPID[meID] {
			toIDs = append(toIDs, meID)
			toNames = append(toNames, "")
			seenPID[meID] = true
		}
		_ = st.ReplaceMessageRecipients(messageID, "to", toIDs, toNames)

		// Labels.
		if parentLabelID != 0 {
			_ = st.LinkMessageLabel(messageID, parentLabelID)
		}
		if sectionLabelID != 0 {
			_ = st.LinkMessageLabel(messageID, sectionLabelID)
		}

		// Attachments.
		for _, att := range m.Attachments {
			summary.AttachmentsFound++
			storagePath, contentHash, size := handleAttachment(att, opts.AttachmentsDir)
			if storagePath != "" {
				summary.AttachmentsStored++
			}
			if storagePath != "" || contentHash != "" || att.AbsPath != "" {
				if err := st.UpsertAttachment(messageID, att.Filename, att.MimeType, storagePath, contentHash, size); err != nil {
					logger.Warn("fbmessenger: upsert attachment", "err", err)
				}
			} else {
				// Empty row so the user sees a trace that something was referenced.
				if err := st.UpsertAttachment(messageID, att.Filename, att.MimeType, "", "", 0); err != nil {
					logger.Warn("fbmessenger: upsert attachment (empty)", "err", err)
				}
			}
		}

		// Reactions: first-class rows and body-append already done.
		for _, r := range m.Reactions {
			actorAddr := Address(r.Actor)
			m2, err := st.EnsureParticipantsBatch([]mime.Address{actorAddr})
			if err != nil {
				continue
			}
			pid := m2[actorAddr.Email]
			if pid == 0 {
				continue
			}
			if err := st.UpsertReaction(messageID, pid, "emoji", r.Reaction, m.SentAt); err == nil {
				summary.ReactionsAdded++
			}
		}

		// FTS indexing.
		fromAddr := ""
		if senderID.Valid {
			fromAddr = nameToEmail[senderName]
			if isFromMe {
				fromAddr = opts.Me
			}
		}
		toAddr := strings.Join(nameToEmailsList(nameToEmail, opts.Me, senderName), " ")
		if err := st.UpsertFTS(messageID, thread.Title, m.Body, fromAddr, toAddr, ""); err != nil {
			logger.Warn("fbmessenger: upsert fts", "err", err)
		}

		// Checkpoint every N messages within a long thread. We save
		// the current thread index (not threadIdx+1) because this
		// thread is still in progress; we also record the last
		// message index so an observer can see progress, though
		// resume skips at thread granularity only.
		if !opts.NoResume && summary.MessagesAdded > 0 && summary.MessagesAdded%int64(opts.CheckpointEvery) == 0 {
			_ = saveFbmessengerCheckpoint(st, syncID, absRoot, threadIdx, m.Index, cp, summary)
		}
	}
	return nil
}

// saveFbmessengerCheckpoint marshals a fbmessengerCheckpoint JSON blob
// into sync_runs.cursor_before along with the counter fields. Errors
// are returned to the caller but ImportDYI logs rather than aborts.
func saveFbmessengerCheckpoint(
	st *store.Store, syncID int64,
	absRoot string, threadIdx int, lastMsgIdx int,
	cp *store.Checkpoint, summary *ImportSummary,
) error {
	b, err := json.Marshal(fbmessengerCheckpoint{
		RootDir:          absRoot,
		ThreadIndex:      threadIdx,
		LastMessageIndex: lastMsgIdx,
	})
	if err != nil {
		return fmt.Errorf("marshal fbmessenger checkpoint: %w", err)
	}
	cp.PageToken = string(b)
	cp.MessagesProcessed = summary.MessagesProcessed
	cp.MessagesAdded = summary.MessagesAdded
	cp.ErrorsCount = int64(summary.Errors)
	return st.UpdateSyncCheckpoint(syncID, cp)
}

func nameToEmailsList(m map[string]string, me, skipName string) []string {
	out := make([]string, 0, len(m)+1)
	for name, email := range m {
		if name == skipName {
			continue
		}
		out = append(out, email)
	}
	out = append(out, me)
	return out
}

func buildSnippet(body string) string {
	s := strings.TrimSpace(body)
	if utf8.RuneCountInString(s) > 200 {
		s = string([]rune(s)[:200])
	}
	return s
}

// handleAttachment copies an attachment file into content-addressed
// storage and returns (storagePath, contentHash, size). All zero values
// when the file is missing, unreadable, or no AttachmentsDir is configured.
// Symlinks and non-regular files (devices, sockets, named pipes) are
// rejected: a malicious DYI export could plant a symlink pointing at a
// sensitive local file (e.g. ~/.ssh/id_rsa) and we would otherwise copy
// the target into the attachment store.
func handleAttachment(att Attachment, attachmentsDir string) (string, string, int) {
	if attachmentsDir == "" || att.AbsPath == "" {
		return "", "", 0
	}
	linfo, err := os.Lstat(att.AbsPath)
	if err != nil {
		return "", "", 0
	}
	if !linfo.Mode().IsRegular() {
		return "", "", 0
	}
	f, err := os.Open(att.AbsPath)
	if err != nil {
		return "", "", 0
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return "", "", 0
	}
	if !info.Mode().IsRegular() {
		return "", "", 0
	}

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", "", 0
	}
	contentHash := fmt.Sprintf("%x", h.Sum(nil))
	rel := filepath.Join(contentHash[:2], contentHash)
	absStorage := filepath.Join(attachmentsDir, rel)

	if _, err := os.Stat(absStorage); err == nil {
		return rel, contentHash, int(info.Size())
	}
	if err := os.MkdirAll(filepath.Dir(absStorage), 0750); err != nil {
		return "", contentHash, 0
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", contentHash, 0
	}
	dst, err := os.OpenFile(absStorage, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			return rel, contentHash, int(info.Size())
		}
		return "", contentHash, 0
	}
	if _, err := io.Copy(dst, f); err != nil {
		_ = dst.Close()
		_ = os.Remove(absStorage)
		return "", contentHash, 0
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(absStorage)
		return "", contentHash, 0
	}
	return rel, contentHash, int(info.Size())
}
