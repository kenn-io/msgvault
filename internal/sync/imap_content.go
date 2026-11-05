package sync

import (
	"context"
	"encoding/json/v2"
	"fmt"
)

type imapContentOrigin string

const (
	imapContentSent     imapContentOrigin = "sent"
	imapContentDrafts   imapContentOrigin = "drafts"
	imapContentOutgoing imapContentOrigin = "outgoing"
)

// Content origin belongs to the saved snapshot, not its current mailbox.
// Location-only adoption leaves this metadata intact, including across runs
// where the original mailbox no longer appears in LIST.
type imapMessageMetadata struct {
	ContentOrigin  imapContentOrigin `json:"imap_content_origin"`
	EmailInReplyTo string            `json:"email_in_reply_to,omitempty"`
}

func (s *Syncer) imapContentOrigin(sourceMessageID string) imapContentOrigin {
	if !s.relocationContentAuthorized(sourceMessageID) {
		return ""
	}
	if placement, ok := s.client.(sentPrecedencePlacement); ok {
		if placement.IsSentPlacementMailbox(sourceMessageID) {
			return imapContentSent
		}
		if placement.IsDraftsPlacementMailbox(sourceMessageID) {
			return imapContentDrafts
		}
	}
	return imapContentOutgoing
}

func (s *Syncer) savedIMAPContentOrigin(ctx context.Context, messageID int64) (imapContentOrigin, error) {
	metadata, err := s.store.GetMessageMetadataContext(ctx, messageID)
	if err != nil {
		return "", fmt.Errorf("get IMAP content origin: %w", err)
	}
	var saved imapMessageMetadata
	if metadata.Valid {
		if err := json.Unmarshal([]byte(metadata.String), &saved); err != nil {
			return "", fmt.Errorf("decode IMAP content origin: %w", err)
		}
	}
	return saved.ContentOrigin, nil
}

func (destination imapContentOrigin) canRefresh(saved imapContentOrigin, canonicalExists bool) bool {
	if destination == "" || (saved == imapContentSent && destination == imapContentDrafts) {
		return false
	}
	if canonicalExists {
		// Existing outgoing snapshots only yield to Sent-over-Drafts.
		return saved == "" || (destination == imapContentSent && saved == imapContentDrafts)
	}
	// Once the canonical disappears, equal-priority outgoing survivors can
	// refresh the snapshot, including Sent-to-Sent and Drafts-to-Drafts.
	return true
}
