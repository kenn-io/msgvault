package sync

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

var errGmailMetadataRefresh = errors.New("refresh Gmail label metadata")

// Incremental performs an incremental sync using the Gmail History API. It
// returns ErrHistoryExpired when Gmail requires a full history recovery.
//
// The caller must resolve the correct *store.Source before calling this
// method. This avoids ambiguity when multiple sources share the same
// identifier (e.g. a Gmail and IMAP source for the same email address).
func (s *Syncer) Incremental(ctx context.Context, source *store.Source) (summary *gmail.SyncSummary, err error) {
	if source == nil {
		return nil, errors.New("no source provided - run full sync first")
	}
	return s.runWithSyncExecution(ctx, source.ID, func(execution *store.SyncExecution) (*gmail.SyncSummary, error) {
		return s.incremental(ctx, source, execution)
	})
}

func (s *Syncer) incremental(
	ctx context.Context, source *store.Source, execution *store.SyncExecution,
) (summary *gmail.SyncSummary, err error) {
	startTime := time.Now()
	summary = &gmail.SyncSummary{StartTime: startTime}

	// Get last history ID
	if !source.SyncCursor.Valid || source.SyncCursor.String == "" {
		return nil, fmt.Errorf("no history ID for %s - run full sync first", source.Identifier)
	}

	startHistoryID, err := strconv.ParseUint(source.SyncCursor.String, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid history ID %q: %w", source.SyncCursor.String, err)
	}

	// Read the prior run's fetch debt before a new run exists, so a ledger error
	// cannot fail a run and the completed run stays visible for the next attempt.
	pendingReplayIDs, err := s.pendingFetchReplayIDs(source.ID)
	if err != nil {
		return nil, fmt.Errorf("find pending fetch replays: %w", err)
	}

	// Start sync
	syncID, err := s.startSync(ctx, execution, "incremental", "")
	if err != nil {
		return nil, fmt.Errorf("start sync: %w", err)
	}
	scoped := *s
	scoped.store = s.store.ScopedToSync(source.ID, syncID).WithIngestContext(store.IngestContext{Mode: store.IngestUnknown, ObservedAt: time.Now().UTC()})
	s = &scoped
	summary.SyncRunID = syncID

	// Defer failure handling — recover from panics and return as error
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			s.logger.Error("sync panic recovered", "panic", r, "stack", string(stack))
			if failErr := s.store.FailSync(syncID, fmt.Sprintf("panic: %v", r)); failErr != nil {
				s.logger.Error("failed to record sync failure", "error", failErr)
			}
			summary = nil
			err = fmt.Errorf("sync panicked: %v", r)
		}
	}()

	// Get profile for current history ID
	profile, err := s.client.GetProfile(ctx)
	if err != nil {
		_ = s.store.FailSync(syncID, err.Error())
		return nil, fmt.Errorf("get profile: %w", err)
	}

	if err := s.refreshProfileIdentity(ctx, source, profile); err != nil {
		_ = s.store.FailSync(syncID, err.Error())
		return nil, err
	}

	s.logger.Info("incremental sync", "email", source.Identifier, "start_history", startHistoryID, "current_history", profile.HistoryID)

	// Settle any discovery debt a previous run parked, before the up-to-date
	// check can return. A wound-down archive syncs to a no-op indefinitely, so a
	// drain behind that return would never run again for the account that needs
	// it most. Unlike the post-completion hook below, this re-derives evidence
	// from local messages and costs one key lookup when there is no debt.
	s.drainIdentityDiscoveryBacklog(ctx, source.ID)

	// If history IDs match, nothing to do locally — but provider inventory can
	// change while a mailbox is idle, so the post-completion hook still runs,
	// flagged as a no-op so it only reaches for the provider when a refresh is
	// owed (never succeeded, previously failed, or stale).
	if startHistoryID >= profile.HistoryID && len(pendingReplayIDs) == 0 {
		s.logger.Info("already up to date")
		if err := s.completeSyncWithoutHook(
			ctx, syncID, source.ID, strconv.FormatUint(profile.HistoryID, 10), true,
		); err != nil {
			return nil, err
		}
		s.runSuccessfulSyncHook(ctx, source, false)
		summary.EndTime = time.Now()
		summary.Duration = summary.EndTime.Sub(summary.StartTime)
		summary.FinalHistoryID = profile.HistoryID
		return summary, nil
	}

	// Sync labels first (new labels may have been created)
	labelMap, err := s.syncLabels(ctx, source.ID)
	if err != nil {
		_ = s.store.FailSync(syncID, err.Error())
		return nil, fmt.Errorf("sync labels: %w", err)
	}

	checkpoint := &store.Checkpoint{}
	var discoveryHealth pageDiscoveryHealth

	// Retry the prior run's absent fetches inside this scoped run, before history
	// selection, so an idle mailbox still works off its durable debt.
	if len(pendingReplayIDs) > 0 {
		replayedIDs, err := s.replayFetchFailures(
			ctx, syncID, source.ID, pendingReplayIDs, labelMap, checkpoint, summary,
		)
		if err != nil {
			if checkpointErr := s.store.UpdateSyncCheckpoint(syncID, checkpoint); checkpointErr != nil {
				s.logger.Warn("failed to save checkpoint before failing sync", "error", checkpointErr)
			}
			s.failStoppedSync(syncID, err)
			return nil, err
		}
		if len(replayedIDs) > 0 {
			discoveryHealth.observe(s.runPageIdentityDiscovery(ctx, source.ID, replayedIDs))
			if ctxErr := ctx.Err(); ctxErr != nil {
				err := fmt.Errorf("sync canceled during replay identity discovery: %w", ctxErr)
				s.failStoppedSync(syncID, err)
				return nil, err
			}
		}
		if err := s.store.UpdateSyncCheckpoint(syncID, checkpoint); err != nil {
			s.logger.Warn("failed to save replay checkpoint", "error", err)
		}
	}

	if startHistoryID < profile.HistoryID {
		// Process history
		pageToken := ""

		for {
			historyResp, err := s.client.ListHistory(ctx, startHistoryID, pageToken)
			if err != nil {
				// Check for 404 - history too old
				if _, ok := errors.AsType[*gmail.NotFoundError](err); ok {
					s.logger.Info("gmail history expired; full sync required")
					_ = s.store.FailSync(syncID, "history too old")
					// Callers fall back to a full sync on ErrHistoryExpired.
					return nil, ErrHistoryExpired
				}
				_ = s.store.FailSync(syncID, err.Error())
				return nil, fmt.Errorf("list history: %w", err)
			}

			// Collect all message IDs referenced in this page for a single batch existence check
			allIDs := make(map[string]bool)
			for _, record := range historyResp.History {
				for _, msg := range record.MessagesAdded {
					allIDs[msg.Message.ID] = true
				}
				for _, item := range record.LabelsAdded {
					allIDs[item.Message.ID] = true
				}
				for _, item := range record.LabelsRemoved {
					allIDs[item.Message.ID] = true
				}
			}
			idList := make([]string, 0, len(allIDs))
			for id := range allIDs {
				idList = append(idList, id)
			}
			existingMap, err := s.store.MessageExistsBatch(source.ID, idList)
			if err != nil {
				_ = s.store.FailSync(syncID, err.Error())
				return nil, fmt.Errorf("check existing messages: %w", err)
			}

			// Collect new message IDs to batch-fetch and deleted IDs to batch-mark
			newMsgThreads := make(map[string]string) // deduplicates by ID
			deletedSet := make(map[string]bool)
			updatedExisting := make(map[string]struct{})
			identityDiscoverySet := make(map[string]struct{})

			for _, record := range historyResp.History {
				for _, msg := range record.MessagesAdded {
					if _, exists := existingMap[msg.Message.ID]; !exists {
						newMsgThreads[msg.Message.ID] = msg.Message.ThreadID
					} else {
						identityDiscoverySet[msg.Message.ID] = struct{}{}
					}
				}
				for _, msg := range record.MessagesDeleted {
					deletedSet[msg.Message.ID] = true
				}
			}
			for _, record := range historyResp.History {
				if err := s.processLabelChanges(ctx, syncID, source.ID, record, labelMap, existingMap, newMsgThreads, updatedExisting, identityDiscoverySet, checkpoint, summary); err != nil {
					err = fmt.Errorf("process incremental Gmail labels: %w", err)
					if failErr := s.store.FailSyncWithCheckpoint(syncID, err.Error(), checkpoint); failErr != nil {
						return nil, errors.Join(err, fmt.Errorf("fail incremental sync: %w", failErr))
					}
					return nil, err
				}
			}
			checkpoint.MessagesUpdated += int64(len(updatedExisting))
			checkpoint.MessagesProcessed += int64(len(newMsgThreads) + len(deletedSet) + len(updatedExisting))
			newMsgIDs := make([]string, 0, len(newMsgThreads))
			for id := range newMsgThreads {
				newMsgIDs = append(newMsgIDs, id)
			}
			deletedIDs := make([]string, 0, len(deletedSet))
			for id := range deletedSet {
				deletedIDs = append(deletedIDs, id)
			}

			// Batch-fetch and ingest new messages
			if len(newMsgIDs) > 0 {
				rawMessages, fetchErr := s.getMessagesRawBatchWithDiagnostics(ctx, newMsgIDs)
				if fetchErr != nil {
					s.logger.Warn("failed to batch fetch messages", "error", fetchErr)
					for _, id := range newMsgIDs {
						s.recordSyncItem(syncID, id, syncItemPhaseFetch, store.SyncRunItemStatusError, syncItemKindBatchFetchError, fetchErr)
					}
					checkpoint.ErrorsCount += int64(len(newMsgIDs))
				} else {
					for i, fetch := range rawMessages {
						raw := fetch.Message
						if raw == nil {
							if isGmailNotFound(fetch.Err) {
								s.logger.Debug("skipping message deleted before fetch", "id", newMsgIDs[i])
								s.recordSyncItem(syncID, newMsgIDs[i], syncItemPhaseFetch, store.SyncRunItemStatusSkipped, syncItemKindGmailNotFound, fetch.Err)
								continue
							}
							errMsg := syncItemErrorMessage(fetch.Err, errRawBatchMissing.Error())
							s.logger.Warn("failed to fetch message", "id", newMsgIDs[i], "error", errMsg)
							s.recordSyncItem(syncID, newMsgIDs[i], syncItemPhaseFetch, store.SyncRunItemStatusError, syncItemKindFetchError, fetch.Err)
							checkpoint.ErrorsCount++
							continue
						}
						threadID := newMsgThreads[newMsgIDs[i]]
						if _, err := s.ingestLiveMessage(
							ctx, source.ID, raw, threadID, labelMap,
						); err != nil {
							s.logger.Warn("failed to ingest added message", "id", newMsgIDs[i], "error", err)
							s.recordSyncItem(syncID, newMsgIDs[i], syncItemPhaseIngest, store.SyncRunItemStatusError, syncItemKindIngestError, err)
							checkpoint.ErrorsCount++
							continue
						}
						checkpoint.MessagesAdded++
						summary.BytesDownloaded += int64(len(raw.Raw))
						identityDiscoverySet[newMsgIDs[i]] = struct{}{}
					}

					// Newly-persisted messages get embed_gen = NULL by column
					// default, so the scan-and-fill embed worker picks them up
					// automatically — no sync-time enqueue step is needed.
				}
			}

			// Batch-mark deleted messages
			if len(deletedIDs) > 0 {
				if err := s.store.MarkMessagesDeletedBatch(source.ID, deletedIDs); err != nil {
					s.logger.Warn("failed to batch mark messages deleted", "error", err)
					for _, id := range deletedIDs {
						s.recordSyncItem(syncID, id, syncItemPhaseDelete, store.SyncRunItemStatusError, syncItemKindDeleteError, err)
					}
					checkpoint.ErrorsCount += int64(len(deletedIDs))
				}
			}

			identityDiscoveryIDs := make([]string, 0, len(identityDiscoverySet))
			for id := range identityDiscoverySet {
				identityDiscoveryIDs = append(identityDiscoveryIDs, id)
			}
			sort.Strings(identityDiscoveryIDs)
			discoveryHealth.observe(s.runPageIdentityDiscovery(ctx, source.ID, identityDiscoveryIDs))
			if ctxErr := ctx.Err(); ctxErr != nil {
				err := fmt.Errorf("sync canceled during identity discovery: %w", ctxErr)
				s.failStoppedSync(syncID, err)
				return nil, err
			}

			// Report progress
			s.progress.OnProgress(checkpoint.MessagesProcessed, checkpoint.MessagesAdded, summary.MessagesSkipped)

			// Save checkpoint
			pageToken = historyResp.NextPageToken
			checkpoint.PageToken = pageToken
			if err := s.store.UpdateSyncCheckpoint(syncID, checkpoint); err != nil {
				s.logger.Warn("failed to save checkpoint", "error", err)
			}

			// No more pages
			if pageToken == "" {
				break
			}
		}
	}

	// Debt parked by an earlier page of this run is settled now, provided the
	// run showed discovery recovering.
	if discoveryHealth.shouldDrainAtCompletion() {
		s.drainIdentityDiscoveryBacklog(ctx, source.ID)
	}

	// Advance the cursor when best-effort raw fetch errors have durable replay
	// debt. Metadata refresh failures return above with the source cursor intact.
	historyIDStr := strconv.FormatUint(profile.HistoryID, 10)
	if checkpoint.ErrorsCount > 0 {
		s.logger.Warn("incremental sync completed with errors",
			"errors", checkpoint.ErrorsCount,
			"history_id", historyIDStr)
	}
	// Mark sync complete before running best-effort provider maintenance.
	mailboxChanged := startHistoryID < profile.HistoryID || checkpoint.MessagesAdded > 0
	if err := s.completeSyncAndRunHook(ctx, syncID, historyIDStr, source, mailboxChanged); err != nil {
		return nil, err
	}

	// Build summary
	summary.EndTime = time.Now()
	summary.Duration = summary.EndTime.Sub(summary.StartTime)
	summary.MessagesFound = checkpoint.MessagesProcessed
	summary.MessagesAdded = checkpoint.MessagesAdded
	summary.MessagesUpdated = checkpoint.MessagesUpdated
	summary.Errors = checkpoint.ErrorsCount
	summary.FinalHistoryID = profile.HistoryID

	s.progress.OnProgress(checkpoint.MessagesProcessed, checkpoint.MessagesAdded, summary.MessagesSkipped)
	s.progress.OnComplete(summary)
	return summary, nil
}

