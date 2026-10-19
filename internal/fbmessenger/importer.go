package fbmessenger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
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
	// NotifyImports opts this logical import into the independently enabled Events family.
	NotifyImports bool
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
	ImportRunID          string
	WasResumed           bool
}

// ImportDYI imports a Facebook Messenger DYI export into the store.
func ImportDYI(ctx context.Context, st *store.Store, opts ImportOptions) (_ *ImportSummary, retErr error) {
	st = st.WithIngestContext(store.IngestContext{Mode: store.IngestBackfill})
	start := time.Now()
	summary := &ImportSummary{}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if opts.CheckpointEvery <= 0 {
		opts.CheckpointEvery = 200
	}
	format := strings.ToLower(opts.Format)
	if format == "" {
		format = formatAuto
	}
	switch format {
	case formatAuto, formatJSON, "html", "both":
		// valid
	default:
		return nil, fmt.Errorf("fbmessenger: unknown --format %q (valid: auto, json, html, both)", format)
	}

	// Validate --me.
	if opts.Me == "" {
		return nil, errors.New("fbmessenger: --me is required")
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

	if err := st.ValidateNativeImportChoice(ctx, "facebook_messenger", opts.NotifyImports); err != nil {
		return nil, err
	}

	// Create source and start sync run.
	source, err := st.GetOrCreateSource("facebook_messenger", opts.Me)
	if err != nil {
		return nil, fmt.Errorf("fbmessenger: source: %w", err)
	}

	// Resume only a checkpoint whose worker has stopped. StartSync rejects a
	// running sync, so a live worker cannot be replaced by this import.
	var (
		startThreadIdx int
		hasCheckpoint  bool
		cp             store.Checkpoint
	)
	if !opts.NoResume {
		prev, err := st.GetLatestCheckpointedSyncByType(source.ID, "import-messenger")
		if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
			return nil, fmt.Errorf("fbmessenger: check checkpointed sync: %w", err)
		}
		if prev != nil && prev.CursorBefore.Valid && prev.CursorBefore.String != "" {
			hasCheckpoint = true
			var prior fbmessengerCheckpoint
			if err := json.Unmarshal([]byte(prev.CursorBefore.String), &prior); err == nil {
				if prior.RootDir != "" && prior.RootDir != absRoot {
					return nil, fmt.Errorf(
						"fbmessenger: checkpointed import is for a different root (%q), not %q; rerun with --no-resume to start fresh",
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

	ownershipCtx := context.WithoutCancel(ctx)
	execution, err := st.AcquireSyncExecutionContext(ownershipCtx, source.ID)
	if err != nil {
		return nil, fmt.Errorf("fbmessenger: acquire source execution: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	requestHash := sha256.Sum256([]byte("facebook_messenger\x00" + absRoot + "\x00" + format + "\x00" + opts.AttachmentsDir))
	requestFingerprint := hex.EncodeToString(requestHash[:])
	var recoveredSyncID int64
	if !opts.NoResume {
		recoveredSyncID, err = st.FindResumableImportAttempt(ownershipCtx, source.ID, "facebook_messenger", requestFingerprint)
		if err != nil {
			return nil, fmt.Errorf("fbmessenger: find resumable import: %w", err)
		}
	}

	if opts.NotifyImports && hasCheckpoint && recoveredSyncID == 0 {
		return nil, errors.New("legacy Messenger checkpoint is silent; use --no-resume to start a new notifying run")
	}
	syncID, err := execution.StartSyncWithRequestContext(ownershipCtx, "import-messenger", "", requestFingerprint)
	if err != nil {
		return nil, fmt.Errorf("fbmessenger: start sync: %w", err)
	}
	st = st.ScopedToSync(source.ID, syncID)
	var syncErr error
	defer func() {
		if syncErr != nil {
			_ = st.FailSync(syncID, syncErr.Error())
		} else {
			_ = st.CompleteSync(syncID, "")
		}
	}()

	logicalRun, err := st.BeginNativeImportAttempt(ownershipCtx, source.ID, "facebook_messenger", opts.NotifyImports, syncID, recoveredSyncID)
	if err != nil {
		syncErr = err
		return nil, fmt.Errorf("fbmessenger: begin import: %w", err)
	}
	summary.ImportRunID = logicalRun.ID
	importStore, err := st.WithImportRun(ownershipCtx, logicalRun.ID)
	if err != nil {
		syncErr = err
		return nil, fmt.Errorf("fbmessenger: bind import: %w", err)
	}
	if importStore == nil {
		err = errors.New("WithImportRun returned a nil Store")
		syncErr = err
		return nil, fmt.Errorf("fbmessenger: bind import: %w", err)
	}
	st = importStore
	if recoveredSyncID != 0 {
		// Native message IDs make a full recovery scan idempotent. A durable
		// logical run, rather than best-effort progress, owns notification choice.
		startThreadIdx = 0
		summary.WasResumed = true
		summary.MessagesProcessed = 0
		summary.MessagesAdded = 0
	}

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

	meLocal := StripDomain(opts.Me)

	// Discover threads.
	threads, err := Discover(absRoot)
	if err != nil {
		syncErr = err
		return nil, fmt.Errorf("fbmessenger: discover: %w", err)
	}

	// Preserve the native empty-import owner trace. Nonempty imports resolve
	// all participant identities in the canonical message transaction.
	if len(threads) == 0 {
		if _, err := st.EnsureParticipantsBatch([]mime.Address{{Email: opts.Me, Domain: Domain}}); err != nil {
			syncErr = err
			return nil, fmt.Errorf("fbmessenger: seed empty import owner: %w", err)
		}
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
			case formatJSON:
				if effective == "html" {
					continue
				}
				effective = formatJSON
			case "html":
				if effective == formatJSON {
					continue
				}
				effective = "html"
			case "both":
				// Keep as-is; "both" threads get both parsed.
			case formatAuto:
				if effective == "both" {
					effective = formatJSON
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
			syncErr = errors.Join(syncErr, err)
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
	case effective == formatJSON:
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
	threadKey := td.Section + "/" + td.Name
	// Descriptor caches contain no speculative SQL IDs. A failed message must
	// not publish participants or leave IDs that a later message can reuse.
	nameToAddress := make(map[string]mime.Address)
	for _, p := range thread.Participants {
		nameToAddress[p.Name] = Address(p.Name)
	}
	participantsCounted := false
	parentLabelID := labelIDs["Messenger"]
	sectionLabelID := labelIDs[sectionLabelName(td.Section)]
	rawFormat := "fbmessenger_" + thread.Format
	var snapshotErrors error
	for mi, m := range thread.Messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		if opts.Limit > 0 && summary.MessagesAdded >= int64(opts.Limit) {
			return errLimitReached
		}
		summary.MessagesProcessed++
		srcMsgID := fmt.Sprintf("%s__%s%d", threadKey, prefix, m.Index)
		senderName := m.SenderName
		isFromMe := senderName != "" && Slug(senderName) == meLocal
		var priorID sql.NullInt64
		var priorAddress mime.Address
		if senderName == "" {
			var priorName, priorEmail sql.NullString
			var priorIsFromMe bool
			err := st.DB().QueryRowContext(ctx, st.Rebind(`SELECT m.sender_id,m.is_from_me,
			 COALESCE(NULLIF(p.display_name,''),(SELECT mr.display_name FROM message_recipients mr WHERE mr.message_id=m.id AND mr.recipient_type='from' LIMIT 1)),p.email_address
			 FROM messages m LEFT JOIN participants p ON p.id=m.sender_id WHERE m.source_id=? AND m.source_message_id=?`), sourceID, srcMsgID).Scan(&priorID, &priorIsFromMe, &priorName, &priorEmail)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("read prior Messenger sender: %w", err)
			}
			if err == nil && priorID.Valid {
				isFromMe = priorIsFromMe
				senderName = priorName.String
				priorAddress = mime.Address{Name: senderName, Email: priorEmail.String, Domain: Domain}
			}
		}
		addresses := make(map[string]mime.Address, len(nameToAddress)+1)
		maps.Copy(addresses, nameToAddress)
		if senderName != "" {
			address := Address(senderName)
			if priorAddress.Email != "" {
				address = priorAddress
			}
			addresses[senderName] = address
		}
		var participants []store.ParticipantPersistData
		byEmail := make(map[string]int)
		add := func(addr mime.Address) int {
			if index, ok := byEmail[addr.Email]; ok {
				if addr.Name != "" {
					participants[index].DisplayName = addr.Name
				}
				return index
			}
			index := len(participants)
			byEmail[addr.Email] = index
			participants = append(participants, store.ParticipantPersistData{EmailAddress: addr.Email, DisplayName: addr.Name, Domain: addr.Domain})
			return index
		}
		meIndex := add(mime.Address{Email: opts.Me, Domain: Domain})
		sortedNames := make([]string, 0, len(addresses))
		for name := range addresses {
			sortedNames = append(sortedNames, name)
		}
		sort.Strings(sortedNames)
		nameIndex := make(map[string]int, len(addresses))
		nameToEmail := make(map[string]string, len(addresses))
		for _, name := range sortedNames {
			nameIndex[name] = add(addresses[name])
			nameToEmail[name] = addresses[name].Email
		}
		senderIndex := -1
		if senderName != "" {
			senderIndex = nameIndex[senderName]
		}
		if isFromMe {
			senderIndex = meIndex
		}
		reactionIndices := make([]int, len(m.Reactions))
		for i, reaction := range m.Reactions {
			reactionIndices[i] = add(Address(reaction.Actor))
		}
		var readyAttachments *store.KeyedAttachmentSnapshot
		preparedStored := 0
		if opts.NotifyImports && len(m.Attachments) > 0 {
			writes := make([]store.AttachmentWrite, 0, len(m.Attachments))
			for ai, attachment := range m.Attachments {
				write := prepareMessengerAttachment(attachment, ai, opts.AttachmentsDir)
				if write.StoragePath != "" {
					preparedStored++
				}
				writes = append(writes, write)
			}
			readyAttachments = &store.KeyedAttachmentSnapshot{Prefix: fbSyntheticAttachmentKeyPrefix, Writes: writes, PreserveStoredOnFailure: true, InvalidateCacheOnChange: true, PreserveUnmentioned: true}
		}
		messageID, err := st.PersistMessageWithParticipantsContext(ctx, participants, func(ids []int64) *store.MessagePersistData {
			var senderID sql.NullInt64
			if senderIndex >= 0 {
				senderID = sql.NullInt64{Int64: ids[senderIndex], Valid: true}
			} else if priorID.Valid {
				senderID = priorID
			}
			members := []store.ConversationParticipantRef{{ParticipantID: ids[meIndex], Role: "member"}}
			for _, name := range sortedNames {
				members = append(members, store.ConversationParticipantRef{ParticipantID: ids[nameIndex[name]], Role: "member"})
			}
			if priorID.Valid && senderIndex < 0 {
				members = append(members, store.ConversationParticipantRef{ParticipantID: priorID.Int64, Role: "member"})
			}
			var recipients []store.RecipientSet
			if senderID.Valid {
				recipients = append(recipients, store.RecipientSet{Type: "from", ParticipantIDs: []int64{senderID.Int64}, DisplayNames: []string{senderName}})
			}
			to := store.RecipientSet{Type: "to"}
			seen := make(map[int64]bool)
			if senderID.Valid {
				seen[senderID.Int64] = true
			}
			for _, name := range sortedNames {
				id := ids[nameIndex[name]]
				if !seen[id] {
					seen[id] = true
					to.ParticipantIDs = append(to.ParticipantIDs, id)
					to.DisplayNames = append(to.DisplayNames, name)
				}
			}
			if !isFromMe && !seen[ids[meIndex]] {
				to.ParticipantIDs = append(to.ParticipantIDs, ids[meIndex])
				to.DisplayNames = append(to.DisplayNames, "")
			}
			recipients = append(recipients, to)
			reactions := make([]store.ReactionRef, 0, len(m.Reactions))
			for i, reaction := range m.Reactions {
				reactions = append(reactions, store.ReactionRef{ParticipantID: ids[reactionIndices[i]], Type: "emoji", Value: reaction.Reaction, CreatedAt: m.SentAt})
			}
			var raw []byte
			if mi == 0 {
				raw = thread.RawBytes
			}
			var labels []int64
			for _, id := range []int64{parentLabelID, sectionLabelID} {
				if id != 0 {
					labels = append(labels, id)
				}
			}
			snippet := buildSnippet(m.Body)
			fromAddr := ""
			if senderID.Valid {
				fromAddr = nameToEmail[senderName]
				if isFromMe {
					fromAddr = opts.Me
				}
			}
			return &store.MessagePersistData{
				Message:      &store.Message{SourceID: sourceID, SourceMessageID: srcMsgID, MessageType: "fbmessenger", SentAt: sql.NullTime{Time: m.SentAt, Valid: !m.SentAt.IsZero()}, ReceivedAt: sql.NullTime{Time: m.SentAt, Valid: !m.SentAt.IsZero()}, SenderID: senderID, IsFromMe: isFromMe, Snippet: sql.NullString{String: snippet, Valid: snippet != ""}, SizeEstimate: int64(len(m.Body)), HasAttachments: len(m.Attachments) > 0, AttachmentCount: len(m.Attachments)},
				Conversation: &store.ConversationPersistData{SourceConversationID: threadKey, ConversationType: thread.ConvType, Title: thread.Title, Participants: members, PreserveExistingParticipants: true},
				BodyText:     sql.NullString{String: m.Body, Valid: m.Body != ""}, RawMIME: raw, RawFormat: rawFormat, Recipients: recipients, PreserveLabels: true, AddLabelIDs: labels, AddReactionRefs: reactions, KeyedAttachmentSnapshot: readyAttachments,
				FTS: &store.FTSDoc{Subject: thread.Title, Body: m.Body, FromAddr: fromAddr, ToAddrs: strings.Join(nameToEmailsList(nameToEmail, opts.Me, senderName), " ")}, FTSBestEffort: true,
			}
		})
		if err != nil {
			summary.Errors++
			summary.MessagesSkipped++
			snapshotErrors = errors.Join(snapshotErrors, err)
			logger.Warn("fbmessenger: persist message snapshot", "err", err)
			continue
		}
		nameToAddress = addresses
		if !participantsCounted {
			unique := map[string]bool{opts.Me: true}
			for _, p := range thread.Participants {
				unique[Address(p.Name).Email] = true
			}
			summary.ParticipantsResolved += len(unique)
			participantsCounted = true
		}
		summary.MessagesAdded++
		if isFromMe {
			summary.FromMeCount++
		}
		summary.ReactionsAdded += len(m.Reactions)

		// Silent imports retain native placeholder and acquisition-gap behavior.
		if opts.NotifyImports {
			summary.AttachmentsFound += len(m.Attachments)
			summary.AttachmentsStored += preparedStored
		}
		for ai := range m.Attachments {
			if opts.NotifyImports {
				break
			}
			att := m.Attachments[ai]
			summary.AttachmentsFound++
			storagePath, contentHash, size := handleAttachment(att, opts.AttachmentsDir)
			if storagePath != "" {
				summary.AttachmentsStored++
			}
			syntheticHash := fbAttachmentHash(att, ai)
			hash := contentHash
			storedContent := storagePath != "" && contentHash != ""
			existingContentHash := contentHash
			if !storedContent && existingContentHash == "" {
				existingContentHash = hashAttachmentSource(att)
			}
			if !storedContent && existingContentHash != "" {
				stored, err := hasStoredAttachment(st, messageID, existingContentHash)
				if err != nil {
					logger.Warn("fbmessenger: check stored attachment", "err", err)
				} else if stored {
					if err := deleteLegacyHashlessAttachment(ctx, st, messageID); err != nil {
						logger.Warn("fbmessenger: delete legacy hashless attachment", "err", err)
					}
					if err := deleteSyntheticPlaceholderAttachment(ctx, st, messageID, syntheticHash); err != nil {
						logger.Warn("fbmessenger: delete synthetic attachment placeholder", "err", err)
					}
					continue
				}
			}
			if !storedContent {
				hash = syntheticHash
			} else if err := deleteFailedStoredAttachment(ctx, st, messageID, contentHash); err != nil {
				logger.Warn("fbmessenger: delete failed stored attachment", "err", err)
			}
			role := store.AttachmentRoleStandalone
			roleSource := store.AttachmentRoleSourceImporterSemantics
			if att.Kind == "sticker" {
				role = store.AttachmentRoleSticker
				roleSource = store.AttachmentRoleSourceProviderExplicit
			}
			if err := st.UpsertAttachmentRecord(ctx, messageID, store.AttachmentWrite{
				Filename: att.Filename, MIMEType: att.MimeType, StoragePath: storagePath,
				ContentHash: hash, Size: int64(size), SourceAttachmentID: att.URI,
				MediaType: att.Kind, Role: role, RoleSource: roleSource,
				SourcePartKey: syntheticHash,
			}); err != nil {
				logger.Warn("fbmessenger: upsert attachment", "err", err)
				continue
			}
			if err := deleteLegacyHashlessAttachment(ctx, st, messageID); err != nil {
				logger.Warn("fbmessenger: delete legacy hashless attachment", "err", err)
			}
			if storedContent {
				if err := deleteSyntheticPlaceholderAttachment(ctx, st, messageID, syntheticHash); err != nil {
					logger.Warn("fbmessenger: delete synthetic attachment placeholder", "err", err)
				}
			} else if contentHash != "" {
				if err := deleteFailedStoredAttachment(ctx, st, messageID, contentHash); err != nil {
					logger.Warn("fbmessenger: delete failed stored attachment", "err", err)
				}
			} else if err := deleteFailedStoredAttachmentByMetadata(ctx, st, messageID, att.Filename, att.MimeType); err != nil {
				logger.Warn("fbmessenger: delete failed stored attachment", "err", err)
			}
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
	if len(thread.Messages) == 0 {
		if err := ensureEmptyThreadTrace(ctx, st, sourceID, threadKey, thread, opts, summary); err != nil {
			return errors.Join(snapshotErrors, err)
		}
	}
	return snapshotErrors
}

func ensureEmptyThreadTrace(
	ctx context.Context,
	st *store.Store,
	sourceID int64,
	threadKey string,
	thread *Thread,
	opts ImportOptions,
	summary *ImportSummary,
) error {
	conversationID, err := st.EnsureConversationWithTypeContext(ctx, sourceID, threadKey, thread.ConvType, thread.Title)
	if err != nil {
		return fmt.Errorf("ensure empty Messenger conversation: %w", err)
	}
	addresses := make([]mime.Address, 0, len(thread.Participants)+1)
	addresses = append(addresses, mime.Address{Email: opts.Me, Domain: Domain})
	for _, participant := range thread.Participants {
		addresses = append(addresses, Address(participant.Name))
	}
	participantIDs, err := st.EnsureParticipantsBatch(addresses)
	if err != nil {
		return fmt.Errorf("ensure empty Messenger participants: %w", err)
	}
	summary.ParticipantsResolved += len(participantIDs)
	ownerID := participantIDs[opts.Me]
	if ownerID == 0 {
		return errors.New("ensure empty Messenger thread owner: participant was not returned")
	}
	if err := st.EnsureConversationParticipantContext(ctx, conversationID, ownerID, "member"); err != nil {
		return fmt.Errorf("link empty Messenger thread owner: %w", err)
	}
	seen := map[int64]bool{ownerID: true}
	for _, participant := range thread.Participants {
		participantID := participantIDs[Address(participant.Name).Email]
		if participantID == 0 || seen[participantID] {
			continue
		}
		if err := st.EnsureConversationParticipantContext(ctx, conversationID, participantID, "member"); err != nil {
			return fmt.Errorf("link empty Messenger thread participant: %w", err)
		}
		seen[participantID] = true
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
	}, json.Deterministic(true))
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

// prepareMessengerAttachment performs only native file preparation. The ready
// occurrence and any stronger stored reference are resolved in the canonical
// transaction, before the import occurrence can become visible.
func prepareMessengerAttachment(att Attachment, index int, dir string) store.AttachmentWrite {
	role, roleSource := store.AttachmentRoleStandalone, store.AttachmentRoleSourceImporterSemantics
	if att.Kind == "sticker" {
		role, roleSource = store.AttachmentRoleSticker, store.AttachmentRoleSourceProviderExplicit
	}
	write := store.AttachmentWrite{Filename: att.Filename, MIMEType: att.MimeType, SourceAttachmentID: att.URI, MediaType: att.Kind, Role: role, RoleSource: roleSource, SourcePartKey: fbAttachmentHash(att, index), State: attachmentpolicy.StateFailed, SkipReason: attachmentpolicy.SkipFetchFailure}
	if dir == "" {
		write.State = attachmentpolicy.StateSkipped
		write.SkipReason = attachmentpolicy.SkipPolicyScope
		return write
	}
	path, hash, size := handleAttachment(att, dir)
	if path != "" && hash != "" {
		write.StoragePath = path
		write.ContentHash = hash
		write.Size = int64(size)
		write.State = attachmentpolicy.StateStored
		write.SkipReason = ""
	}
	return write
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
	rel, contentHash, size, err := export.StoreAttachmentFromPath(attachmentsDir, att.AbsPath, 0)
	if err != nil {
		// contentHash is set when hashing succeeded but storage failed —
		// preserved so the caller's failed-store bookkeeping still works.
		return "", contentHash, 0
	}
	return rel, contentHash, int(size)
}

func hashAttachmentSource(att Attachment) string {
	if att.AbsPath == "" {
		return ""
	}
	linfo, err := os.Lstat(att.AbsPath)
	if err != nil || !linfo.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(att.AbsPath)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

const fbSyntheticAttachmentKeyPrefix = "fbmessenger:attachment:"

func deleteLegacyHashlessAttachment(ctx context.Context, st *store.Store, messageID int64) error {
	return st.DeleteLegacyHashlessAttachmentsContext(ctx, messageID)
}

func deleteSyntheticPlaceholderAttachment(
	ctx context.Context, st *store.Store, messageID int64, contentHash string,
) error {
	return st.DeleteUnstoredAttachmentByHashContext(ctx, messageID, contentHash)
}

func deleteFailedStoredAttachment(
	ctx context.Context, st *store.Store, messageID int64, contentHash string,
) error {
	return st.DeleteUnstoredAttachmentByHashContext(ctx, messageID, contentHash)
}

func deleteFailedStoredAttachmentByMetadata(
	ctx context.Context, st *store.Store, messageID int64, filename, mimeType string,
) error {
	return st.DeleteUnstoredAttachmentByMetadataContext(ctx, messageID, filename, mimeType)
}

func hasStoredAttachment(st *store.Store, messageID int64, contentHash string) (bool, error) {
	var stored bool
	err := st.DB().QueryRow(st.Rebind(`
		SELECT EXISTS (
			SELECT 1
			FROM attachments
			WHERE message_id = ?
			  AND content_hash = ?
			  AND storage_path <> ''
		)
	`), messageID, contentHash).Scan(&stored)
	return stored, err
}

// fbAttachmentHash derives a stable synthetic attachment key for an attachment
// that has no real (content-derived) hash — e.g. the file is missing or no
// attachments dir was configured. Without it, UpsertAttachment collapses every
// hashless attachment on a message to a single row. Keyed on the export-relative
// URI (present even when the file is missing), then AbsPath/Filename, with the
// loop index as a last-resort tiebreaker, so re-importing the same export is
// idempotent. The prefix prevents downstream export paths from mistaking it for
// a SHA-256 hash of stored bytes, so storage_path stays empty.
func fbAttachmentHash(att Attachment, idx int) string {
	key := att.URI
	if key == "" {
		key = att.AbsPath
	}
	if key == "" {
		key = att.Filename
	}
	if key == "" {
		key = fmt.Sprintf("idx-%d", idx)
	}
	sum := sha256.Sum256([]byte("fbmessenger\x00" + key))
	return fbSyntheticAttachmentKeyPrefix + hex.EncodeToString(sum[:])
}
