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

	syncItemKindBatchFetchError               = "batch_fetch_error"
	syncItemKindFetchError                    = "fetch_error"
	syncItemKindGmailNotFound                 = "gmail_not_found"
	syncItemKindMessageGone                   = "message_gone"
	syncItemKindIngestError                   = "ingest_error"
	syncItemKindAttachmentError               = "attachment_error"
	syncItemKindRemoteImageError              = "remote_image_error"
	syncItemKindAttachmentAndRemoteImageError = "attachment_remote_image_error"
	syncItemKindDeleteError                   = "delete_error"
	syncItemKindAcquired                      = "acquired"
)

// AcquisitionFailureDescription describes an acquisition error kind for users.
func AcquisitionFailureDescription(kind string) string {
	switch kind {
	case syncItemKindFetchError, syncItemKindBatchFetchError:
		return "message download failed"
	case syncItemKindIngestError:
		return "message processing failed"
	case syncItemKindAttachmentError:
		return "attachment saving failed"
	case syncItemKindRemoteImageError:
		return "remote image saving failed"
	case syncItemKindAttachmentAndRemoteImageError:
		return "attachment and remote image saving failed"
	default:
		return "message acquisition failed"
	}
}

// IsAcquisitionReplayCandidate reports whether a failed fetch or ingest names
// a usable Gmail message ID.
func IsAcquisitionReplayCandidate(item store.SyncRunItem) bool {
	return ((item.Phase == syncItemPhaseFetch &&
		(item.ErrorKind == syncItemKindFetchError || item.ErrorKind == syncItemKindBatchFetchError)) ||
		(item.Phase == syncItemPhaseIngest && (item.ErrorKind == syncItemKindIngestError || materializationFromKind(item.ErrorKind) != 0))) &&
		item.SourceMessageID != "" && item.SourceMessageID != "(unknown)"
}

var (
	errRawBatchMissing           = errors.New("missing raw message in batch result")
	errAcquisitionFailureRecord  = errors.New("record acquisition failure")
	errAttachmentMaterialization = errors.New("save attachments")
	errRemoteImageArchival       = errors.New("archive remote images")
)

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

	s.pendingAttachmentRepairs = make(map[string]materializationWork)
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if !IsAcquisitionReplayCandidate(item) {
			continue
		}
		ids = append(ids, item.SourceMessageID)
		if work := materializationFromKind(item.ErrorKind); work != 0 {
			s.pendingAttachmentRepairs[item.SourceMessageID] = work
		}
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
	if s.pendingAttachmentRepairs[messageID] != 0 {
		return errors.New("cannot settle acquisition while materialization remains pending")
	}
	return s.settleAcquisition(messageID)
}

// Only callers that finished every required post-commit operation may settle
// a materialization obligation. A failed ledger write leaves both maps intact.
func (s *Syncer) settleAcquisition(messageID string) error {
	if item := s.acquisitionResolution(messageID); item != nil {
		if err := s.store.RecordSyncRunItem(*item); err != nil {
			return fmt.Errorf("resolve acquisition %q: %w", messageID, err)
		}
		delete(s.pendingAcquisitions, messageID)
		delete(s.pendingAttachmentRepairs, messageID)
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

// recordAcquisitionFailure must succeed before Gmail can advance past an
// acquisition. Callers must stop on error: even completing an idle incremental
// run hides older retry debt behind ListUnresolvedSyncItemErrors' lower bound.
func (s *Syncer) recordAcquisitionFailure(syncID int64, sourceMessageID, phase, kind string, acquisitionErr error) error {
	work := s.pendingAttachmentRepairs[sourceMessageID]
	if errors.Is(acquisitionErr, errAttachmentMaterialization) {
		work |= materializeMIME
	}
	if errors.Is(acquisitionErr, errRemoteImageArchival) {
		work |= archiveRemoteImages
	}
	if work != 0 {
		phase, kind = syncItemPhaseIngest, materializationKind(work)
	}
	if s.opts.SourceType != "" && s.opts.SourceType != sourceTypeGmail {
		s.recordSyncItem(syncID, sourceMessageID, phase, store.SyncRunItemStatusError, kind, acquisitionErr)
		return nil
	}
	if err := s.writeSyncItem(syncID, sourceMessageID, phase, store.SyncRunItemStatusError, kind, acquisitionErr); err != nil {
		return fmt.Errorf("%w for Gmail message %q (%s): %w", errAcquisitionFailureRecord, sourceMessageID, phase, err)
	}
	return nil
}

func (s *Syncer) writeSyncItem(syncID int64, sourceMessageID, phase, status, kind string, err error) error {
	// Fetch outcomes, including a later 404, cannot supersede an obligation
	// for content that already committed. Only explicit settlement can do so.
	if work := s.pendingAttachmentRepairs[sourceMessageID]; work != 0 && status != store.SyncRunItemStatusResolved {
		phase, status, kind = syncItemPhaseIngest, store.SyncRunItemStatusError, materializationKind(work|materializationFromKind(kind))
	}
	if s.pendingAcquisitions != nil && syncID == s.acquisitionSyncID && status == store.SyncRunItemStatusError &&
		IsAcquisitionReplayCandidate(store.SyncRunItem{SourceMessageID: sourceMessageID, Phase: phase, ErrorKind: kind}) {
		s.pendingAcquisitions[sourceMessageID] = struct{}{}
		if work := materializationFromKind(kind); work != 0 {
			if s.pendingAttachmentRepairs == nil {
				s.pendingAttachmentRepairs = make(map[string]materializationWork)
			}
			s.pendingAttachmentRepairs[sourceMessageID] |= work
		}
	}
	if sourceMessageID == "" {
		sourceMessageID = "(unknown)"
	}
	errMsg := syncItemErrorMessage(err, kind)
	return s.store.RecordSyncRunItem(store.SyncRunItem{
		SyncRunID:       syncID,
		SourceMessageID: sourceMessageID,
		Phase:           phase,
		Status:          status,
		ErrorKind:       kind,
		ErrorMessage:    errMsg,
	})
}

func (s *Syncer) recordSyncItem(syncID int64, sourceMessageID, phase, status, kind string, err error) {
	if sourceMessageID == "" {
		sourceMessageID = "(unknown)"
	}
	if recErr := s.writeSyncItem(syncID, sourceMessageID, phase, status, kind, err); recErr != nil {
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