// replayFetchFailures re-fetches the IDs a prior completed incremental run
// failed to read, skipping any that already reached the archive. Reads and
// local ingest only; a repeated failure becomes this run's fetch error and
// carries the ID to the next run.
func (s *Syncer) replayFetchFailures(
	ctx context.Context,
	syncID, sourceID int64,
	messageIDs []string,
	labelMap map[string]int64,
	checkpoint *store.Checkpoint,
	summary *gmail.SyncSummary,
) ([]string, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}

	existing, err := s.store.MessageExistsBatch(sourceID, messageIDs)
	if err != nil {
		return nil, fmt.Errorf("check replay messages: %w", err)
	}
	fetchIDs := make([]string, 0, len(messageIDs))
	for _, messageID := range messageIDs {
		if _, ok := existing[messageID]; !ok {
			fetchIDs = append(fetchIDs, messageID)
		}
	}
	if len(fetchIDs) == 0 {
		return nil, nil
	}

	batchSize := 10
	if s.opts.BatchSize > 0 {
		batchSize = s.opts.BatchSize
	}
	successfulIDs := make([]string, 0, len(fetchIDs))
	for batchStart := 0; batchStart < len(fetchIDs); batchStart += batchSize {
		batchEnd := min(batchStart+batchSize, len(fetchIDs))
		batchIDs := fetchIDs[batchStart:batchEnd]
		checkpoint.MessagesProcessed += int64(len(batchIDs))
		results, fetchErr := s.getMessagesRawBatchWithDiagnostics(ctx, batchIDs)
		if fetchErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if errors.Is(fetchErr, context.Canceled) || errors.Is(fetchErr, context.DeadlineExceeded) {
				return nil, fetchErr
			}
			s.logger.Warn("failed to batch fetch replay messages", "error", fetchErr)
			for _, messageID := range batchIDs {
				s.recordSyncItem(syncID, messageID, syncItemPhaseFetch, store.SyncRunItemStatusError, syncItemKindBatchFetchError, fetchErr)
			}
			checkpoint.ErrorsCount += int64(len(batchIDs))
			if err := s.store.UpdateSyncCheckpoint(syncID, checkpoint); err != nil {
				return nil, fmt.Errorf("checkpoint replay batch: %w", err)
			}
			continue
		}
		for i, messageID := range batchIDs {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			result := results[i]
			if result.Message == nil {
				if kind, gone := messageGoneKind(result.Err); gone {
					s.recordSyncItem(syncID, messageID, syncItemPhaseFetch, store.SyncRunItemStatusSkipped, kind, result.Err)
					summary.MessagesSkipped++
					continue
				}
				errMsg := syncItemErrorMessage(result.Err, errRawBatchMissing.Error())
				err := errors.New(errMsg)
				s.recordSyncItem(syncID, messageID, syncItemPhaseFetch, store.SyncRunItemStatusError, syncItemKindFetchError, err)
				checkpoint.ErrorsCount++
				continue
			}

			if _, err := s.ingestMessage(ctx, sourceID, result.Message, result.Message.ThreadID, labelMap); err != nil {
				if errors.Is(err, store.ErrSyncRunSuperseded) || ctx.Err() != nil {
					return nil, err
				}
				s.recordSyncItem(syncID, messageID, syncItemPhaseIngest, store.SyncRunItemStatusError, syncItemKindIngestError, err)
				checkpoint.ErrorsCount++
				continue
			}
			checkpoint.MessagesAdded++
			summary.BytesDownloaded += int64(len(result.Message.Raw))
			successfulIDs = append(successfulIDs, messageID)
		}
		if err := s.store.UpdateSyncCheckpoint(syncID, checkpoint); err != nil {
			return nil, fmt.Errorf("checkpoint replay batch: %w", err)
		}
	}

	return successfulIDs, nil
}

