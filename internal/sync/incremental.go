package sync

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

// Incremental performs an incremental sync using the Gmail History API.
// Falls back to full sync if history is too old (404 error).
//
// The caller must resolve the correct *store.Source before calling this
// method. This avoids ambiguity when multiple sources share the same
// identifier (e.g. a Gmail and IMAP source for the same email address).
func (s *Syncer) Incremental(ctx context.Context, source *store.Source) (summary *gmail.SyncSummary, err error) {
	if source == nil {
		return nil, errors.New("no source provided - run full sync first")
	}

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

	// Start sync
	syncID, err := s.store.StartSync(source.ID, "incremental")
	if err != nil {
		return nil, fmt.Errorf("start sync: %w", err)
	}

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

	s.logger.Info("incremental sync", "email", source.Identifier, "start_history", startHistoryID, "current_history", profile.HistoryID)

	// If history IDs match, nothing to do
	if startHistoryID >= profile.HistoryID {
		s.logger.Info("already up to date")
		_ = s.store.CompleteSync(syncID, strconv.FormatUint(profile.HistoryID, 10))
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

	// Process history
	checkpoint := &store.Checkpoint{}
	pageToken := ""

	for {
		historyResp, err := s.client.ListHistory(ctx, startHistoryID, pageToken)
		if err != nil {
			// Check for 404 - history too old
			var notFound *gmail.NotFoundError
			if errors.As(err, &notFound) {
				s.logger.Warn("history too old, falling back to full sync")
				_ = s.store.FailSync(syncID, "history too old")
				// Caller should trigger full sync
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

		for _, record := range historyResp.History {
			for _, msg := range record.MessagesAdded {
				if _, exists := existingMap[msg.Message.ID]; !exists {
					newMsgThreads[msg.Message.ID] = msg.Message.ThreadID
				}
			}
			for _, msg := range record.MessagesDeleted {
				deletedSet[msg.Message.ID] = true
			}
			s.processLabelChanges(ctx, source.ID, record, labelMap, existingMap, updatedExisting)
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
			rawMessages, fetchErr := s.client.GetMessagesRawBatch(ctx, newMsgIDs)
			if fetchErr != nil {
				s.logger.Warn("failed to batch fetch messages", "error", fetchErr)
				checkpoint.ErrorsCount += int64(len(newMsgIDs))
			} else {
				var insertedIDs []int64
				for i, raw := range rawMessages {
					if raw == nil {
						s.logger.Warn("failed to fetch message (nil response)", "id", newMsgIDs[i])
						checkpoint.ErrorsCount++
						continue
					}
					threadID := newMsgThreads[newMsgIDs[i]]
					insertedID, err := s.ingestMessage(ctx, source.ID, raw, threadID, labelMap)
					if err != nil {
						s.logger.Warn("failed to ingest added message", "id", newMsgIDs[i], "error", err)
						checkpoint.ErrorsCount++
						continue
					}
					if insertedID > 0 {
						insertedIDs = append(insertedIDs, insertedID)
					}
					checkpoint.MessagesAdded++
					summary.BytesDownloaded += int64(len(raw.Raw))
				}

				// Hook vector-search enqueue. Non-fatal on failure: missed
				// IDs get picked up by full-rebuild.
				if s.embedEnqueuer != nil && len(insertedIDs) > 0 {
					if err := s.embedEnqueuer.EnqueueMessages(ctx, insertedIDs); err != nil {
						s.logger.Warn("vector enqueue failed", "ids", len(insertedIDs), "error", err)
					}
				}
			}
		}

		// Batch-mark deleted messages
		if len(deletedIDs) > 0 {
			if err := s.store.MarkMessagesDeletedBatch(source.ID, deletedIDs); err != nil {
				s.logger.Warn("failed to batch mark messages deleted", "error", err)
				checkpoint.ErrorsCount += int64(len(deletedIDs))
			}
		}

		// Report progress
		s.progress.OnProgress(checkpoint.MessagesProcessed, checkpoint.MessagesAdded, 0)

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

	// Always advance the cursor so a single permanently-failing
	// message doesn't block all future incremental syncs.
	// Failed messages can be recovered via full sync.
	historyIDStr := strconv.FormatUint(profile.HistoryID, 10)
	if checkpoint.ErrorsCount > 0 {
		s.logger.Warn("incremental sync completed with errors",
			"errors", checkpoint.ErrorsCount,
			"history_id", historyIDStr)
	}
	if err := s.store.UpdateSourceSyncCursor(source.ID, historyIDStr); err != nil {
		s.logger.Warn("failed to update sync cursor", "error", err)
	}

	// Mark sync complete
	if err := s.store.CompleteSync(syncID, historyIDStr); err != nil {
		s.logger.Warn("failed to complete sync", "error", err)
	}

	// Build summary
	summary.EndTime = time.Now()
	summary.Duration = summary.EndTime.Sub(summary.StartTime)
	summary.MessagesFound = checkpoint.MessagesProcessed
	summary.MessagesAdded = checkpoint.MessagesAdded
	summary.MessagesUpdated = checkpoint.MessagesUpdated
	summary.Errors = checkpoint.ErrorsCount
	summary.FinalHistoryID = profile.HistoryID

	s.progress.OnComplete(summary)
	return summary, nil
}

// processLabelChanges handles label additions and removals for messages.
// existingMap maps source_message_id -> internal message_id for known messages.
func (s *Syncer) processLabelChanges(ctx context.Context, sourceID int64, record gmail.HistoryRecord, labelMap map[string]int64, existingMap map[string]int64, updatedExisting map[string]struct{}) {
	for _, item := range record.LabelsAdded {
		updated, err := s.handleLabelChange(ctx, sourceID, item.Message.ID, item.Message.ThreadID, item.LabelIDs, labelMap, true, existingMap)
		if err != nil {
			s.logLabelChangeError("add", item.Message.ID, err)
			continue
		}
		if updated {
			updatedExisting[item.Message.ID] = struct{}{}
		}
	}
	for _, item := range record.LabelsRemoved {
		updated, err := s.handleLabelChange(ctx, sourceID, item.Message.ID, item.Message.ThreadID, item.LabelIDs, labelMap, false, existingMap)
		if err != nil {
			s.logLabelChangeError("remove", item.Message.ID, err)
			continue
		}
		if updated {
			updatedExisting[item.Message.ID] = struct{}{}
		}
	}
}

// handleLabelChange processes a label addition or removal.
// For existing messages, applies the label diff directly without any API calls.
// For unknown messages with labels being added, fetches and ingests the message.
func (s *Syncer) handleLabelChange(ctx context.Context, sourceID int64, messageID, threadID string, gmailLabelIDs []string, labelMap map[string]int64, isAdd bool, existingMap map[string]int64) (bool, error) {
	internalID, exists := existingMap[messageID]

	if !exists {
		// Message doesn't exist locally - if adding labels, we should fetch it
		if isAdd {
			raw, err := s.client.GetMessageRaw(ctx, messageID)
			if err != nil {
				return false, err
			}
			insertedID, err := s.ingestMessage(ctx, sourceID, raw, threadID, labelMap)
			if err != nil {
				return false, err
			}
			// Hook vector-search enqueue for the new message.
			if s.embedEnqueuer != nil && insertedID > 0 {
				if err := s.embedEnqueuer.EnqueueMessages(ctx, []int64{insertedID}); err != nil {
					s.logger.Warn("vector enqueue failed", "ids", 1, "error", err)
				}
			}
			return false, nil
		}
		// Removing labels from non-existent message is a no-op
		return false, nil
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
		return true, s.store.AddMessageLabels(internalID, labelIDs)
	}
	return true, s.store.RemoveMessageLabels(internalID, labelIDs)
}

// logLabelChangeError logs label change errors, downgrading "not found"
// to a debug-level message since deleted messages are expected during
// incremental sync (e.g., spam auto-deleted between sync runs).
func (s *Syncer) logLabelChangeError(action, messageID string, err error) {
	var notFound *gmail.NotFoundError
	if errors.As(err, &notFound) {
		s.logger.Debug("skipping label "+action+": message deleted from Gmail", "id", messageID)
	} else {
		s.logger.Warn("failed to handle label "+action, "id", messageID, "error", err)
	}
}
