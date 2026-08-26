// Package sync provides Gmail synchronization workflows.
package sync

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// ErrHistoryExpired indicates that the Gmail history ID is too old and a full sync is required.
var ErrHistoryExpired = errors.New("history expired - run full sync")

const (
	labelTypeSystem               = "system"
	sourceTypeIMAP                = "imap"
	invalidatedIMAPSourceIDPrefix = "msgvault-invalidated:"
)

// Options configures sync behavior.
type Options struct {
	// SourceType is the type of source being synced ("gmail" or "imap").
	// Defaults to "gmail" if empty.
	SourceType string

	// Query is an optional Gmail search query (e.g., "before:2020/01/01")
	Query string

	// NoResume forces a fresh sync even if a checkpoint exists
	NoResume bool

	// BatchSize is the number of messages to fetch in parallel (default: 10)
	BatchSize int

	// AttachmentsDir is where to store attachments
	AttachmentsDir string

	// Limit caps the number of messages scanned per sync (0 = unlimited).
	// Enforced by truncating the message ID list before downloading content.
	// The API listing call (which returns lightweight IDs, not bodies) may
	// return more IDs than the limit; only the truncated set is fetched.
	Limit int
}

// DefaultOptions returns sensible defaults.
func DefaultOptions() *Options {
	return &Options{
		BatchSize:  10,
		SourceType: "gmail",
	}
}

// Syncer performs Gmail synchronization.
type Syncer struct {
	client             gmail.API
	store              *store.Store
	logger             *slog.Logger
	progress           gmail.SyncProgress
	opts               *Options
	successfulHookName string
	successfulHook     SuccessfulSyncHook
}

// SuccessfulSyncHook runs after the durable sync run is marked complete.
// mailboxChanged reports whether the run observed provider-side change; a
// no-op incremental run passes false so the hook can skip provider-facing
// work it has performed recently.
// Its failure is warning-only and never replaces the successful sync result.
type SuccessfulSyncHook func(ctx context.Context, source *store.Source, mailboxChanged bool) error

type messageAcknowledger interface {
	AcknowledgeMessages(ctx context.Context, messageIDs []string)
}

type labelSnapshotCompleteness interface {
	LabelsSnapshotComplete() bool
}

type authoritativeLabelReconciliationDeferrer interface {
	DefersAuthoritativeLabelReconciliation() bool
}

type limitedFullEnumerationForcer interface {
	ForceFullEnumerationForLimitedSync()
}

type sourceMessageMatcher interface {
	SourceMessageMatches(
		ctx context.Context,
		messageID, expectedRFC822MessageID string,
	) (matches bool, conclusive bool, err error)
}

type sourceMessageAliaser interface {
	CanonicalSourceMessageID(sourceMessageID string) (string, bool)
}

type preferredIMAPSourceID interface {
	IsPreferredSourceMessageID(messageID string) bool
}

type fetchedSourceMessageMatcher interface {
	FetchedSourceMessageMatches(
		messageID, expectedRFC822MessageID, actualRFC822MessageID string,
	) (matches bool, conclusive bool, err error)
}

type validatedMessageDedupSeeder interface {
	SeedValidatedMessageDedup(
		messageID, rfc822MessageID string,
	) error
}

type messageLabelBatchReader interface {
	GetMessageLabelsBatch(ctx context.Context, messageIDs []string) ([]gmail.MessageLabelsBatchResult, error)
}

// New creates a new Syncer.
func New(client gmail.API, store *store.Store, opts *Options) *Syncer {
	if opts == nil {
		opts = DefaultOptions()
	}

	if opts.Limit > 0 {
		if forcer, ok := client.(limitedFullEnumerationForcer); ok {
			forcer.ForceFullEnumerationForLimitedSync()
		}
	}

	return &Syncer{
		client:   client,
		store:    store,
		logger:   slog.Default(),
		progress: gmail.NullProgress{},
		opts:     opts,
	}
}

// WithLogger sets the logger.
func (s *Syncer) WithLogger(logger *slog.Logger) *Syncer {
	s.logger = logger
	return s
}

// WithProgress sets the progress reporter.
func (s *Syncer) WithProgress(p gmail.SyncProgress) *Syncer {
	s.progress = p
	return s
}

func (s *Syncer) labelsSnapshotComplete() bool {
	completeness, ok := s.client.(labelSnapshotCompleteness)
	return !ok || completeness.LabelsSnapshotComplete()
}

func (s *Syncer) defersAuthoritativeLabelReconciliation() bool {
	// Only an unlimited run can publish the deferred mailbox snapshot. Limited
	// runs must reconcile each processed message before returning.
	if s.opts.SourceType != sourceTypeIMAP || s.opts.Limit > 0 || !s.labelsSnapshotComplete() {
		return false
	}
	deferrer, ok := s.client.(authoritativeLabelReconciliationDeferrer)
	return ok && deferrer.DefersAuthoritativeLabelReconciliation()
}

// WithSuccessfulSyncHook installs one best-effort post-completion hook.
func (s *Syncer) WithSuccessfulSyncHook(name string, hook SuccessfulSyncHook) *Syncer {
	s.successfulHookName = strings.TrimSpace(name)
	s.successfulHook = hook
	return s
}

func (s *Syncer) runSuccessfulSyncHook(ctx context.Context, source *store.Source, mailboxChanged bool) {
	if s.successfulHook == nil {
		return
	}
	if err := s.successfulHook(ctx, source, mailboxChanged); err != nil {
		s.logger.Warn(
			"successful sync hook failed",
			"hook", s.successfulHookName,
			"source_id", source.ID,
			"error", err,
		)
	}
}

// completeSyncWithoutHook atomically publishes the source cursor and marks the
// still-current run complete.
func (s *Syncer) completeSyncWithoutHook(
	ctx context.Context, syncID int64, sourceID int64, historyID string,
) error {
	if err := s.store.CompleteSyncAndUpdateSourceCursorContext(
		ctx, syncID, sourceID, historyID,
	); err != nil {
		if !errors.Is(err, store.ErrSyncRunSuperseded) {
			s.failSyncUnlessCanceled(syncID, err)
		}
		return fmt.Errorf("publish completed sync: %w", err)
	}
	return nil
}

func (s *Syncer) completeSyncAndRunHook(
	ctx context.Context,
	syncID int64,
	historyID string,
	source *store.Source,
) error {
	if err := s.completeSyncWithoutHook(ctx, syncID, source.ID, historyID); err != nil {
		return err
	}
	s.runSuccessfulSyncHook(ctx, source, true)
	return nil
}

// identityDiscoveryRetryBackoff is the delay before the second per-page
// identity discovery attempt; each further attempt doubles it. It is a variable
// so tests can run the retry loop without real backoff.
var identityDiscoveryRetryBackoff = time.Second

const (
	identityDiscoveryAttempts          = 3
	identityDiscoveryRetryLogMessage   = "identity discovery failed for sync page; retrying"
	identityDiscoveryBacklogLogMessage = "identity discovery failed for sync page; recorded backlog"
	identityDiscoveryDrainLogMessage   = "draining identity discovery backlog"
)

// runPageIdentityDiscovery merges one page's identity evidence into the
// source's confirmed identities, retrying a bounded number of times.
//
// It deliberately returns no error. The page's messages are already durably
// archived by the time it runs, and the evidence it derives is recomputable
// from them, so failing the run here would discard real archived work over a
// debt that RefreshConfirmedForSource can settle later. Persistent failure is
// logged and parked in a durable per-source backlog marker instead. Callers
// must still check ctx after it returns: a cancelled sync is an interruption,
// not a discovery failure, and has to stay resumable.
//
// It reports whether the page ended up parking a backlog marker, which is the
// signal callers use to decide whether an immediate drain is worth attempting.
func (s *Syncer) runPageIdentityDiscovery(
	ctx context.Context,
	sourceID int64,
	sourceMessageIDs []string,
) (parkedBacklog bool) {
	backoff := identityDiscoveryRetryBackoff
	var err error
	for attempt := range identityDiscoveryAttempts {
		if _, err = identityops.DiscoverStrongForSourceMessageIDs(
			ctx, s.store, sourceID, sourceMessageIDs,
		); err == nil {
			return false
		}
		if ctx.Err() != nil || attempt == identityDiscoveryAttempts-1 {
			break
		}
		s.logger.Warn(identityDiscoveryRetryLogMessage,
			"source_id", sourceID, "attempt", attempt+1, "error", err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return false
		}
		backoff *= 2
	}
	if ctx.Err() != nil {
		return false
	}
	err = fmt.Errorf("discover identities for sync page: %w", err)
	s.logger.Warn(identityDiscoveryBacklogLogMessage, "source_id", sourceID, "error", err)
	if setErr := s.store.SetIdentityDiscoveryBacklogContext(ctx, sourceID, err); setErr != nil {
		s.logger.Warn("record identity discovery backlog", "source_id", sourceID, "error", setErr)
	}
	return true
}

