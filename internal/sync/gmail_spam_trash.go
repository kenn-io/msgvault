package sync

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

// gmailSpamTrashQuery lists the messages the filtered acquisition listing omits.
const gmailSpamTrashQuery = "in:spam OR in:trash"

func (s *Syncer) listAcquisitionMessages(ctx context.Context, pageToken string) (*gmail.MessageListResponse, error) {
	if !s.opts.GmailSkipSpamTrash {
		return s.client.ListMessages(ctx, s.opts.Query, pageToken)
	}
	reader, ok := s.client.(gmail.SpamTrashExcludingLister)
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
	s.recordSyncItem(syncID, messageID, syncItemPhaseIngest, store.SyncRunItemStatusSkipped, syncItemKindExcludedGmailLabel, nil)
	return true
}

// refreshArchivedSpamTrashLabels re-reads labels for archived messages now in
// Spam or Trash. Filtered recovery enumeration omits them, and History from
// before the recovery boundary is lost, so this is the only way recovery sees
// a move into either folder. New messages in the listing are not acquired.
func (s *Syncer) refreshArchivedSpamTrashLabels(ctx context.Context, state *syncState, sourceID int64, labelMap map[string]int64, summary *gmail.SyncSummary) error {
	pageToken := ""
	for {
		listResp, err := s.client.ListMessages(ctx, gmailSpamTrashQuery, pageToken)
		if err != nil {
			return fmt.Errorf("list Gmail Spam and Trash: %w", err)
		}
		ids := make([]string, len(listResp.Messages))
		for i, message := range listResp.Messages {
			ids[i] = message.ID
		}
		existing, err := s.store.MessageExistsBatchContext(ctx, sourceID, ids)
		if err != nil {
			return fmt.Errorf("check archived Gmail Spam and Trash: %w", err)
		}
		archived := &gmail.MessageListResponse{}
		rows := make(map[string]store.MessageWithRawMetadata, len(existing))
		for _, message := range listResp.Messages {
			if rowID, ok := existing[message.ID]; ok {
				rows[message.ID] = store.MessageWithRawMetadata{ID: rowID}
				archived.Messages = append(archived.Messages, message)
			}
		}
		result, err := s.processBatchWithArchivedRows(ctx, state.syncID, sourceID, archived, labelMap, state.checkpoint, summary, rows)
		if err != nil {
			return fmt.Errorf("refresh archived Gmail Spam and Trash labels: %w", err)
		}
		state.checkpoint.MessagesUpdated += result.updated
		if listResp.NextPageToken == "" {
			return nil
		}
		pageToken = listResp.NextPageToken
	}
}
