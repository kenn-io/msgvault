package meta

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

const rawFormat = "facebook-comment-json"
const objectMetadataVersion = 1

type objectMetadata struct {
	Version  int    `json:"version"`
	AppID    string `json:"app_id"`
	PageID   string `json:"page_id"`
	PostID   string `json:"post_id"`
	ParentID string `json:"parent_id,omitempty"`
	Organic  bool   `json:"organic,omitempty"`
	Visitor  bool   `json:"visitor,omitempty"`
	Ad       bool   `json:"ad,omitempty"`
}

func (r *importRun) persist(ctx context.Context, obj Object, root, parent string, edge PostEdge) error {
	if obj.Parent != nil && obj.Parent.ID != "" {
		parent = obj.Parent.ID
	}
	if parent != "" && !safeObjectID(parent) {
		return errors.New("invalid Facebook parent identity")
	}
	if parent == obj.ID {
		return errors.New("facebook comment cannot parent itself")
	}
	existing, err := r.imp.store.StoredMessagesContext(ctx, r.source.ID, rawFormat, []string{obj.ID, parent, root})
	if err != nil {
		return err
	}
	prior, exists := existing[obj.ID]
	meta := objectMetadata{Version: objectMetadataVersion, AppID: r.app, PageID: r.page, PostID: root, ParentID: parent}
	var previousMetadata sql.NullString
	if exists {
		previousMetadata, err = r.imp.store.GetMessageMetadataContext(ctx, prior.ID)
		if err != nil {
			return err
		}
		var storedMeta objectMetadata
		if !previousMetadata.Valid || json.Unmarshal([]byte(previousMetadata.String), &storedMeta) != nil || storedMeta.PostID != root || storedMeta.ParentID != parent || storedMeta.AppID != r.app || storedMeta.PageID != r.page {
			return errors.New("facebook object already belongs to a different root or parent")
		}
		if storedMeta.Version != objectMetadataVersion {
			return errors.New("unsupported Facebook object metadata version")
		}
		meta = storedMeta
	}
	switch edge {
	case Posts:
		meta.Organic = true
	case VisitorPosts:
		meta.Visitor = true
	case AdsPosts:
		meta.Ad = true
	}
	var reply sql.NullInt64
	if parent != "" {
		rootRow, ok := existing[root]
		if !ok {
			return errors.New("facebook root is not archived")
		}
		parentRow, ok := existing[parent]
		if !ok {
			return errors.New("facebook parent is not archived")
		}
		if rootRow.ConversationID != parentRow.ConversationID || (exists && prior.ConversationID != parentRow.ConversationID) {
			return errors.New("facebook parent belongs to a different conversation")
		}
		savedRoot, err := r.imp.store.GetMessageMetadataContext(ctx, rootRow.ID)
		if err != nil {
			return err
		}
		var rootMeta objectMetadata
		if !savedRoot.Valid || json.Unmarshal([]byte(savedRoot.String), &rootMeta) != nil || rootMeta.AppID != r.app || rootMeta.PageID != r.page || rootMeta.PostID != root || rootMeta.ParentID != "" {
			return errors.New("facebook root belongs to a different context or has a parent")
		}
		if rootMeta.Version != objectMetadataVersion {
			return errors.New("unsupported Facebook root metadata version")
		}
		if parent != root {
			saved, err := r.imp.store.GetMessageMetadataContext(ctx, parentRow.ID)
			if err != nil {
				return err
			}
			var parentMeta objectMetadata
			if !saved.Valid || json.Unmarshal([]byte(saved.String), &parentMeta) != nil || parentMeta.AppID != r.app || parentMeta.PageID != r.page || parentMeta.PostID != root {
				return errors.New("facebook parent belongs to a different conversation")
			}
			if parentMeta.Version != objectMetadataVersion {
				return errors.New("unsupported Facebook parent metadata version")
			}
		}
		reply = sql.NullInt64{Int64: parentRow.ID, Valid: true}
	}
	sender, err := resolveActor(ctx, r.imp.store, r.source.ID, r.app, r.page, obj.From)
	if err != nil {
		return err
	}
	recipients := []store.RecipientSet{}
	participants := []store.ConversationParticipantRef{}
	fromName := ""
	self := obj.From != nil && obj.From.ID == r.page
	if sender == 0 && exists && prior.SenderID.Valid {
		sender = prior.SenderID.Int64
		self = prior.SourceIsFromMe.Valid && prior.SourceIsFromMe.Bool
		// Preserve stronger prior evidence when the current Graph projection omits
		// its author. An absent field is not a provider retraction.
		previous, err := r.imp.store.GetMessagesSummariesByIDsContext(ctx, []int64{prior.ID})
		if err != nil {
			return err
		}
		if len(previous) > 0 {
			fromName = previous[0].FromName
		}
	} else if obj.From != nil {
		fromName = obj.From.Name
	}
	if sender != 0 {
		recipients = append(recipients, store.RecipientSet{Type: "from", ParticipantIDs: []int64{sender}, DisplayNames: []string{fromName}})
		participants = append(participants, store.ConversationParticipantRef{ParticipantID: sender, Role: "member"})
	}
	var sent sql.NullTime
	if obj.CreatedTime != "" {
		parsed, err := time.Parse("2006-01-02T15:04:05-0700", obj.CreatedTime)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, obj.CreatedTime)
		}
		if err != nil {
			return errors.New("invalid Facebook object creation time")
		}
		sent = sql.NullTime{Time: parsed, Valid: true}
	} else if exists {
		sent = prior.SentAt
	}
	metadata, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	title := ""
	if parent == "" {
		title = obj.Message
		if title == "" {
			title = "Facebook post " + root
		}
	}
	msg := &store.Message{SourceID: r.source.ID, SourceMessageID: obj.ID, MessageType: SourceType,
		SentAt: sent, SenderID: sql.NullInt64{Int64: sender, Valid: sender != 0}, IsFromMe: self,
		ReplyToMessageID: reply, Snippet: sql.NullString{String: obj.Message, Valid: true}, SizeEstimate: int64(len(obj.Message)),
	}
	data := &store.MessagePersistData{
		Message: msg, Conversation: &store.ConversationPersistData{SourceConversationID: root, ConversationType: "public_comments", Title: title, Participants: participants, PreserveExistingParticipants: true},
		Metadata: &sql.NullString{String: string(metadata), Valid: true}, BodyText: sql.NullString{String: obj.Message, Valid: true},
		RawMIME: raw, RawFormat: rawFormat, Recipients: recipients, PreserveLabels: true,
		FTS: &store.FTSDoc{Body: obj.Message, FromAddr: fromName},
	}
	unchanged := false
	if exists {
		unchanged, err = r.messageUnchanged(ctx, prior, previousMetadata, data)
		if err != nil {
			return err
		}
	}
	if unchanged {
		conversationID, err := r.imp.store.EnsureConversationWithTypeContext(ctx, r.source.ID, root, data.Conversation.ConversationType, title)
		if err != nil {
			return err
		}
		unchanged = conversationID == prior.ConversationID
		if unchanged {
			for _, member := range participants {
				if err := r.imp.store.EnsureConversationParticipantContext(ctx, conversationID, member.ParticipantID, member.Role); err != nil {
					return err
				}
			}
		}
	}
	if !unchanged {
		if _, err := r.imp.store.PersistMessageContext(ctx, data); err != nil {
			return err
		}
	}
	r.sum.MessagesProcessed++
	if !exists {
		r.sum.MessagesAdded++
	}
	return nil
}