// pageDiscoveryHealth tracks whether the most recent page's identity discovery
// worked, so a run only spends an end-of-run whole-archive refresh when there
// is reason to believe it can succeed.
type pageDiscoveryHealth struct {
	ran    bool
	parked bool
}

func (h *pageDiscoveryHealth) observe(parkedBacklog bool) {
	h.ran = true
	h.parked = parkedBacklog
}

// shouldDrainAtCompletion reports whether a run that just finished its pages
// should try to settle debt it parked itself. Retrying right after discovery
// failed every attempt on the last page costs a full archive scan to learn
// nothing; debt parked by an earlier page of a run that recovered is worth
// settling now rather than making the archive wait for the next sync.
func (h *pageDiscoveryHealth) shouldDrainAtCompletion() bool {
	return h.ran && !h.parked
}

// drainIdentityDiscoveryBacklog settles a parked discovery debt by re-deriving
// the source's confirmed identities from the whole archive. A refresh that
// fails leaves the marker in place for the next attempt; it never fails the
// sync that happens to be carrying it.
func (s *Syncer) drainIdentityDiscoveryBacklog(ctx context.Context, sourceID int64) {
	found, lastError, err := s.store.IdentityDiscoveryBacklogContext(ctx, sourceID)
	if err != nil {
		s.logger.Warn("read identity discovery backlog", "source_id", sourceID, "error", err)
		return
	}
	if !found {
		return
	}
	s.logger.Info(identityDiscoveryDrainLogMessage,
		"source_id", sourceID, "last_error", lastError)
	if err := identityops.RefreshConfirmedForSource(ctx, s.store, sourceID); err != nil {
		s.logger.Warn("drain identity discovery backlog", "source_id", sourceID, "error", err)
		return
	}
	if err := s.store.ClearIdentityDiscoveryBacklogContext(ctx, sourceID); err != nil {
		s.logger.Warn("clear identity discovery backlog", "source_id", sourceID, "error", err)
	}
}

// syncState holds the state for a sync operation.
type syncState struct {
	syncID        int64
	checkpoint    *store.Checkpoint
	pageToken     string
	handoffCursor string
	wasResumed    bool
}

func isPinnedHistoryRecovery(run *store.SyncRun) bool {
	return run != nil && run.CursorAfter.Valid && run.CursorAfter.String != ""
}

// failSyncUnlessCanceled marks the run failed for real errors. A cancelled
// sync (Ctrl-C, daemon shutdown, a scheduled sync yielding to a waiting
// operation) keeps status='running' with its saved checkpoint, matching the
// killed-process semantics GetActiveSync resumes from; marking it failed
// would discard the checkpoint and restart the sync from scratch.
func (s *Syncer) failSyncUnlessCanceled(syncID int64, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	_ = s.store.FailSync(syncID, err.Error())
}

// initSyncState initializes sync state, resuming from checkpoint if possible.
func (s *Syncer) initSyncState(sourceID int64) (*syncState, error) {
	state := &syncState{
		checkpoint: &store.Checkpoint{},
	}

	if !s.opts.NoResume {
		activeSync, err := s.store.GetActiveSync(sourceID)
		if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
			return nil, fmt.Errorf("check active sync: %w", err)
		}
		if activeSync != nil {
			state.syncID = activeSync.ID
			if activeSync.CursorBefore.Valid {
				state.pageToken = activeSync.CursorBefore.String
			}
			state.checkpoint = &store.Checkpoint{
				PageToken:         state.pageToken,
				MessagesProcessed: activeSync.MessagesProcessed,
				MessagesAdded:     activeSync.MessagesAdded,
				MessagesUpdated:   activeSync.MessagesUpdated,
				ErrorsCount:       activeSync.ErrorsCount,
			}
			state.wasResumed = true
			s.logger.Info("resuming sync", "messages_processed", state.checkpoint.MessagesProcessed)
			return state, nil
		}
	}

	// Start new sync
	syncID, err := s.store.StartSync(sourceID, "full")
	if err != nil {
		return nil, fmt.Errorf("start sync: %w", err)
	}
	state.syncID = syncID
	return state, nil
}

// initHistoryRecoveryState only resumes a full run that already pinned its
// history handoff cursor. An ordinary full-sync checkpoint cannot be reused:
// its processed prefix may have changed before recovery captured a cursor.
func (s *Syncer) initHistoryRecoveryState(sourceID int64) (*syncState, error) {
	if !s.opts.NoResume {
		activeSync, err := s.store.GetActiveSync(sourceID)
		if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
			return nil, fmt.Errorf("check active history recovery: %w", err)
		}
		if isPinnedHistoryRecovery(activeSync) {
			state := &syncState{
				syncID:        activeSync.ID,
				checkpoint:    &store.Checkpoint{},
				handoffCursor: activeSync.CursorAfter.String,
				wasResumed:    true,
			}
			if activeSync.CursorBefore.Valid {
				state.pageToken = activeSync.CursorBefore.String
			}
			state.checkpoint = &store.Checkpoint{
				PageToken:         state.pageToken,
				MessagesProcessed: activeSync.MessagesProcessed,
				MessagesAdded:     activeSync.MessagesAdded,
				MessagesUpdated:   activeSync.MessagesUpdated,
				ErrorsCount:       activeSync.ErrorsCount,
			}
			s.logger.Info("resuming Gmail history recovery",
				"messages_processed", state.checkpoint.MessagesProcessed,
				"handoff_cursor", state.handoffCursor)
			return state, nil
		}
	}

	syncID, err := s.store.StartSync(sourceID, "full")
	if err != nil {
		return nil, fmt.Errorf("start history recovery: %w", err)
	}
	return &syncState{syncID: syncID, checkpoint: &store.Checkpoint{}}, nil
}

// batchResult holds the result of processing a batch.
type batchResult struct {
	processed        int64
	added            int64
	updated          int64
	skipped          int64
	oldestDate       time.Time
	acknowledged     []string
	sourceMessageIDs []string
}

type inconclusiveLabelRefresh struct {
	labelIDs         []int64
	rfc822MessageID  string
	snapshotComplete bool
}

