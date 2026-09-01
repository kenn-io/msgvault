package sync

import (
	"context"
	"errors"

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
)

var errRawBatchMissing = errors.New("missing raw message in batch result")

type rawBatchWithErrors interface {
	GetMessagesRawBatchWithErrors(ctx context.Context, messageIDs []string) ([]gmail.RawMessageBatchResult, error)
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