// processLabelChanges handles label additions and removals for messages.
// existingMap maps source_message_id -> internal message_id for known messages.
func (s *Syncer) processLabelChanges(ctx context.Context, syncID, sourceID int64, record gmail.HistoryRecord, labelMap map[string]int64, existingMap map[string]int64, newMsgThreads map[string]string, updatedExisting, identityDiscoverySet map[string]struct{}, checkpoint *store.Checkpoint, summary *gmail.SyncSummary) error {
	for _, item := range record.LabelsAdded {
		if _, exists := existingMap[item.Message.ID]; !exists {
			if _, pending := newMsgThreads[item.Message.ID]; pending {
				continue
			}
		}
		updated, discoverable, err := s.handleLabelChange(ctx, syncID, sourceID, item.Message.ID, item.Message.ThreadID, item.LabelIDs, labelMap, true, existingMap, checkpoint, summary)
		if err != nil {
			if errors.Is(err, errGmailMetadataRefresh) && !isGmailNotFound(err) {
				s.recordSyncItem(syncID, item.Message.ID, syncItemPhaseFetch, store.SyncRunItemStatusError, syncItemKindMetadataRefreshError, err)
				checkpoint.ErrorsCount++
				return fmt.Errorf("refresh metadata for message %s: %w", item.Message.ID, err)
			}
			s.logLabelChangeError("add", item.Message.ID, err)
			continue
		}
		if discoverable {
			identityDiscoverySet[item.Message.ID] = struct{}{}
		}
		if updated {
			updatedExisting[item.Message.ID] = struct{}{}
		}
	}
	for _, item := range record.LabelsRemoved {
		updated, discoverable, err := s.handleLabelChange(ctx, syncID, sourceID, item.Message.ID, item.Message.ThreadID, item.LabelIDs, labelMap, false, existingMap, checkpoint, summary)
		if err != nil {
			if errors.Is(err, errGmailMetadataRefresh) && !isGmailNotFound(err) {
				s.recordSyncItem(syncID, item.Message.ID, syncItemPhaseFetch, store.SyncRunItemStatusError, syncItemKindMetadataRefreshError, err)
				checkpoint.ErrorsCount++
				return fmt.Errorf("refresh metadata for message %s: %w", item.Message.ID, err)
			}
			s.logLabelChangeError("remove", item.Message.ID, err)
			continue
		}
		if discoverable {
			identityDiscoverySet[item.Message.ID] = struct{}{}
		}
		if updated {
			updatedExisting[item.Message.ID] = struct{}{}
		}
	}
	return nil
}