// processBatch processes a single batch of messages from a list response.
func (s *Syncer) processBatch(ctx context.Context, syncID, sourceID int64, listResp *gmail.MessageListResponse, labelMap map[string]int64, checkpoint *store.Checkpoint, summary *gmail.SyncSummary) (*batchResult, error) {
	result := &batchResult{}

	if len(listResp.Messages) == 0 {
		return result, nil
	}

	// Build message ID list and thread ID map
	messageIDs := make([]string, len(listResp.Messages))
	threadIDs := make(map[string]string) // messageID -> threadID
	for i, m := range listResp.Messages {
		messageIDs[i] = m.ID
		threadIDs[m.ID] = m.ThreadID
	}
	result.sourceMessageIDs = messageIDs

	// Check which messages already exist
	lookupMessageIDs := append([]string(nil), messageIDs...)
	aliases := make(map[string]string)
	if s.opts.SourceType == sourceTypeIMAP {
		if aliaser, ok := s.client.(sourceMessageAliaser); ok {
			for _, sourceMessageID := range messageIDs {
				canonicalSourceMessageID, exists := aliaser.CanonicalSourceMessageID(sourceMessageID)
				if !exists || canonicalSourceMessageID == sourceMessageID {
					continue
				}
				aliases[sourceMessageID] = canonicalSourceMessageID
				lookupMessageIDs = append(lookupMessageIDs, canonicalSourceMessageID)
			}
		}
	}
	existingMap, err := s.store.MessageMetadataWithRawBatch(
		sourceID, lookupMessageIDs)
	if err != nil {
		return nil, fmt.Errorf("check existing: %w", err)
	}
	for sourceMessageID, canonicalSourceMessageID := range aliases {
		if canonical, exists := existingMap[canonicalSourceMessageID]; exists {
			existingMap[sourceMessageID] = canonical
		}
	}

	// Existing exact IDs only need current label metadata. Clients with a
	// lightweight metadata path use it for both complete snapshots (replace
	// labels) and incomplete snapshots (merge labels). Keep the old fallback
	// for clients that cannot fetch labels separately.
	snapshotComplete := s.labelsSnapshotComplete()
	labelReader, canReadLabels := s.client.(messageLabelBatchReader)
	var fetchIDs []string
	var labelRefreshIDs []string
	var existingCount int
	inconclusiveRefreshes := make(map[string]inconclusiveLabelRefresh)
	for _, id := range messageIDs {
		if _, exists := existingMap[id]; !exists {
			fetchIDs = append(fetchIDs, id)
			continue
		}
		existingCount++
		if canReadLabels {
			labelRefreshIDs = append(labelRefreshIDs, id)
			continue
		}
		if snapshotComplete {
			result.acknowledged = append(result.acknowledged, id)
			continue
		}
		fetchIDs = append(fetchIDs, id)
	}

	result.processed = int64(len(messageIDs))
	result.skipped = int64(existingCount)

	if len(labelRefreshIDs) > 0 {
		labelResults, err := labelReader.GetMessageLabelsBatch(ctx, labelRefreshIDs)
		if err != nil {
			for _, id := range labelRefreshIDs {
				s.recordSyncItem(syncID, id, syncItemPhaseFetch, store.SyncRunItemStatusError, syncItemKindBatchFetchError, err)
			}
			checkpoint.ErrorsCount += int64(len(labelRefreshIDs))
			return nil, fmt.Errorf("fetch message labels: %w", err)
		}

		for i, sourceMessageID := range labelRefreshIDs {
			if i >= len(labelResults) {
				err := errors.New("label metadata missing from batch response")
				s.logger.Warn("failed to fetch message labels",
					"id", sourceMessageID, "error", err)
				s.recordSyncItem(syncID, sourceMessageID, syncItemPhaseFetch, store.SyncRunItemStatusError, syncItemKindFetchError, err)
				checkpoint.ErrorsCount++
				continue
			}
			labelResult := labelResults[i]
			if labelResult.Err != nil {
				s.logger.Warn("failed to fetch message labels",
					"id", sourceMessageID, "error", labelResult.Err)
				s.recordSyncItem(syncID, sourceMessageID, syncItemPhaseFetch, store.SyncRunItemStatusError, syncItemKindFetchError, labelResult.Err)
				checkpoint.ErrorsCount++
				continue
			}

			existing := existingMap[sourceMessageID]
			labelIDs := labelIDsFor(labelResult.LabelIDs, labelMap)
			if s.opts.SourceType == sourceTypeIMAP {
				matcher, ok := s.client.(fetchedSourceMessageMatcher)
				if !ok {
					err := errors.New(
						"fetched source validation unavailable for IMAP exact-ID refresh")
					s.logger.Warn("failed to validate existing message identity",
						"id", sourceMessageID, "error", err)
					s.recordSyncItem(
						syncID,
						sourceMessageID,
						syncItemPhaseIngest,
						store.SyncRunItemStatusError,
						syncItemKindIngestError,
						err,
					)
					checkpoint.ErrorsCount++
					continue
				}

				matches, conclusive, err := matcher.FetchedSourceMessageMatches(
					sourceMessageID,
					existing.RFC822MessageID.String,
					labelResult.RFC822MessageID,
				)
				if err != nil {
					s.logger.Warn("failed to validate existing message identity",
						"id", sourceMessageID, "error", err)
					s.recordSyncItem(
						syncID,
						sourceMessageID,
						syncItemPhaseIngest,
						store.SyncRunItemStatusError,
						syncItemKindIngestError,
						err,
					)
					checkpoint.ErrorsCount++
					continue
				}
				if !conclusive {
					inconclusiveRefreshes[sourceMessageID] =
						inconclusiveLabelRefresh{
							labelIDs:         labelIDs,
							rfc822MessageID:  labelResult.RFC822MessageID,
							snapshotComplete: snapshotComplete,
						}
					fetchIDs = append(fetchIDs, sourceMessageID)
					continue
				}
				if !matches {
					err := s.preserveReusedIMAPSource(
						existing.ID, sourceMessageID)
					if err != nil {
						s.logger.Warn("failed to preserve reused IMAP source ID",
							"id", sourceMessageID, "error", err)
						s.recordSyncItem(
							syncID,
							sourceMessageID,
							syncItemPhaseIngest,
							store.SyncRunItemStatusError,
							syncItemKindIngestError,
							err,
						)
						checkpoint.ErrorsCount++
						continue
					}
					delete(existingMap, sourceMessageID)
					fetchIDs = append(fetchIDs, sourceMessageID)
					result.skipped--
					continue
				}
			}

			changed, err := s.reconcileValidatedMessageLabels(
				existing.ID,
				sourceMessageID,
				labelResult.RFC822MessageID,
				labelIDs,
				snapshotComplete,
			)
			if err != nil {
				s.logger.Warn("failed to refresh existing message labels",
					"id", sourceMessageID, "error", err)
				s.recordSyncItem(syncID, sourceMessageID, syncItemPhaseIngest, store.SyncRunItemStatusError, syncItemKindIngestError, err)
				checkpoint.ErrorsCount++
				continue
			}
			if changed {
				result.updated++
				result.skipped--
			}
			result.acknowledged = append(result.acknowledged, sourceMessageID)
		}
	}

	// Raw MIME is needed for new messages and as a compatibility fallback for
	// incomplete snapshots whose client cannot fetch labels separately.
	if len(fetchIDs) > 0 {
		rawMessages, err := s.getMessagesRawBatchWithDiagnostics(ctx, fetchIDs)
		if err != nil {
			for _, id := range fetchIDs {
				s.recordSyncItem(syncID, id, syncItemPhaseFetch, store.SyncRunItemStatusError, syncItemKindBatchFetchError, err)
			}
			checkpoint.ErrorsCount += int64(len(fetchIDs))
			return nil, fmt.Errorf("fetch messages: %w", err)
		}

		for i, fetch := range rawMessages {
			sourceMessageID := fetchIDs[i]
			existing, alreadyExists := existingMap[sourceMessageID]
			pendingRefresh, needsRawComparison :=
				inconclusiveRefreshes[sourceMessageID]
			raw := fetch.Message
			if raw == nil {
				if isGmailNotFound(fetch.Err) {
					s.logger.Debug("skipping message deleted before fetch", "id", sourceMessageID)
					s.recordSyncItem(syncID, sourceMessageID, syncItemPhaseFetch, store.SyncRunItemStatusSkipped, syncItemKindGmailNotFound, fetch.Err)
					if !alreadyExists {
						result.skipped++
					}
					result.acknowledged = append(result.acknowledged, sourceMessageID)
					continue
				}
				errMsg := syncItemErrorMessage(fetch.Err, errRawBatchMissing.Error())
				s.logger.Warn("failed to fetch message", "id", sourceMessageID, "error", errMsg)
				s.recordSyncItem(syncID, sourceMessageID, syncItemPhaseFetch, store.SyncRunItemStatusError, syncItemKindFetchError, fetch.Err)
				checkpoint.ErrorsCount++
				continue
			}
			if needsRawComparison && raw.Raw == nil {
				err := errors.New(
					"inconclusive IMAP identity returned a dedup stub")
				s.logger.Warn("failed to compare existing message identity",
					"id", sourceMessageID, "error", err)
				s.recordSyncItem(
					syncID,
					sourceMessageID,
					syncItemPhaseIngest,
					store.SyncRunItemStatusError,
					syncItemKindIngestError,
					err,
				)
				checkpoint.ErrorsCount++
				continue
			}
			// Non-nil stub with nil Raw signals a cross-mailbox
			// dedup skip (e.g. same message in All Mail and Trash).
			// Distinct from []byte{} which is a genuine empty body.
			if raw.Raw == nil {
				if !alreadyExists {
					result.skipped++
				}
				result.acknowledged = append(result.acknowledged, sourceMessageID)
				continue
			}

			if needsRawComparison {
				archivedRaw, err := s.store.GetMessageRaw(existing.ID)
				if err != nil {
					s.logger.Warn("failed to load archived message for identity comparison",
						"id", sourceMessageID, "error", err)
					s.recordSyncItem(
						syncID,
						sourceMessageID,
						syncItemPhaseIngest,
						store.SyncRunItemStatusError,
						syncItemKindIngestError,
						err,
					)
					checkpoint.ErrorsCount++
					continue
				}
				if bytes.Equal(archivedRaw, raw.Raw) {
					changed, err := s.reconcileValidatedMessageLabels(
						existing.ID,
						sourceMessageID,
						pendingRefresh.rfc822MessageID,
						pendingRefresh.labelIDs,
						pendingRefresh.snapshotComplete,
					)
					if err != nil {
						s.logger.Warn("failed to refresh raw-validated message labels",
							"id", sourceMessageID, "error", err)
						s.recordSyncItem(
							syncID,
							sourceMessageID,
							syncItemPhaseIngest,
							store.SyncRunItemStatusError,
							syncItemKindIngestError,
							err,
						)
						checkpoint.ErrorsCount++
						continue
					}
					if changed {
						result.updated++
						result.skipped--
					}
					result.acknowledged = append(
						result.acknowledged, sourceMessageID)
					summary.BytesDownloaded += int64(len(raw.Raw))
					continue
				}

				if err := s.preserveReusedIMAPSource(
					existing.ID, sourceMessageID); err != nil {
					s.logger.Warn("failed to preserve reused IMAP source ID",
						"id", sourceMessageID, "error", err)
					s.recordSyncItem(
						syncID,
						sourceMessageID,
						syncItemPhaseIngest,
						store.SyncRunItemStatusError,
						syncItemKindIngestError,
						err,
					)
					checkpoint.ErrorsCount++
					continue
				}
				delete(existingMap, sourceMessageID)
				alreadyExists = false
				result.skipped--
			}

			if alreadyExists {
				changed := false
				if !s.defersAuthoritativeLabelReconciliation() {
					changed, err = s.store.ReconcileMessageLabels(
						existing.ID,
						labelIDsFor(raw.LabelIDs, labelMap),
						false,
					)
					if err != nil {
						s.logger.Warn("failed to merge existing message labels",
							"id", sourceMessageID, "error", err)
						s.recordSyncItem(syncID, sourceMessageID, syncItemPhaseIngest, store.SyncRunItemStatusError, syncItemKindIngestError, err)
						checkpoint.ErrorsCount++
						continue
					}
				}
				if changed {
					result.updated++
					result.skipped--
				}
				result.acknowledged = append(result.acknowledged, sourceMessageID)
				summary.BytesDownloaded += int64(len(raw.Raw))
				continue
			}

			// Track oldest message date for progress display
			// Gmail returns messages newest-to-oldest, so oldest shows where we've reached
			if raw.InternalDate > 0 {
				// .UTC() to match how InternalDate is normalized everywhere
				// else (the stored message date and parsed.Date both use
				// .UTC()); without it oldestDate carries the local zone and
				// callers reading its calendar day are off by one east of UTC.
				msgDate := time.UnixMilli(raw.InternalDate).UTC()
				if result.oldestDate.IsZero() || msgDate.Before(result.oldestDate) {
					result.oldestDate = msgDate
				}
			}

			threadID := threadIDs[sourceMessageID]
			updated, err := s.ingestMessage(
				ctx, sourceID, raw, threadID, labelMap)
			if err != nil {
				if errors.Is(err, errDeferredIMAPIdentity) {
					if updated {
						result.updated++
					} else {
						result.skipped++
					}
					continue
				}
				if errors.Is(err, errDuplicateRFC822) {
					if updated {
						result.updated++
					} else {
						result.skipped++
					}
					result.acknowledged = append(result.acknowledged, sourceMessageID)
					continue
				}
				s.logger.Warn("failed to ingest message", "id", raw.ID, "error", err)
				s.recordSyncItem(syncID, sourceMessageID, syncItemPhaseIngest, store.SyncRunItemStatusError, syncItemKindIngestError, err)
				checkpoint.ErrorsCount++
				continue
			}

			result.added++
			result.acknowledged = append(result.acknowledged, sourceMessageID)
			summary.BytesDownloaded += int64(len(raw.Raw))
		}

		// Newly-persisted messages get embed_gen = NULL by column default,
		// so the scan-and-fill embed worker picks them up automatically on
		// its next run — no sync-time enqueue step is needed.
	}

	return result, nil
}

