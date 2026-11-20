package sync

import (
	"context"
	"errors"
	"slices"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

type gmailFilteredMessageLister interface {
	ListMessagesExcludingSpamTrash(ctx context.Context, query, pageToken string) (*gmail.MessageListResponse, error)
}

func (s *Syncer) listAcquisitionMessages(ctx context.Context, pageToken string) (*gmail.MessageListResponse, error) {
	if !s.opts.GmailSkipSpamTrash {
		return s.client.ListMessages(ctx, s.opts.Query, pageToken)
	}
	reader, ok := s.client.(gmailFilteredMessageLister)
	if !ok {
		return nil, errors.New("filtered Gmail message listing is unavailable")
	}
	return reader.ListMessagesExcludingSpamTrash(ctx, s.opts.Query, pageToken)
}

func hasGmailSpamOrTrash(labels []string) bool {
	return slices.Contains(labels, "SPAM") || slices.Contains(labels, "TRASH")
}

func (s *Syncer) skipGmailAcquisition(syncID int64, messageID string, labels []string) bool {
	if !s.opts.GmailSkipSpamTrash || !hasGmailSpamOrTrash(labels) {
		return false
	}
	s.recordSyncItem(syncID, messageID, syncItemPhaseIngest, store.SyncRunItemStatusSkipped, "excluded_gmail_label", nil)
	return true
}
