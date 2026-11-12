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
// A message already archived is left as it is: a delayed or repeated event
// carries its first text, and polling keeps the current text. Messages
// without text, reactions, edits, deletions, reads and other messaging
// events are ignored.
//
// It trusts the signed payload instead of reading the message from Graph, so
// it refuses, with ErrInvalidChange, a message that is not between the inbox
// owner and another account, or that has no timestamp.
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
		if event.Message == nil || event.Message.IsDeleted || event.Message.Text == "" {
			continue
		}
		fromOwner, toOwner := event.Sender.ID == b.Owner(), event.Recipient.ID == b.Owner()
		if fromOwner == toOwner {
			return fmt.Errorf("%w: message is not between the inbox owner and another account", ErrInvalidChange)
		}
		if event.Timestamp <= 0 {
			return fmt.Errorf("%w: message has no timestamp", ErrInvalidChange)
		}
		customer := event.Sender
		if fromOwner {
			customer = event.Recipient
		}
		if !safeObjectID(event.Message.Mid) || !safeObjectID(customer.ID) {
			return fmt.Errorf("%w: invalid message identity", ErrInvalidChange)
		}
		items = append(items, item{customer, InboxMessage{
			ID: event.Message.Mid, CreatedTime: time.UnixMilli(event.Timestamp).UTC().Format(time.RFC3339Nano),
			From: InboxParticipant{ID: event.Sender.ID}, Message: event.Message.Text,
		}})
	}
	if len(items) == 0 {
		return nil
	}
	return applyWebhook(ctx, st, b.SourceType(), b.Identifier(), func(st *store.Store, source *store.Source, sum *ImportSummary) error {
		for _, it := range items {
			archived, err := st.MessageExistsBatchContext(ctx, source.ID, []string{it.message.ID})
			if err != nil {
				return err
			}
			if archived[it.message.ID] != 0 {
				continue
			}
			if err := b.persistMessage(ctx, st, source.ID, sum, InboxParticipant{ID: b.Owner()}, it.customer, it.message); err != nil {
				return fmt.Errorf("message %q: %w", it.message.ID, err)
			}
		}
		return nil
	})
}
