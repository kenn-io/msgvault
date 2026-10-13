package teams

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type chatPersistContext struct {
	conversationID int64
	chat           Chat
	opts           ImportOptions
	toRecipients   []recipientRef
	live           bool
}

// persistChatMessage commits mandatory native state before publication. Optional
// media downloads and derived search writes follow the readable snapshot.
func (imp *Importer) persistChatMessage(ctx context.Context, sourceID int64, gm *ChatMessage, cc *chatPersistContext, sum *ImportSummary) (int64, bool, error) {
	sourceMessageID := chatSourceMessageID(cc.chat.ID, gm.ID)
	if err := imp.store.MigrateSourceMessageID(sourceID, cc.conversationID, gm.ID, sourceMessageID); err != nil {
		return 0, false, err
	}
	if gm.DeletedDateTime != nil {
		if err := imp.store.MarkMessageDeleted(sourceID, sourceMessageID); err != nil {
			return 0, false, err
		}
		return 0, false, nil
	}
	msg, text := mapMessage(gm, cc.conversationID, sourceID, sourceMessageID)
	from := store.RecipientSet{Type: "from"}
	var members []store.ConversationParticipantRef
	senderName := ""
	if id := identityOf(gm.From); id != nil {
		pid, err := imp.res.resolve(ctx, id)
		if err != nil {
			return 0, false, err
		}
		if pid != 0 {
			msg.SenderID = sql.NullInt64{Int64: pid, Valid: true}
			senderName = id.DisplayName
			from.ParticipantIDs = []int64{pid}
			from.DisplayNames = []string{senderName}
			members = append(members, store.ConversationParticipantRef{ParticipantID: pid, Role: "member"})
		}
	}
	recipients := []store.RecipientSet{from}
	if cc.toRecipients != nil {
		to := store.RecipientSet{Type: "to"}
		for _, r := range cc.toRecipients {
			if r.ID == 0 {
				continue
			}
			members = append(members, store.ConversationParticipantRef{ParticipantID: r.ID, Role: "member"})
			if r.ID != msg.SenderID.Int64 {
				to.ParticipantIDs = append(to.ParticipantIDs, r.ID)
				to.DisplayNames = append(to.DisplayNames, r.Name)
			}
		}
		to.ParticipantIDs, to.DisplayNames = dedupRecipients(to.ParticipantIDs, to.DisplayNames)
		recipients = append(recipients, to)
	}
	mentions := store.RecipientSet{Type: "mention"}
	for _, mention := range gm.Mentions {
		id := identityOf(mention.Mentioned)
		if id == nil {
			continue
		}
		pid, err := imp.res.resolve(ctx, id)
		if err != nil {
			return 0, false, err
		}
		if pid != 0 {
			mentions.ParticipantIDs = append(mentions.ParticipantIDs, pid)
			mentions.DisplayNames = append(mentions.DisplayNames, id.DisplayName)
		}
	}
	mentions.ParticipantIDs, mentions.DisplayNames = dedupRecipients(mentions.ParticipantIDs, mentions.DisplayNames)
	recipients = append(recipients, mentions)
	reactions := make([]store.ReactionRef, 0, len(gm.Reactions))
	for _, reaction := range gm.Reactions {
		pid, err := imp.res.resolve(ctx, identityOf(reaction.User))
		if err != nil {
			return 0, false, err
		}
		if pid != 0 {
			reactions = append(reactions, store.ReactionRef{ParticipantID: pid, Type: reaction.ReactionType, Value: reaction.ReactionType, CreatedAt: reaction.CreatedDateTime})
		}
	}
	var links []store.AttachmentRef
	if recordingURL, name, ok := gm.callRecording(); ok {
		links = append(links, store.AttachmentRef{Filename: name, StoragePath: recordingURL, SourceAttachmentID: "teams:recording:" + recordingURL})
	}
	for _, att := range gm.Attachments {
		if att.ContentURL == "" {
			continue
		}
		id := att.ID
		if id == "" {
			id = att.ContentURL
		}
		links = append(links, store.AttachmentRef{Filename: att.Name, MimeType: att.ContentType, StoragePath: att.ContentURL, SourceAttachmentID: "teams:link:" + id})
	}
	raw := []byte(gm.Raw)
	if len(raw) == 0 {
		var err error
		raw, err = json.Marshal(gm, json.Deterministic(true))
		if err != nil {
			return 0, false, fmt.Errorf("marshal teams message raw archive: %w", err)
		}
	}
	metadataJSON, err := json.Marshal(map[string]string{"teams_chat_id": cc.chat.ID, "teams_message_id": gm.ID}, json.Deterministic(true))
	if err != nil {
		return 0, false, fmt.Errorf("marshal teams message metadata: %w", err)
	}
	metadata := sql.NullString{String: string(metadataJSON), Valid: true}
	bodyHTML := sql.NullString{}
	if gm.Body.ContentType == "html" {
		bodyHTML = sql.NullString{String: gm.Body.Content, Valid: true}
	}
	data := &store.MessagePersistData{
		Message: &msg, Metadata: &metadata, PreserveLabels: true,
		Conversation: &store.ConversationPersistData{
			SourceConversationID: cc.chat.ID, ConversationType: conversationType(cc.chat.ChatType), Title: cc.chat.Topic,
			Participants: members, PreserveExistingParticipants: true,
		},
		BodyText: sql.NullString{String: text, Valid: text != ""}, BodyHTML: bodyHTML,
		RawMIME: raw, RawFormat: "teams_json", Recipients: recipients,
		ReactionSnapshot: &reactions, LinkAttachmentSnapshot: &links,
	}
	mode := store.IngestBackfill
	if cc.live {
		mode = store.IngestLive
	}
	messageID, err := imp.store.WithIngestContext(store.IngestContext{Mode: mode, ObservedAt: time.Now()}).PersistMessageContext(ctx, data)
	if err != nil {
		return 0, false, fmt.Errorf("archive teams message snapshot: %w", err)
	}
	sum.ReactionsAdded += int64(len(reactions))
	sum.AttachmentsFound += int64(len(links))
	if imp.downloadInlineImages(ctx, messageID, gm.Body.Content, cc.opts, sum) {
		if err := imp.store.RecomputeMessageAttachmentStats(messageID); err != nil {
			sum.Errors++
		}
	}
	if err := imp.store.UpsertFTS(messageID, msg.Subject.String, text, senderName, "", ""); err != nil {
		sum.Errors++
	}
	return messageID, true, nil
}
