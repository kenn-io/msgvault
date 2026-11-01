package meta

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

const rawFormat = "facebook-comment-json"

type objectMetadata struct {
	AppID    string `json:"app_id"`
	PageID   string `json:"page_id"`
	PostID   string `json:"post_id"`
	ParentID string `json:"parent_id,omitempty"`
	Organic  bool   `json:"organic,omitempty"`
	Visitor  bool   `json:"visitor,omitempty"`
	Ad       bool   `json:"ad,omitempty"`
}

func (r *importRun) persist(ctx context.Context, obj Object, root, parent string, edge PostEdge) error {
	if !safeObjectID(obj.ID) || !safeObjectID(root) {
		return errors.New("invalid Facebook object identity")
	}
	if obj.Parent != nil && obj.Parent.ID != "" && obj.Parent.ID != parent {
		return errors.New("Facebook comment parent differs from selected traversal context")
	}
	if parent == obj.ID {
		return errors.New("Facebook comment cannot parent itself")
	}
	existing, err := r.imp.store.StoredMessagesContext(ctx, r.source.ID, rawFormat, []string{obj.ID, parent})
	if err != nil {
		return err
	}
	prior, exists := existing[obj.ID]
	meta := objectMetadata{AppID: r.app, PageID: r.page, PostID: root, ParentID: parent}
	if exists {
		saved, err := r.imp.store.GetMessageMetadataContext(ctx, prior.ID)
		if err != nil {
			return err
		}
		if !saved.Valid || json.Unmarshal([]byte(saved.String), &meta) != nil || meta.PostID != root || meta.ParentID != parent || meta.AppID != r.app || meta.PageID != r.page {
			return errors.New("Facebook object already belongs to a different root or parent")
		}
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
		parentRow, ok := existing[parent]
		if !ok {
			return errors.New("Facebook parent is not archived")
		}
		saved, err := r.imp.store.GetMessageMetadataContext(ctx, parentRow.ID)
		if err != nil {
			return err
		}
		var parentMeta objectMetadata
		if !saved.Valid || json.Unmarshal([]byte(saved.String), &parentMeta) != nil || parentMeta.PostID != root || (exists && prior.ConversationID != parentRow.ConversationID) {
			return errors.New("Facebook parent belongs to a different conversation")
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
		previous, err := r.imp.store.GetMessageRecipientsContext(ctx, prior.ID, "from")
		if err != nil {
			return err
		}
		if len(previous) > 0 {
			fromName = previous[0].DisplayName
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
	_, err = r.imp.store.PersistMessageContext(ctx, &store.MessagePersistData{
		Message: msg, Conversation: &store.ConversationPersistData{SourceConversationID: root, ConversationType: "public_comments", Title: title, Participants: participants, PreserveExistingParticipants: true},
		Metadata: &sql.NullString{String: string(metadata), Valid: true}, BodyText: sql.NullString{String: obj.Message, Valid: true},
		RawMIME: raw, RawFormat: rawFormat, Recipients: recipients, PreserveLabels: true,
		FTS: &store.FTSDoc{Body: obj.Message, FromAddr: fromName},
	})
	if err != nil {
		return err
	}
	if parent != "" {
		// UpsertMessage preserves an existing reply_to on updates. Reconcile it
		// only after validating both messages' exact root/conversation above.
		if err = r.imp.store.SetReplyToContext(ctx, r.source.ID, obj.ID, parent); err != nil {
			return err
		}
	}
	r.sum.MessagesProcessed++
	if !exists {
		r.sum.MessagesAdded++
	}
	return nil
}