// handleLabelChange processes a label addition or removal.
// For existing messages, applies the label diff directly without any API calls.
// For unknown messages with labels being added, fetches and ingests the message.
func (s *Syncer) handleLabelChange(ctx context.Context, syncID, sourceID int64, messageID, threadID string, gmailLabelIDs []string, labelMap map[string]int64, isAdd bool, existingMap map[string]int64, checkpoint *store.Checkpoint, summary *gmail.SyncSummary) (bool, bool, error) {
	internalID, exists := existingMap[messageID]

	if !exists {
		// Message doesn't exist locally - if adding labels, we should fetch it
		if isAdd {
			checkpoint.MessagesProcessed++
			raw, err := s.client.GetMessageRaw(ctx, messageID)
			if err != nil {
				if isGmailNotFound(err) {
					s.recordSyncItem(syncID, messageID, syncItemPhaseFetch, store.SyncRunItemStatusSkipped, syncItemKindGmailNotFound, err)
					return false, false, err
				}
				s.recordSyncItem(syncID, messageID, syncItemPhaseFetch, store.SyncRunItemStatusError, syncItemKindFetchError, err)
				checkpoint.ErrorsCount++
				return false, false, err
			}
			if _, err := s.ingestLiveMessage(
				ctx, sourceID, raw, threadID, labelMap,
			); err != nil {
				s.recordSyncItem(syncID, messageID, syncItemPhaseIngest, store.SyncRunItemStatusError, syncItemKindIngestError, err)
				checkpoint.ErrorsCount++
				return false, false, err
			}
			// The new message gets embed_gen = NULL by column default, so
			// the scan-and-fill embed worker picks it up automatically — no
			// sync-time enqueue step is needed.
			checkpoint.MessagesAdded++
			if raw != nil {
				summary.BytesDownloaded += int64(len(raw.Raw))
			}
			return false, true, nil
		}
		// Removing labels from non-existent message is a no-op
		return false, false, nil
	}

	// A current metadata read subsumes the history delta and also refreshes
	// provider inbox/read markers without downloading the message body.
	if s.opts.SourceType == "" || s.opts.SourceType == sourceTypeGmail {
		if reader, ok := s.client.(messageLabelBatchReader); ok {
			observedAt := time.Now().UTC()
			results, err := reader.GetMessageLabelsBatch(ctx, []string{messageID})
			if err != nil {
				return false, false, fmt.Errorf("%w: %w", errGmailMetadataRefresh, err)
			}
			if len(results) != 1 || results[0].ID != messageID {
				return false, false, fmt.Errorf("%w: gmail label metadata identity is unavailable", errGmailMetadataRefresh)
			}
			if results[0].Err != nil {
				if isGmailNotFound(results[0].Err) {
					return false, false, results[0].Err
				}
				return false, false, fmt.Errorf("%w: %w", errGmailMetadataRefresh, results[0].Err)
			}
			metadata := results[0]
			changed, err := s.store.RefreshGmailInboxLabelsContext(ctx, sourceID, internalID, messageID, labelIDsFor(metadata.LabelIDs, labelMap), store.GmailInboxObservation{Tags: metadata.LabelIDs, HistoryID: metadata.HistoryID, ObservedAt: observedAt})
			if err != nil {
				return false, false, fmt.Errorf("%w: %w", errGmailMetadataRefresh, err)
			}
			return changed, changed, nil
		}
	}
	// Convert Gmail label IDs to internal label IDs
	var labelIDs []int64
	for _, gmailID := range gmailLabelIDs {
		if id, ok := labelMap[gmailID]; ok {
			labelIDs = append(labelIDs, id)
		}
	}

	// Apply label diff directly — no API call needed
	if isAdd {
		if err := s.store.AddMessageLabels(internalID, labelIDs); err != nil {
			return false, false, err
		}
		return true, true, nil
	}
	if err := s.store.RemoveMessageLabels(internalID, labelIDs); err != nil {
		return false, false, err
	}
	return true, true, nil
}

// logLabelChangeError logs label change errors, downgrading "not found"
// to a debug-level message since deleted messages are expected during
// incremental sync (e.g., spam auto-deleted between sync runs).
func (s *Syncer) logLabelChangeError(action, messageID string, err error) {
	if _, ok := errors.AsType[*gmail.NotFoundError](err); ok {
		s.logger.Debug("skipping label "+action+": message deleted from Gmail", "id", messageID)
	} else {
		s.logger.Warn("failed to handle label "+action, "id", messageID, "error", err)
	}
}

// ingestLiveMessage is restricted to messages obtained from the current
// history delta. Recovery of earlier fetch failures keeps unknown provenance.
func (s *Syncer) ingestLiveMessage(ctx context.Context, sourceID int64, raw *gmail.RawMessage, threadID string, labelMap map[string]int64) (bool, error) {
	scoped := *s
	scoped.store = s.store.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: time.Now().UTC()})
	return scoped.ingestMessage(ctx, sourceID, raw, threadID, labelMap)
}
