package meta

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// messagingEvent is one entry.messaging item of a Messenger or Instagram
// webhook delivery.
type messagingEvent struct {
	Sender    InboxParticipant `json:"sender"`
	Recipient InboxParticipant `json:"recipient"`
	Timestamp int64            `json:"timestamp"`
	Message   *struct {
		Mid       string `json:"mid"`
		Text      string `json:"text"`
		IsDeleted bool   `json:"is_deleted"`
	} `json:"message"`
}

// ApplyInboxMessages archives new messages, and the inbox owner's own sent
// messages (echoes), from messaging webhook events into the inbox's chats.
// Reactions, edits, deletions, reads and other messaging events are ignored.
func ApplyInboxMessages(ctx context.Context, st *store.Store, b Inbox, values []jsontext.Value) error {
	type item struct {
		customer InboxParticipant
		message  InboxMessage
	}
	var items []item
	for _, value := range values {
		var event messagingEvent
		if err := json.Unmarshal(value, &event); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidChange, err)
		}
		if event.Message == nil || event.Message.IsDeleted {
			continue
		}
		customer := event.Sender
		if event.Sender.ID == b.Owner() {
			customer = event.Recipient
		}
		if !safeObjectID(event.Message.Mid) || !safeObjectID(customer.ID) || customer.ID == b.Owner() {
			return fmt.Errorf("%w: invalid message identity", ErrInvalidChange)
		}
		items = append(items, item{customer, InboxMessage{
			ID: event.Message.Mid, CreatedTime: time.UnixMilli(event.Timestamp).UTC().Format(time.RFC3339),
			From: InboxParticipant{ID: event.Sender.ID}, Message: event.Message.Text,
		}})
	}
	if len(items) == 0 {
		return nil
	}
	return applyWebhook(ctx, st, b.SourceType(), b.Identifier(), func(st *store.Store, source *store.Source, sum *ImportSummary) error {
		for _, it := range items {
			if err := b.persistMessage(ctx, st, source.ID, sum, InboxParticipant{ID: b.Owner()}, it.customer, it.message); err != nil {
				return fmt.Errorf("message %q: %w", it.message.ID, err)
			}
		}
		return nil
	})
}