// Full performs a full synchronization.
func (s *Syncer) Full(ctx context.Context, email string) (summary *gmail.SyncSummary, err error) {
	sourceType := s.opts.SourceType
	if sourceType == "" {
		sourceType = "gmail"
	}
	if sourceType == "gmail" && !s.opts.NoResume {
		source, sourceErr := s.store.GetOrCreateSource(sourceType, email)
		if sourceErr != nil {
			return nil, fmt.Errorf("get/create source: %w", sourceErr)
		}
		active, activeErr := s.store.GetActiveSync(source.ID)
		if activeErr != nil && !errors.Is(activeErr, store.ErrSyncRunNotFound) {
			return nil, fmt.Errorf("check active history recovery: %w", activeErr)
		}
		if isPinnedHistoryRecovery(active) {
			return s.RecoverExpiredHistory(ctx, source)
		}
	}
	return s.full(ctx, email, false)
}

// RecoverExpiredHistory rebuilds the archive from a complete Gmail listing,
// reconciles source-presence metadata, then consumes changes made after the
// full run pinned its starting history cursor.
func (s *Syncer) RecoverExpiredHistory(
	ctx context.Context, source *store.Source,
) (*gmail.SyncSummary, error) {
	if source == nil {
		return nil, errors.New("recover expired history: no source provided")
	}
	if source.SourceType != "gmail" {
		return nil, fmt.Errorf("recover expired history: source %d is %s, not gmail", source.ID, source.SourceType)
	}
	if s.opts.Query != "" || s.opts.Limit > 0 {
		return nil, errors.New("recover expired history requires an unfiltered, unlimited full sync")
	}

	fullSummary, err := s.full(ctx, source.Identifier, true)
	if err != nil {
		return nil, fmt.Errorf("recover expired history: full sync: %w", err)
	}
	refreshed, err := s.store.GetSourceByID(source.ID)
	if err != nil {
		return nil, fmt.Errorf("recover expired history: reload source: %w", err)
	}
	catchup, err := s.Incremental(ctx, refreshed)
	if err != nil {
		return nil, fmt.Errorf("recover expired history: catch up from full-sync cursor: %w", err)
	}
	fullSummary.MessagesFound += catchup.MessagesFound
	fullSummary.MessagesAdded += catchup.MessagesAdded
	fullSummary.MessagesUpdated += catchup.MessagesUpdated
	fullSummary.MessagesSkipped += catchup.MessagesSkipped
	fullSummary.BytesDownloaded += catchup.BytesDownloaded
	fullSummary.Errors += catchup.Errors
	fullSummary.EndTime = catchup.EndTime
	fullSummary.Duration = fullSummary.EndTime.Sub(fullSummary.StartTime)
	fullSummary.FinalHistoryID = catchup.FinalHistoryID
	return fullSummary, nil
}

