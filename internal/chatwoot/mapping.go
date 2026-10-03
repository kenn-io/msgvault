package chatwoot

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/store"
)

func messageBody(m Message) string {
	parts := []string{m.Content}
	for _, a := range m.Attachments {
		if a.TranscribedText != "" {
			parts = append(parts, a.TranscribedText)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func snippet(body string) string {
	if len(body) <= 500 {
		return body
	}
	n := 500
	for n > 0 && !utf8.RuneStart(body[n]) {
		n--
	}
	return body[:n]
}

func (imp *Importer) persistMessage(ctx context.Context, sourceID int64, c Conversation, m Message, opts ImportOptions, sum *ImportSummary) error {
	var sender Actor
	if m.Sender != nil {
		sender = *m.Sender
	} else if m.SenderID > 0 {
		sender = Actor{ID: m.SenderID, Type: m.SenderType}
	}
	if actorKind(sender) == actorUser {
		if enriched, ok := imp.agents[sender.ID]; ok && sender.Email == "" {
			sender.Email = enriched.Email
		}
	}
	senderID, err := imp.resolveActor(ctx, sourceID, sender)
	if err != nil {
		return err
	}
	contact := c.Meta.Sender
	if contact.Type == "" {
		contact.Type = "contact"
	}
	contactID, err := imp.resolveActor(ctx, sourceID, contact)
	if err != nil {
		return err
	}
	var recipients []store.RecipientSet
	var members []store.ConversationParticipantRef
	if senderID > 0 {
		recipients = append(recipients, store.RecipientSet{Type: "from", ParticipantIDs: []int64{senderID}, DisplayNames: []string{actorName(sender)}, EmailAddresses: []string{sender.Email}})
		members = append(members, store.ConversationParticipantRef{ParticipantID: senderID, Role: "member"})
	}
	if m.Private || m.MessageType == 0 {
		inboxKey := SourceIdentifier(imp.client.baseURL, imp.client.accountID, opts.InboxID)
		inboxParticipant, resolveErr := imp.store.EnsureParticipantByIdentifier(SourceType, inboxKey, "Chatwoot inbox "+strconv.FormatInt(opts.InboxID, 10))
		if resolveErr != nil {
			return resolveErr
		}
		recipients = append(recipients, store.RecipientSet{Type: "to", ParticipantIDs: []int64{inboxParticipant}})
	}
	// Private notes and system activity have no observed customer delivery.
	if !m.Private && (m.MessageType == 1 || m.MessageType == 3) && contactID > 0 && contactID != senderID {
		recipients = append(recipients, store.RecipientSet{Type: "to", ParticipantIDs: []int64{contactID}, DisplayNames: []string{actorName(contact)}, EmailAddresses: []string{contact.Email}})
	}
	if contactID > 0 && contactID != senderID {
		members = append(members, store.ConversationParticipantRef{ParticipantID: contactID, Role: "member"})
	}
	body := messageBody(m)
	if m.Private {
		body = "[Private note]\n\n" + body
	}
	metadata := map[string]any{"provider": SourceType, "inbox_id": opts.InboxID, "conversation_id": c.ID, "message_type": m.MessageType, "private": m.Private, "status": m.Status, "sender": sender, "conversation": c}
	encoded, err := json.Marshal(metadata, json.Deterministic(true))
	if err != nil {
		return err
	}
	meta := sql.NullString{String: string(encoded), Valid: true}
	raw := []byte(m.Raw)
	if len(raw) == 0 {
		raw, err = json.Marshal(m)
		if err != nil {
			return err
		}
	}
	sourceMessageID := strconv.FormatInt(m.ID, 10)
	existing, err := imp.store.MessageExistsBatch(sourceID, []string{sourceMessageID})
	if err != nil {
		return err
	}
	title := actorName(contact)
	if title == "" {
		title = fmt.Sprintf("Chatwoot conversation %d", c.ID)
	}
	var toEmails []string
	for _, recipient := range recipients {
		if recipient.Type == "to" {
			toEmails = append(toEmails, recipient.EmailAddresses...)
		}
	}
	messageID, err := imp.store.PersistMessageContext(ctx, &store.MessagePersistData{
		Message: &store.Message{SourceID: sourceID, SourceMessageID: sourceMessageID, MessageType: SourceType,
			SentAt:   sql.NullTime{Time: time.Unix(m.CreatedAt, 0).UTC(), Valid: m.CreatedAt > 0},
			SenderID: sql.NullInt64{Int64: senderID, Valid: senderID > 0}, IsFromMe: imp.personalActor(sender, opts),
			Subject: sql.NullString{String: title, Valid: true}, Snippet: sql.NullString{String: snippet(body), Valid: body != ""}, SizeEstimate: int64(len(body)), PreserveAttachmentStats: true},
		Conversation: &store.ConversationPersistData{SourceConversationID: strconv.FormatInt(c.ID, 10), ConversationType: "direct_chat", Title: title, Participants: members, PreserveExistingParticipants: true},
		Metadata:     &meta, BodyText: sql.NullString{String: body, Valid: body != ""}, RawMIME: raw, RawFormat: "chatwoot_json", Recipients: recipients, PreserveLabels: true,
		FTS: &store.FTSDoc{Subject: title, Body: body, FromAddr: sender.Email, ToAddrs: strings.Join(toEmails, " ")},
	})
	if err != nil {
		return err
	}
	if _, found := existing[sourceMessageID]; !found {
		sum.MessagesAdded++
	}
	if err = imp.store.RecomputeConversationStatsForMessageContext(ctx, messageID); err != nil {
		return err
	}
	if err = imp.persistMedia(ctx, messageID, m.Attachments, opts, sum, nil); err != nil {
		return err
	}
	if m.ContentType == "voice_call" {
		chatMedia, mediaErr := imp.store.MessageChatwootAttachments(messageID)
		if mediaErr != nil {
			return mediaErr
		}
		meetingID, callErr := imp.persistCall(ctx, sourceID, c, m, messageID, contact, contactID, sender, senderID, chatMedia, opts, sum)
		if callErr != nil {
			return callErr
		}
		metadata["meeting_message_id"] = meetingID
		encoded, err = json.Marshal(metadata, json.Deterministic(true))
		if err != nil {
			return err
		}
		if err = imp.store.SetMessageMetadata(messageID, sql.NullString{String: string(encoded), Valid: true}); err != nil {
			return err
		}
	}
	return nil
}
