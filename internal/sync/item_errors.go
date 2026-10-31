package sync

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

const (
	syncItemPhaseFetch  = "fetch"
	syncItemPhaseIngest = "ingest"
	syncItemPhaseDelete = "delete"

	syncItemKindBatchFetchError = "batch_fetch_error"
	syncItemKindFetchError      = "fetch_error"
	syncItemKindGmailNotFound   = "gmail_not_found"
	syncItemKindMessageGone     = "message_gone"
	syncItemKindIngestError     = "ingest_error"
	syncItemKindDeleteError     = "delete_error"
	syncItemKindAcquired        = "acquired"
)

// isAcquisitionReplayCandidate reports whether a failed fetch or ingest names
// a usable Gmail message ID.
func isAcquisitionReplayCandidate(item store.SyncRunItem) bool {
	return ((item.Phase == syncItemPhaseFetch &&
		(item.ErrorKind == syncItemKindFetchError || item.ErrorKind == syncItemKindBatchFetchError)) ||
		(item.Phase == syncItemPhaseIngest && item.ErrorKind == syncItemKindIngestError)) &&
		item.SourceMessageID != "" && item.SourceMessageID != "(unknown)"
}

var errRawBatchMissing = errors.New("missing raw message in batch result")

type rawBatchWithErrors interface {
	GetMessagesRawBatchWithErrors(ctx context.Context, messageIDs []string) ([]gmail.RawMessageBatchResult, error)
}

type rawBatchWithIdentityValidation interface {
	GetMessagesRawBatchWithIdentityValidation(ctx context.Context, messageIDs []string) ([]gmail.RawMessageBatchResult, error)
}

func (s *Syncer) getMessagesRawBatchWithDiagnostics(ctx context.Context, messageIDs []string) ([]gmail.RawMessageBatchResult, error) {
	if client, ok := s.client.(rawBatchWithErrors); ok {
		return client.GetMessagesRawBatchWithErrors(ctx, messageIDs)
	}

	rawMessages, err := s.client.GetMessagesRawBatch(ctx, messageIDs)
	if err != nil {
		return nil, err
	}

	results := make([]gmail.RawMessageBatchResult, len(messageIDs))
	for i, id := range messageIDs {
		var raw *gmail.RawMessage
		if i < len(rawMessages) {
			raw = rawMessages[i]
		}
		results[i] = gmail.RawMessageBatchResult{
			ID:      id,
			Message: raw,
		}
		if raw == nil {
			results[i].Err = errRawBatchMissing
		}
	}
	return results, nil
}

// getMessagesRawBatchWithIdentityValidation fetches validationIDs first with
// full raw MIME so their Message-IDs turn later ordinary copies into dedup
// stubs. Results keep the order of messageIDs.
func (s *Syncer) getMessagesRawBatchWithIdentityValidation(
	ctx context.Context,
	messageIDs []string,
	validationIDs map[string]inconclusiveLabelRefresh,
) ([]gmail.RawMessageBatchResult, error) {
	client, ok := s.client.(rawBatchWithIdentityValidation)
	if !ok || len(validationIDs) == 0 {
		return s.getMessagesRawBatchWithDiagnostics(ctx, messageIDs)
	}

	var requiredIDs, ordinaryIDs []string
	var requiredIdx, ordinaryIdx []int
	for i, id := range messageIDs {
		if _, needsValidation := validationIDs[id]; needsValidation {
			requiredIDs = append(requiredIDs, id)
			requiredIdx = append(requiredIdx, i)
		} else {
			ordinaryIDs = append(ordinaryIDs, id)
			ordinaryIdx = append(ordinaryIdx, i)
		}
	}

	results := make([]gmail.RawMessageBatchResult, len(messageIDs))
	required, err := client.GetMessagesRawBatchWithIdentityValidation(ctx, requiredIDs)
	if err != nil {
		return nil, err
	}
	for i, result := range required {
		results[requiredIdx[i]] = result
	}
	if len(ordinaryIDs) == 0 {
		return results, nil
	}
	ordinary, err := s.getMessagesRawBatchWithDiagnostics(ctx, ordinaryIDs)
	if err != nil {
		return nil, err
	}
	for i, result := range ordinary {
		results[ordinaryIdx[i]] = result
	}
	return results, nil
}