// IncrementalWithHistoryRecovery resumes an interrupted history recovery
// before starting a new incremental run. If Gmail reports an expired cursor,
// it starts recovery immediately. onRecovery runs before the potentially long
// recovery; resumed distinguishes a saved recovery from a newly expired cursor.
func (s *Syncer) IncrementalWithHistoryRecovery(
	ctx context.Context, source *store.Source, onRecovery func(resumed bool),
) (*gmail.SyncSummary, error) {
	if source == nil {
		return nil, errors.New("no source provided - run full sync first")
	}
	active, err := s.store.GetActiveSync(source.ID)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return nil, fmt.Errorf("check active history recovery: %w", err)
	}
	if !s.opts.NoResume && isPinnedHistoryRecovery(active) {
		if onRecovery != nil {
			onRecovery(true)
		}
		return s.RecoverExpiredHistory(ctx, source)
	}

	summary, err := s.Incremental(ctx, source)
	if !errors.Is(err, ErrHistoryExpired) {
		return summary, err
	}
	if onRecovery != nil {
		onRecovery(false)
	}
	return s.RecoverExpiredHistory(ctx, source)
}

func (s *Syncer) full(ctx context.Context, email string, reconcilePresence bool) (summary *gmail.SyncSummary, err error) {
	startTime := time.Now()
	summary = &gmail.SyncSummary{StartTime: startTime}

	// Get or create source
	sourceType := s.opts.SourceType
	if sourceType == "" {
		sourceType = "gmail"
	}
	source, err := s.store.GetOrCreateSource(sourceType, email)
	if err != nil {
		return nil, fmt.Errorf("get/create source: %w", err)
	}

	// Recovery may only resume a run that pinned its history handoff cursor.
	// Ordinary full-sync checkpoints predate that cursor and are unsafe to reuse.
	var state *syncState
	if reconcilePresence {
		state, err = s.initHistoryRecoveryState(source.ID)
	} else {
		state, err = s.initSyncState(source.ID)
	}
	if err != nil {
		return nil, err
	}
	scoped := *s
	scoped.store = s.store.ScopedToSync(source.ID, state.syncID)
	s = &scoped
	summary.SyncRunID = state.syncID
	summary.WasResumed = state.wasResumed
	summary.ResumedFromToken = state.pageToken

	// Defer failure handling — recover from panics and return as error
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			s.logger.Error("sync panic recovered", "panic", r, "stack", string(stack))
			if failErr := s.store.FailSync(state.syncID, fmt.Sprintf("panic: %v", r)); failErr != nil {
				s.logger.Error("failed to record sync failure", "error", failErr)
			}
			summary = nil
			err = fmt.Errorf("sync panicked: %v", r)
		}
	}()

	// Get profile to verify connection and get historyId
	profile, err := s.client.GetProfile(ctx)
	if err != nil {
		s.failSyncUnlessCanceled(state.syncID, err)
		return nil, fmt.Errorf("get profile: %w", err)
	}
	handoffHistoryID := profile.HistoryID
	if reconcilePresence {
		if state.handoffCursor == "" {
			state.handoffCursor = strconv.FormatUint(profile.HistoryID, 10)
			if err := s.store.PinSyncHandoffCursorContext(ctx, state.syncID, state.handoffCursor); err != nil {
				s.failSyncUnlessCanceled(state.syncID, err)
				return nil, fmt.Errorf("pin Gmail history recovery cursor: %w", err)
			}
		} else {
			handoffHistoryID, err = strconv.ParseUint(state.handoffCursor, 10, 64)
			if err != nil {
				s.failSyncUnlessCanceled(state.syncID, err)
				return nil, fmt.Errorf("parse Gmail history recovery cursor %q: %w", state.handoffCursor, err)
			}
		}
	}

	s.logger.Info("syncing account", "email", profile.EmailAddress, "messages", profile.MessagesTotal)

	// Sync labels
	labelMap, err := s.syncLabels(ctx, source.ID)
	if err != nil {
		s.failSyncUnlessCanceled(state.syncID, err)
		return nil, fmt.Errorf("sync labels: %w", err)
	}

	// Settle any discovery debt a previous run parked before this run adds to it.
	s.drainIdentityDiscoveryBacklog(ctx, source.ID)

	// List and sync messages
	var totalEstimate int64
	var discoveryHealth pageDiscoveryHealth
	firstPage := true
	pageToken := state.pageToken

	for {
		// List messages
		listResp, err := s.client.ListMessages(ctx, s.opts.Query, pageToken)
		if err != nil {
			s.failSyncUnlessCanceled(state.syncID, err)
			return nil, fmt.Errorf("list messages: %w", err)
		}

		if firstPage {
			totalEstimate = listResp.ResultSizeEstimate
			s.progress.OnStart(totalEstimate)
			firstPage = false
		}

		if len(listResp.Messages) == 0 {
			break
		}

		// Enforce limit by truncating before the expensive content download
		if s.opts.Limit > 0 {
			remaining := int64(s.opts.Limit) - state.checkpoint.MessagesProcessed
			if remaining <= 0 {
				break
			}
			if int64(len(listResp.Messages)) > remaining {
				listResp.Messages = listResp.Messages[:int(remaining)]
			}
		}

		// Process batch
		result, err := s.processBatch(ctx, state.syncID, source.ID, listResp, labelMap, state.checkpoint, summary)
		if err != nil {
			if checkpointErr := s.store.UpdateSyncCheckpoint(state.syncID, state.checkpoint); checkpointErr != nil {
				s.logger.Warn("failed to save checkpoint before failing sync", "error", checkpointErr)
			}
			s.failSyncUnlessCanceled(state.syncID, err)
			return nil, err
		}

		discoveryHealth.observe(s.runPageIdentityDiscovery(ctx, source.ID, result.sourceMessageIDs))
		if ctxErr := ctx.Err(); ctxErr != nil {
			err := fmt.Errorf("sync canceled during identity discovery: %w", ctxErr)
			s.failSyncUnlessCanceled(state.syncID, err)
			return nil, err
		}
		if ack, ok := s.client.(messageAcknowledger); ok && len(result.acknowledged) > 0 {
			ack.AcknowledgeMessages(ctx, result.acknowledged)
		}

		state.checkpoint.MessagesProcessed += result.processed
		state.checkpoint.MessagesAdded += result.added
		state.checkpoint.MessagesUpdated += result.updated

		// Report current position date before progress (so UI shows consistent state)
		if !result.oldestDate.IsZero() {
			if p, ok := s.progress.(gmail.SyncProgressWithDate); ok {
				p.OnLatestDate(result.oldestDate)
			}
		}

		// Report progress
		s.progress.OnProgress(state.checkpoint.MessagesProcessed, state.checkpoint.MessagesAdded, result.skipped)

		// Save checkpoint
		pageToken = listResp.NextPageToken
		state.checkpoint.PageToken = pageToken
		if err := s.store.UpdateSyncCheckpoint(state.syncID, state.checkpoint); err != nil {
			s.logger.Warn("failed to save checkpoint", "error", err)
		}

		// Stop if we've hit the limit
		if s.opts.Limit > 0 && state.checkpoint.MessagesProcessed >= int64(s.opts.Limit) {
			break
		}

		// No more pages
		if pageToken == "" {
			break
		}
	}

	if reconcilePresence {
		present, err := s.listCompleteMessageSnapshot(ctx)
		if err != nil {
			s.failSyncUnlessCanceled(state.syncID, err)
			return nil, err
		}
		reconciled, err := s.store.ReconcileSourceMessageSnapshot(ctx, source.ID, present)
		if err != nil {
			s.failSyncUnlessCanceled(state.syncID, err)
			return nil, fmt.Errorf("reconcile Gmail message snapshot: %w", err)
		}
		if reconciled > 0 {
			s.logger.Info("reconciled Gmail source deletions", "source_id", source.ID, "messages", reconciled)
		}
	}

	// Debt parked by an earlier page of this run is settled now, provided the
	// run showed discovery recovering.
	if discoveryHealth.shouldDrainAtCompletion() {
		s.drainIdentityDiscoveryBacklog(ctx, source.ID)
	}

	// Update source with final history ID.
	// Full sync always advances the cursor (it records the starting point
	// for future incremental syncs), but warn when errors occurred.
	historyIDStr := strconv.FormatUint(handoffHistoryID, 10)
	if state.checkpoint.ErrorsCount > 0 {
		s.logger.Warn("full sync completed with errors",
			"errors", state.checkpoint.ErrorsCount,
			"history_id", historyIDStr)
	}
	// Mark sync complete before running best-effort provider maintenance.
	if err := s.completeSyncAndRunHook(ctx, state.syncID, historyIDStr, source); err != nil {
		return nil, err
	}

	// Checkpoint WAL after sync to fold it back into the main database.
	// This prevents WAL accumulation across long sync sessions and ensures
	// readers (e.g. build-cache) see a consistent database state.
	if err := s.store.CheckpointWAL(); err != nil {
		s.logger.Warn("wal checkpoint after sync failed", "error", err)
	}

	// Build summary
	summary.EndTime = time.Now()
	summary.Duration = summary.EndTime.Sub(summary.StartTime)
	summary.MessagesFound = state.checkpoint.MessagesProcessed
	summary.MessagesAdded = state.checkpoint.MessagesAdded
	summary.MessagesUpdated = state.checkpoint.MessagesUpdated
	summary.MessagesSkipped = state.checkpoint.MessagesProcessed - state.checkpoint.MessagesAdded - state.checkpoint.MessagesUpdated
	summary.Errors = state.checkpoint.ErrorsCount
	summary.FinalHistoryID = handoffHistoryID

	s.progress.OnComplete(summary)
	return summary, nil
}