func (r *importRun) messageUnchanged(ctx context.Context, prior store.StoredMessage, metadata sql.NullString, data *store.MessagePersistData) (bool, error) {
	msg := data.Message
	if prior.SenderID != msg.SenderID || !prior.SourceIsFromMe.Valid || prior.SourceIsFromMe.Bool != msg.IsFromMe ||
		prior.MessageType != msg.MessageType || prior.Subject != msg.Subject || prior.Snippet != msg.Snippet ||
		prior.SizeEstimate != msg.SizeEstimate || prior.InternalDate != msg.InternalDate ||
		prior.SentAt.Valid != msg.SentAt.Valid || (prior.SentAt.Valid && !prior.SentAt.Time.Equal(msg.SentAt.Time)) ||
		!meetingarchive.JSONEvidenceEqual(prior.Raw, data.RawMIME) ||
		!meetingarchive.JSONEvidenceEqual([]byte(metadata.String), []byte(data.Metadata.String)) {
		return false, nil
	}
	contentMatches, err := r.imp.store.MessageContentMatchesContext(ctx, prior.ID, data.BodyText, *data.FTS)
	if err != nil || !contentMatches {
		return false, err
	}
	return r.imp.store.MessageRecipientsMatchContext(ctx, prior.ID, data.Recipients)
}
