package chatwoot

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
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

// persistMessage returns when the message's refresh window starts, or zero
// once nothing about it can change without new conversation activity. A failed
// download counts from its first failure; a pending recording or transcript from the message.
func (imp *Importer) persistMessage(ctx context.Context, sourceID int64, c Conversation, m Message, opts ImportOptions, sum *ImportSummary) (refreshFrom int64, resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = &fatalImportError{resultErr}
		}
	}()
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
		return 0, err
	}
	contact := c.Meta.Sender
	if contact.Type == "" {
		contact.Type = "contact"
	}
	contactID, err := imp.resolveActor(ctx, sourceID, contact)
	if err != nil {
		return 0, err
	}
	var recipients []store.RecipientSet
	var members []store.ConversationParticipantRef
	if senderID > 0 {
		recipients = append(recipients, store.RecipientSet{Type: "from", ParticipantIDs: []int64{senderID}, DisplayNames: []string{actorName(sender)}, EmailAddresses: []string{envelopeEmail(sender)}})
		members = append(members, store.ConversationParticipantRef{ParticipantID: senderID, Role: "member"})
	}
	if m.Private || m.MessageType == 0 {
		inboxKey := SourceIdentifier(imp.client.baseURL, imp.client.accountID, opts.InboxID)
		inboxParticipant, resolveErr := imp.store.EnsureParticipantByIdentifier(SourceType, inboxKey, "Chatwoot inbox "+strconv.FormatInt(opts.InboxID, 10))
		if resolveErr != nil {
			return 0, resolveErr
		}
		recipients = append(recipients, store.RecipientSet{Type: "to", ParticipantIDs: []int64{inboxParticipant}})
	}
	// Private notes and system activity have no observed customer delivery.
	if !m.Private && (m.MessageType == 1 || m.MessageType == 3) {
		addressed, addressErr := imp.addressedRecipients(ctx, m)
		if addressErr != nil {
			return 0, addressErr
		}
		recipients = append(recipients, addressed...)
		named := slices.ContainsFunc(addressed, func(set store.RecipientSet) bool { return set.Type == "to" })
		if !named && contactID > 0 && contactID != senderID {
			recipients = append(recipients, store.RecipientSet{Type: "to", ParticipantIDs: []int64{contactID}, DisplayNames: []string{actorName(contact)}, EmailAddresses: []string{contact.Email}})
		}
	}
	// Every role is written, so a refreshed message drops recipients it lost.
	for _, kind := range []string{"from", "to", "cc", "bcc"} {
		if !slices.ContainsFunc(recipients, func(set store.RecipientSet) bool { return set.Type == kind }) {
			recipients = append(recipients, store.RecipientSet{Type: kind})
		}
	}
	if contactID > 0 && contactID != senderID {
		members = append(members, store.ConversationParticipantRef{ParticipantID: contactID, Role: "member"})
	}
	body := messageBody(m)
	if m.Private {
		body = "[Private note]\n\n" + body
	}
	metadata := map[string]any{"provider": SourceType, "inbox_id": opts.InboxID, "conversation_id": c.ID, "message_type": m.MessageType, "private": m.Private, "status": m.Status, "sender": sender}
	raw := []byte(m.Raw)
	if len(raw) == 0 {
		raw, err = json.Marshal(m)
		if err != nil {
			return 0, err
		}
	}
	sourceMessageID := strconv.FormatInt(m.ID, 10)
	existing, err := imp.store.MessageMetadataBatch(sourceID, []string{sourceMessageID})
	if err != nil {
		return 0, err
	}
	if previous, found := existing[sourceMessageID]; found && m.ContentType == "voice_call" {
		if linked := store.RelatedMessageCandidate(previous.ID, SourceType, previous.Metadata.String); linked != nil {
			metadata["meeting_message_id"] = *linked
		}
	}
	encoded, err := json.Marshal(metadata, json.Deterministic(true))
	if err != nil {
		return 0, err
	}
	meta := sql.NullString{String: string(encoded), Valid: true}
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
			SenderID: sql.NullInt64{Int64: senderID, Valid: senderID > 0}, IdentityDerivedIsFromMe: true,
			Subject: sql.NullString{String: title, Valid: true}, Snippet: sql.NullString{String: meetingarchive.Snippet(body), Valid: body != ""}, SizeEstimate: int64(len(body)), PreserveAttachmentStats: true},
		Conversation: &store.ConversationPersistData{SourceConversationID: strconv.FormatInt(c.ID, 10), ConversationType: "direct_chat", Title: title, Participants: members, PreserveExistingParticipants: true},
		Metadata:     &meta, BodyText: sql.NullString{String: body, Valid: body != ""}, RawMIME: raw, RawFormat: "chatwoot_json", Recipients: recipients, PreserveLabels: true,
		FTS: &store.FTSDoc{Subject: title, Body: body, FromAddr: envelopeEmail(sender), ToAddrs: strings.Join(toEmails, " ")},
	})
	if err != nil {
		return 0, err
	}
	if _, found := existing[sourceMessageID]; !found {
		sum.MessagesAdded++
	}
	if err = imp.store.RecomputeConversationStatsForMessageContext(ctx, messageID); err != nil {
		return 0, err
	}
	failedSince, waiting, err := imp.persistMedia(ctx, messageID, m.Attachments, opts, sum, nil)
	if err != nil {
		return 0, err
	}
	if waiting {
		refreshFrom = m.CreatedAt
	}
	for _, a := range m.Attachments {
		if isAudio(a) && a.TranscribedText == "" {
			refreshFrom = m.CreatedAt
		}
	}
	refreshFrom = max(refreshFrom, failedSince)
	if m.ContentType == "voice_call" {
		chatMedia, mediaErr := imp.store.MessageProviderAttachments(messageID, "chatwoot:")
		if mediaErr != nil {
			return 0, mediaErr
		}
		_, callRefreshFrom, callErr := imp.persistCall(ctx, sourceID, c, m, messageID, contact, contactID, sender, senderID, metadata, chatMedia, opts, sum)
		if callErr != nil {
			return 0, callErr
		}
		refreshFrom = max(refreshFrom, callRefreshFrom)
	}
	return refreshFrom, nil
}

// addressedRecipients returns the recipients an email reply or forward names.
// Chatwoot stores them on the message; other outgoing messages name none.
func (imp *Importer) addressedRecipients(ctx context.Context, m Message) ([]store.RecipientSet, error) {
	var sets []store.RecipientSet
	for _, field := range []struct{ key, kind string }{{"to_emails", "to"}, {"cc_emails", "cc"}, {"bcc_emails", "bcc"}} {
		var emails []string
		if raw, ok := m.ContentAttributes[field.key]; !ok || json.Unmarshal(raw, &emails) != nil {
			continue
		}
		set := store.RecipientSet{Type: field.kind}
		for _, email := range emails {
			email = strings.ToLower(strings.TrimSpace(email))
			at := strings.LastIndex(email, "@")
			if at <= 0 || at == len(email)-1 {
				continue
			}
			pid, err := imp.store.EnsureParticipantContext(ctx, email, "", email[at+1:])
			if err != nil {
				return nil, err
			}
			set.ParticipantIDs = append(set.ParticipantIDs, pid)
			set.DisplayNames = append(set.DisplayNames, "")
			set.EmailAddresses = append(set.EmailAddresses, email)
		}
		if len(set.ParticipantIDs) > 0 {
			sets = append(sets, set)
		}
	}
	return sets, nil
}