func (s *Syncer) listCompleteMessageSnapshot(ctx context.Context) (map[string]struct{}, error) {
	lister, ok := s.client.(gmail.CompleteMessageSnapshotReader)
	if !ok {
		return nil, errors.New("complete Gmail message snapshot is unavailable")
	}

	present := make(map[string]struct{})
	pageToken := ""
	for {
		response, err := lister.ListCompleteMessageSnapshot(ctx, pageToken)
		if err != nil {
			return nil, fmt.Errorf("list complete Gmail message snapshot: %w", err)
		}
		for _, message := range response.Messages {
			present[message.ID] = struct{}{}
		}
		next := response.NextPageToken
		if next == "" {
			return present, nil
		}
		if next == pageToken {
			return nil, fmt.Errorf("list complete Gmail message snapshot: page token did not advance from %q", pageToken)
		}
		pageToken = next
	}
}

// syncLabels syncs all labels and returns a map of Gmail label ID to internal ID.
func (s *Syncer) syncLabels(ctx context.Context, sourceID int64) (map[string]int64, error) {
	labels, err := s.client.ListLabels(ctx)
	if err != nil {
		return nil, err
	}

	labelInfos := make(map[string]store.LabelInfo)
	for _, l := range labels {
		labelType := l.Type
		if labelType == "" {
			labelType = "user"
			if store.IsSystemLabel(l.ID) {
				labelType = labelTypeSystem
			}
		}
		labelInfos[l.ID] = store.LabelInfo{
			Name:       l.Name,
			Type:       labelType,
			SystemRole: l.SystemRole,
		}
	}

	return s.store.EnsureLabelsBatch(sourceID, labelInfos)
}

// messageData holds all parsed data for a message before persistence.
type messageData struct {
	message        *store.Message
	bodyText       string
	bodyHTML       string
	rawMIME        []byte
	from           []mime.Address
	to             []mime.Address
	cc             []mime.Address
	bcc            []mime.Address
	gmailLabelIDs  []string
	attachments    []mime.Attachment
	participantMap map[string]int64
}

// parseToModel parses a raw Gmail message into a messageData struct.
func (s *Syncer) parseToModel(sourceID int64, raw *gmail.RawMessage, threadID string) (*messageData, error) {
	// Validate raw MIME data exists
	if len(raw.Raw) == 0 {
		return nil, fmt.Errorf("missing raw MIME data for message %s", raw.ID)
	}

	// Fall back to raw.ThreadID if list response threadID is missing,
	// then to message ID as last resort
	if threadID == "" {
		threadID = raw.ThreadID
		if threadID == "" {
			threadID = raw.ID
		}
	}

	// Parse MIME - on failure, store with placeholder body
	// (threading override for IMAP happens after parsing below)
	parsed, parseErr := mime.Parse(raw.Raw)
	if parseErr != nil {
		// Extract just the first line of error (enmime includes full stack traces)
		errMsg := textutil.FirstLine(parseErr.Error())

		// Create placeholder message for MIME parse failures
		// This preserves the raw data for potential future re-parsing
		parsed = &mime.Message{
			Subject:  extractSubjectFromSnippet(raw.Snippet),
			BodyText: fmt.Sprintf("[MIME parsing failed: %s]\n\nRaw MIME data is preserved in message_raw table.", errMsg),
		}
		s.logger.Warn("MIME parse failed, storing with placeholder",
			"id", raw.ID,
			"error", errMsg)
	}

	// For IMAP sources, the API provides no real thread info —
	// ThreadID is just the composite message ID. Derive a thread
	// key from MIME threading headers to group related messages
	// into conversations.
	if s.opts.SourceType == sourceTypeIMAP {
		if derived := deriveThreadKey(parsed); derived != "" {
			threadID = derived
		}
	}

	// Ensure all text fields are valid UTF-8
	subject := textutil.EnsureUTF8(parsed.Subject)
	bodyText := textutil.EnsureUTF8(parsed.GetBodyText())
	bodyHTML := textutil.EnsureUTF8(parsed.BodyHTML)
	snippet := textutil.EnsureUTF8(raw.Snippet)

	// Ensure participant names are valid UTF-8 before database insertion
	ensureAddressUTF8(parsed.From)
	ensureAddressUTF8(parsed.To)
	ensureAddressUTF8(parsed.Cc)
	ensureAddressUTF8(parsed.Bcc)

	// Ensure attachment filenames and content types are valid UTF-8
	for i := range parsed.Attachments {
		parsed.Attachments[i].Filename = textutil.EnsureUTF8(parsed.Attachments[i].Filename)
		parsed.Attachments[i].ContentType = textutil.EnsureUTF8(parsed.Attachments[i].ContentType)
	}

	// Ensure participants exist in database
	allAddresses := make([]mime.Address, 0, len(parsed.From)+len(parsed.To)+len(parsed.Cc)+len(parsed.Bcc))
	allAddresses = append(allAddresses, parsed.From...)
	allAddresses = append(allAddresses, parsed.To...)
	allAddresses = append(allAddresses, parsed.Cc...)
	allAddresses = append(allAddresses, parsed.Bcc...)
	participantMap, err := s.store.EnsureParticipantsBatch(allAddresses)
	if err != nil {
		return nil, fmt.Errorf("ensure participants: %w", err)
	}

	// Get sender ID
	var senderID sql.NullInt64
	if len(parsed.From) > 0 && parsed.From[0].Email != "" {
		if id, ok := participantMap[parsed.From[0].Email]; ok {
			senderID = sql.NullInt64{Int64: id, Valid: true}
		}
	}

	// Use placeholder for conversation matching only (subject can be empty for storage)
	convSubject := subject
	if convSubject == "" {
		convSubject = "(no subject)"
	}
	conversationID, err := s.store.EnsureConversation(sourceID, threadID, convSubject)
	if err != nil {
		return nil, fmt.Errorf("ensure conversation: %w", err)
	}

	// Build message record
	rfc822ID := sql.NullString{}
	if parsed.MessageID != "" {
		rfc822ID = sql.NullString{
			String: parsed.MessageID, Valid: true,
		}
	}
	msg := &store.Message{
		ConversationID:  conversationID,
		SourceID:        sourceID,
		SourceMessageID: raw.ID,
		RFC822MessageID: rfc822ID,
		MessageType:     "email",
		SenderID:        senderID,
		Subject:         sql.NullString{String: subject, Valid: subject != ""},
		Snippet:         sql.NullString{String: snippet, Valid: snippet != ""},
		SizeEstimate:    raw.SizeEstimate,
		HasAttachments:  len(parsed.Attachments) > 0,
		AttachmentCount: len(parsed.Attachments),
	}

	// Set dates - always store in UTC for consistent querying.
	now := time.Now().UTC()
	var fallbackDate time.Time
	if raw.InternalDate > 0 {
		fallbackDate = time.UnixMilli(raw.InternalDate).UTC()
		msg.InternalDate = sql.NullTime{Time: fallbackDate, Valid: true}
	}
	resolvedDate, dateSource := mime.ResolveMessageDate(
		parsed.Date,
		parsed.ReceivedDates,
		fallbackDate,
		now,
	)
	if !parsed.Date.IsZero() && !mime.IsPlausibleDate(parsed.Date, now) {
		s.logger.Warn(
			"ignored implausible email Date header",
			"id", raw.ID,
			"header_date", parsed.Date.UTC(),
			"replacement_source", dateSource,
		)
	}
	if !resolvedDate.IsZero() {
		msg.SentAt = sql.NullTime{Time: resolvedDate, Valid: true}
	}

	return &messageData{
		message:        msg,
		bodyText:       bodyText,
		bodyHTML:       bodyHTML,
		rawMIME:        raw.Raw,
		from:           parsed.From,
		to:             parsed.To,
		cc:             parsed.Cc,
		bcc:            parsed.Bcc,
		gmailLabelIDs:  raw.LabelIDs,
		attachments:    parsed.Attachments,
		participantMap: participantMap,
	}, nil
}

