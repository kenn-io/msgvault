package sync

import (
	"database/sql"
	"sort"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

func preparedMessageParticipants(data *messageData) ([]store.ParticipantPersistData, map[string]int) {
	byEmail := make(map[string]mime.Address)
	for _, addresses := range [][]mime.Address{data.from, data.to, data.cc, data.bcc} {
		for _, address := range addresses {
			if address.Email != "" {
				if _, exists := byEmail[address.Email]; !exists {
					byEmail[address.Email] = address
				}
			}
		}
	}
	emails := make([]string, 0, len(byEmail))
	for email := range byEmail {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	participants := make([]store.ParticipantPersistData, len(emails))
	index := make(map[string]int, len(emails))
	for i, email := range emails {
		address := byEmail[email]
		participants[i] = store.ParticipantPersistData{
			EmailAddress: email, DisplayName: address.Name, Domain: address.Domain,
		}
		index[email] = i
	}
	return participants, index
}

func buildPreparedSnapshot(
	prepared *messageData,
	participantIndex map[string]int,
	participantIDs []int64,
	attachmentWrites []store.AttachmentWrite,
) *store.MessagePersistData {
	participantMap := make(map[string]int64, len(participantIndex))
	for email, index := range participantIndex {
		participantMap[email] = participantIDs[index]
	}
	message := *prepared.message
	if len(prepared.from) > 0 && prepared.from[0].Email != "" {
		if senderID, ok := participantMap[prepared.from[0].Email]; ok {
			message.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
		}
	}

	return &store.MessagePersistData{
		Message: &message,
		Conversation: &store.ConversationPersistData{
			SourceConversationID: prepared.threadID,
			ConversationType:     prepared.conversationType,
			Title:                prepared.conversationTitle,
		},
		BodyText: sql.NullString{
			String: prepared.bodyText, Valid: prepared.bodyText != "",
		},
		BodyHTML: sql.NullString{
			String: prepared.bodyHTML, Valid: prepared.bodyHTML != "",
		},
		RawMIME: prepared.rawMIME,
		Recipients: []store.RecipientSet{
			buildRecipientSet("from", prepared.from, participantMap),
			buildRecipientSet("to", prepared.to, participantMap),
			buildRecipientSet("cc", prepared.cc, participantMap),
			buildRecipientSet("bcc", prepared.bcc, participantMap),
		},
		MIMEAttachmentReplacement: &attachmentWrites,
		FTS: &store.FTSDoc{
			Subject:  message.Subject.String,
			Body:     prepared.bodyText,
			FromAddr: joinEmails(prepared.from),
			ToAddrs:  joinEmails(prepared.to),
			CcAddrs:  joinEmails(prepared.cc),
		},
	}
}