// pendingAcquisitionReplayIDs returns the Gmail messages whose latest fetch or
// ingest outcome since the latest completed incremental run is an error.
func (s *Syncer) pendingAcquisitionReplayIDs(ctx context.Context, sourceID int64) ([]string, error) {
	items, err := s.store.ListUnresolvedSyncItemErrors(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("list unresolved acquisition errors: %w", err)
	}

	ids := make([]string, 0, len(items))
	for _, item := range items {
		if !isAcquisitionReplayCandidate(item) {
			continue
		}
		ids = append(ids, item.SourceMessageID)
	}
	sort.Strings(ids)
	return ids, nil
}

// initAcquisitionReplay snapshots earlier failures for this run.
func (s *Syncer) initAcquisitionReplay(ctx context.Context, sourceID, syncID int64) error {
	ids, err := s.pendingAcquisitionReplayIDs(ctx, sourceID)
	if err != nil {
		return err
	}
	s.setAcquisitionReplay(syncID, ids)
	return nil
}

func (s *Syncer) setAcquisitionReplay(syncID int64, ids []string) {
	s.acquisitionSyncID = syncID
	s.acquisitionRecordErr = nil
	s.pendingAcquisitions = make(map[string]struct{}, len(ids))
	for _, id := range ids {
		s.pendingAcquisitions[id] = struct{}{}
	}
}

func (s *Syncer) acquisitionResolution(messageID string) *store.SyncRunItem {
	if _, pending := s.pendingAcquisitions[messageID]; !pending {
		return nil
	}
	return &store.SyncRunItem{
		SyncRunID:       s.acquisitionSyncID,
		SourceMessageID: messageID,
		Phase:           syncItemPhaseFetch,
		Status:          store.SyncRunItemStatusResolved,
		ErrorKind:       syncItemKindAcquired,
		ErrorMessage:    syncItemKindAcquired,
	}
}

func (s *Syncer) resolveAcquisition(messageID string) error {
	if item := s.acquisitionResolution(messageID); item != nil {
		if err := s.store.RecordSyncRunItem(*item); err != nil {
			return fmt.Errorf("resolve acquisition %q: %w", messageID, err)
		}
		delete(s.pendingAcquisitions, messageID)
	}
	return nil
}

func isGmailNotFound(err error) bool {
	var notFound *gmail.NotFoundError
	return errors.As(err, &notFound)
}

// messageGoneKind reports whether the source no longer holds the message, and
// the sync-item kind that records why. A Gmail 404 and an IMAP UID expunged
// between enumeration and fetch mean the same thing to the run: the message was
// handled, not failed. Both must be acknowledged, or the folder they belong to
// never saves its high water mark.
func messageGoneKind(err error) (string, bool) {
	switch {
	case isGmailNotFound(err):
		return syncItemKindGmailNotFound, true
	case errors.Is(err, gmail.ErrMessageGone):
		return syncItemKindMessageGone, true
	default:
		return "", false
	}
}

func syncItemErrorMessage(err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	return fallback
}

func (s *Syncer) recordSyncItem(syncID int64, sourceMessageID, phase, status, kind string, err error) {
	retryable := s.pendingAcquisitions != nil && syncID == s.acquisitionSyncID && status == store.SyncRunItemStatusError &&
		isAcquisitionReplayCandidate(store.SyncRunItem{SourceMessageID: sourceMessageID, Phase: phase, ErrorKind: kind})
	if retryable {
		s.pendingAcquisitions[sourceMessageID] = struct{}{}
	}
	if sourceMessageID == "" {
		sourceMessageID = "(unknown)"
	}
	errMsg := syncItemErrorMessage(err, kind)
	if recErr := s.store.RecordSyncRunItem(store.SyncRunItem{
		SyncRunID:       syncID,
		SourceMessageID: sourceMessageID,
		Phase:           phase,
		Status:          status,
		ErrorKind:       kind,
		ErrorMessage:    errMsg,
	}); recErr != nil {
		if retryable && s.acquisitionRecordErr == nil {
			s.acquisitionRecordErr = fmt.Errorf("record retry for Gmail message %q: %w", sourceMessageID, recErr)
		}
		s.logger.Warn("failed to record sync item",
			"sync_id", syncID,
			"source_message_id", sourceMessageID,
			"phase", phase,
			"status", status,
			"error_kind", kind,
			"error", recErr,
		)
	}
}