// persistMessage stores a parsed message and all related data, returning
// the internal message ID to callers. No vector-search enqueue happens
// here: persisted rows leave embed_gen NULL (column default) and the
// scan-and-fill worker discovers them later.
func (s *Syncer) persistMessage(data *messageData, labelMap map[string]int64) (int64, error) {
	// Map Gmail label IDs to internal IDs
	var labelIDs []int64
	for _, gmailLabelID := range data.gmailLabelIDs {
		if internalID, ok := labelMap[gmailLabelID]; ok {
			labelIDs = append(labelIDs, internalID)
		}
	}

	// Build recipient sets
	recipients := []struct {
		typ   string
		addrs []mime.Address
	}{
		{"from", data.from},
		{"to", data.to},
		{"cc", data.cc},
		{"bcc", data.bcc},
	}
	var recipientSets []store.RecipientSet
	for _, r := range recipients {
		rs := buildRecipientSet(r.typ, r.addrs, data.participantMap)
		recipientSets = append(recipientSets, rs)
	}

	// Persist atomically
	messageID, err := s.store.PersistMessage(&store.MessagePersistData{
		Message:    data.message,
		BodyText:   sql.NullString{String: data.bodyText, Valid: data.bodyText != ""},
		BodyHTML:   sql.NullString{String: data.bodyHTML, Valid: data.bodyHTML != ""},
		RawMIME:    data.rawMIME,
		Recipients: recipientSets,
		LabelIDs:   labelIDs,
	})
	if err != nil {
		return 0, err
	}

	// Store attachments (best-effort, file I/O outside transaction)
	if s.opts.AttachmentsDir != "" && len(data.attachments) > 0 {
		for _, att := range data.attachments {
			if err := s.storeAttachment(messageID, &att); err != nil {
				s.logger.Warn("failed to store attachment", "message", messageID, "filename", att.Filename, "error", err)
			}
		}

		// Correct metadata if any attachments failed to store
		var storedCount int
		if err := s.store.DB().QueryRow(
			s.store.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`),
			messageID,
		).Scan(&storedCount); err != nil {
			s.logger.Warn("failed to count stored attachments",
				"message", messageID, "error", err)
		} else if storedCount != len(data.attachments) {
			if err := s.store.RecomputeMessageAttachmentStats(messageID); err != nil {
				s.logger.Warn("failed to update attachment metadata",
					"message", messageID, "error", err)
			}
		}
	}

	// Populate FTS index (best-effort outside transaction)
	if s.store.FTS5Available() {
		subject := ""
		if data.message.Subject.Valid {
			subject = data.message.Subject.String
		}
		fromAddr := joinEmails(data.from)
		toAddrs := joinEmails(data.to)
		ccAddrs := joinEmails(data.cc)
		if err := s.store.UpsertFTS(messageID, subject, data.bodyText, fromAddr, toAddrs, ccAddrs); err != nil {
			s.logger.Warn("failed to upsert FTS", "message", messageID, "error", err)
		}
	}

	return messageID, nil
}

// errDuplicateRFC822 signals that a message was skipped because
// another message with the same RFC822 Message-ID already exists
// for this source. Used for cross-sync dedup on IMAP where
// composite IDs change when messages move between mailboxes.
var errDuplicateRFC822 = errors.New("duplicate RFC822 Message-ID")

// errDeferredIMAPIdentity signals that deduplication found an archived
// canonical ID whose mailbox is outside the current folder selection. The
// alternate ID remains unacknowledged so a later inclusive scan retries it.
var errDeferredIMAPIdentity = errors.New(
	"deferred IMAP source identity validation")

func labelIDsFor(sourceLabelIDs []string, labelMap map[string]int64) []int64 {
	labelIDs := make([]int64, 0, len(sourceLabelIDs))
	for _, label := range sourceLabelIDs {
		if id, ok := labelMap[label]; ok {
			labelIDs = append(labelIDs, id)
		}
	}
	return labelIDs
}

// ingestMessage parses and stores a single message. The boolean reports
// whether RFC822 dedup reconciliation changed an existing message. It returns
// errDuplicateRFC822 or errDeferredIMAPIdentity for IMAP deduplication skips.
func (s *Syncer) ingestMessage(
	ctx context.Context,
	sourceID int64,
	raw *gmail.RawMessage,
	threadID string,
	labelMap map[string]int64,
) (bool, error) {
	data, err := s.parseToModel(sourceID, raw, threadID)
	if err != nil {
		return false, err
	}

	// For IMAP sources, check if a message with the same RFC822
	// Message-ID already exists under a different composite ID.
	// This handles messages that moved between mailboxes across
	// syncs (e.g. All Mail → Trash changes the mailbox|uid key).
	// Validate the archived composite ID before replacing it regardless of
	// snapshot completeness. Completeness controls only whether reconciled
	// labels replace the stored memberships or merge with them.
	if s.opts.SourceType == sourceTypeIMAP &&
		data.message.RFC822MessageID.Valid {
		existingID, err := s.store.GetMessageIDByRFC822ID(
			sourceID, data.message.RFC822MessageID.String)
		if err != nil {
			return false, fmt.Errorf("check rfc822 dedup: %w", err)
		}
		if existingID > 0 {
			labelIDs := labelIDsFor(data.gmailLabelIDs, labelMap)
			matcher, ok := s.client.(sourceMessageMatcher)
			if !ok {
				return false, errors.New(
					"exact source validation unavailable for IMAP dedup")
			}

			oldSourceMessageID, err := s.store.GetMessageSourceID(existingID)
			if err != nil {
				return false, fmt.Errorf("get dedup source ID: %w", err)
			}
			matches := false
			conclusive := true
			if !isInvalidatedIMAPSourceID(oldSourceMessageID) {
				matches, conclusive, err = matcher.SourceMessageMatches(
					ctx,
					oldSourceMessageID,
					data.message.RFC822MessageID.String,
				)
				if err != nil {
					return false, fmt.Errorf(
						"validate dedup source ID %q: %w",
						oldSourceMessageID, err)
				}
			}
			deferLabels := s.defersAuthoritativeLabelReconciliation()
			if !conclusive {
				if deferLabels {
					return false, errDeferredIMAPIdentity
				}
				changed, err := s.store.ReconcileMessageLabels(
					existingID, labelIDs, false)
				return dedupMutationResultWithSentinel(
					changed,
					"merge deferred dedup labels",
					err,
					errDeferredIMAPIdentity,
				)
			}
			complete := s.labelsSnapshotComplete()
			if matches {
				changed := false
				if !deferLabels {
					changed, err = s.store.ReconcileMessageLabels(
						existingID, labelIDs, complete)
					if err != nil {
						return false, fmt.Errorf(
							"reconcile validated dedup labels: %w", err)
					}
				}
				if complete && oldSourceMessageID != data.message.SourceMessageID {
					if preferred, ok := s.client.(preferredIMAPSourceID); ok &&
						preferred.IsPreferredSourceMessageID(data.message.SourceMessageID) {
						rekeyed, err := s.store.RekeyMessageSourceID(
							existingID, oldSourceMessageID, data.message.SourceMessageID)
						if err != nil {
							return false, fmt.Errorf(
								"adopt preferred IMAP source ID: %w", err)
						}
						if !rekeyed {
							return false, fmt.Errorf(
								"preferred IMAP source ID %q changed before adoption",
								data.message.SourceMessageID)
						}
						changed = true
					}
				}
				return dedupMutationResult(
					changed, "reconcile validated dedup labels", nil)
			}
			if complete {
				if deferLabels {
					rekeyed, err := s.store.RekeyMessageSourceID(
						existingID, oldSourceMessageID, data.message.SourceMessageID)
					if err != nil {
						return false, fmt.Errorf("rekey dedup message: %w", err)
					}
					if !rekeyed {
						return false, fmt.Errorf(
							"source message ID %q changed before dedup rekey",
							oldSourceMessageID)
					}
					return dedupMutationResult(
						true, "rekey dedup message", nil)
				}
				changed, err := s.store.UpdateMessageOnDedup(
					existingID,
					data.message.SourceMessageID,
					labelIDs,
				)
				return dedupMutationResult(
					changed, "update dedup message", err)
			}
			changed, err := s.store.UpdateMessageOnPartialDedup(
				existingID,
				data.message.SourceMessageID,
				labelIDs,
			)
			return dedupMutationResult(
				changed, "update moved dedup message", err)
		}
	}

	_, err = s.persistMessage(data, labelMap)
	return false, err
}

func invalidatedIMAPSourceID(messageID int64) string {
	return invalidatedIMAPSourceIDPrefix +
		strconv.FormatInt(messageID, 10)
}

func (s *Syncer) preserveReusedIMAPSource(
	existingID int64,
	sourceMessageID string,
) error {
	rekeyed, err := s.store.RekeyMessageSourceID(
		existingID,
		sourceMessageID,
		invalidatedIMAPSourceID(existingID),
	)
	if err != nil {
		return err
	}
	if !rekeyed {
		return fmt.Errorf(
			"source message ID %q changed before invalidation",
			sourceMessageID,
		)
	}
	return nil
}

func (s *Syncer) reconcileValidatedMessageLabels(
	existingID int64,
	sourceMessageID string,
	rfc822MessageID string,
	labelIDs []int64,
	snapshotComplete bool,
) (bool, error) {
	changed := false
	if !s.defersAuthoritativeLabelReconciliation() {
		var err error
		changed, err = s.store.ReconcileMessageLabels(
			existingID, labelIDs, snapshotComplete)
		if err != nil {
			return false, err
		}
	}
	if seeder, ok := s.client.(validatedMessageDedupSeeder); ok {
		if err := seeder.SeedValidatedMessageDedup(
			sourceMessageID, rfc822MessageID); err != nil {
			return false, fmt.Errorf("seed validated message dedup: %w", err)
		}
	}
	return changed, nil
}

func isInvalidatedIMAPSourceID(sourceMessageID string) bool {
	return strings.HasPrefix(
		sourceMessageID, invalidatedIMAPSourceIDPrefix)
}

func dedupMutationResult(
	changed bool,
	action string,
	err error,
) (bool, error) {
	return dedupMutationResultWithSentinel(
		changed, action, err, errDuplicateRFC822)
}

func dedupMutationResultWithSentinel(
	changed bool,
	action string,
	err, sentinel error,
) (bool, error) {
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return changed, sentinel
}

// ensureAddressUTF8 validates and converts address names to valid UTF-8 in place.
func ensureAddressUTF8(addrs []mime.Address) {
	for i := range addrs {
		addrs[i].Name = textutil.EnsureUTF8(addrs[i].Name)
	}
}

// buildRecipientSet deduplicates addresses and returns a RecipientSet
// ready for store.PersistMessage.
func buildRecipientSet(recipientType string, addresses []mime.Address, participantMap map[string]int64) store.RecipientSet {
	rs := store.RecipientSet{Type: recipientType}
	if len(addresses) == 0 {
		return rs
	}

	// One row per (participant, normalized envelope address): two aliases
	// that resolve to the same already-merged participant each keep their
	// own envelope snapshot instead of collapsing onto the first-seen
	// address, so identity discovery still observes both. Display names
	// stay per participant, preferring non-empty names — a duplicate whose
	// first occurrence has an empty name picks up a later, better one, and
	// every row of that participant carries the same name.
	type rowKey struct {
		participantID int64
		email         string
	}
	idToName := make(map[int64]string)
	seen := make(map[rowKey]struct{})
	var orderedIDs []int64
	var orderedEmails []string

	for _, addr := range addresses {
		id, ok := participantMap[addr.Email]
		if !ok {
			continue
		}
		name := textutil.EnsureUTF8(addr.Name)
		if existing, tracked := idToName[id]; !tracked || (existing == "" && name != "") {
			idToName[id] = name
		}
		key := rowKey{participantID: id, email: strings.ToLower(addr.Email)}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		orderedIDs = append(orderedIDs, id)
		orderedEmails = append(orderedEmails, addr.Email)
	}

	rs.ParticipantIDs = orderedIDs
	rs.DisplayNames = make([]string, len(orderedIDs))
	rs.EmailAddresses = orderedEmails
	for i, id := range orderedIDs {
		rs.DisplayNames[i] = idToName[id]
	}
	return rs
}

// storeAttachment stores an attachment to disk and records it in the database.
func (s *Syncer) storeAttachment(messageID int64, att *mime.Attachment) error {
	storagePath, err := export.StoreAttachmentFile(s.opts.AttachmentsDir, att)
	if err != nil || storagePath == "" {
		return err
	}

	role, roleSource := store.AttachmentRoleFromMIME(
		att.Disposition, att.IsInline, att.ContentID)
	return s.store.UpsertAttachmentRecord(context.Background(), messageID, store.AttachmentWrite{
		Filename:      att.Filename,
		MIMEType:      att.ContentType,
		StoragePath:   storagePath,
		ContentHash:   att.ContentHash,
		Size:          int64(len(att.Content)),
		Role:          role,
		RoleSource:    roleSource,
		SourcePartKey: att.PartKey,
		ContentID:     att.ContentID,
	})
}

// joinEmails concatenates email addresses from a slice of mime.Address with spaces.
func joinEmails(addrs []mime.Address) string {
	if len(addrs) == 0 {
		return ""
	}
	emails := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a.Email != "" {
			emails = append(emails, a.Email)
		}
	}
	return strings.Join(emails, " ")
}

// deriveThreadKey extracts a thread identifier from parsed MIME
// headers. Returns the thread root Message-ID from References
// (first entry per RFC 2822), falls back to InReplyTo for simple
// replies, then to the message's own Message-ID. Returns "" when
// no threading info is available.
//
// InReplyTo is parsed as a msg-id list per RFC 2822 (it may
// contain multiple IDs and comments); only the first valid ID
// is used. Angle brackets are stripped for consistency with
// parseReferences.
func deriveThreadKey(parsed *mime.Message) string {
	if len(parsed.References) > 0 {
		return parsed.References[0]
	}
	if parsed.InReplyTo != "" {
		if ids := parseMsgIDList(parsed.InReplyTo); len(ids) > 0 {
			return ids[0]
		}
	}
	if parsed.MessageID != "" {
		if ids := parseMsgIDList(parsed.MessageID); len(ids) > 0 {
			return ids[0]
		}
	}
	return ""
}

// parseMsgIDList extracts angle-bracketed message-IDs from a
// header value (In-Reply-To, Message-ID, References). Only
// content inside < > pairs is returned, with brackets stripped.
// Comments, CFWS, and bare tokens are ignored per RFC 2822
// msg-id syntax.
func parseMsgIDList(s string) []string {
	var result []string
	for {
		open := strings.IndexByte(s, '<')
		if open < 0 {
			break
		}
		closeIdx := strings.IndexByte(s[open+1:], '>')
		if closeIdx < 0 {
			break
		}
		id := s[open+1 : open+1+closeIdx]
		if id != "" {
			result = append(result, id)
		}
		s = s[open+1+closeIdx+1:]
	}
	return result
}

// extractSubjectFromSnippet attempts to extract a subject from the message snippet.
// Used as fallback when MIME parsing fails.
func extractSubjectFromSnippet(snippet string) string {
	if snippet == "" {
		return "(MIME parse error)"
	}
	// Use first line of snippet, truncated
	line := textutil.FirstLine(snippet)
	return textutil.TruncateRunes(line, 80)
}
