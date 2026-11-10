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
	Version            int    `json:"version"`
	AppID              string `json:"app_id"`
	PageID             string `json:"page_id"`
	PostID             string `json:"post_id,omitempty"`
	InstagramAccountID string `json:"instagram_account_id,omitempty"`
	MediaID            string `json:"media_id,omitempty"`
	ParentID           string `json:"parent_id,omitempty"`
	Organic            bool   `json:"organic,omitempty"`
	Visitor            bool   `json:"visitor,omitempty"`
	Ad                 bool   `json:"ad,omitempty"`
	// Hidden is set by a webhook hide or unhide; polling keeps it.
	Hidden *bool `json:"hidden,omitempty"`
}

func (m objectMetadata) matches(app, page, account, root string) bool {
	if m.AppID != app || m.PageID != page || m.InstagramAccountID != account {
		return false
	}
	if account != "" {
		return m.MediaID == root && m.PostID == ""
	}
	return m.PostID == root && m.MediaID == ""
}

func (r *importRun) objectFormat() string {
	if r.imp.instagram != "" {
		return "instagram-comment-json"
	}
	return rawFormat
}

func (r *importRun) persist(ctx context.Context, obj Object, root, parent string, edge PostEdge) error {
	if r.imp.instagram != "" && obj.InstagramMedia != nil && obj.InstagramMedia.Owner != nil {
		if owner := obj.InstagramMedia.Owner.ID; owner != "" && owner != r.imp.instagram {
			return errors.New("instagram media owner mismatch")
		}
	}
	if obj.Parent != nil && obj.Parent.ID != "" {
		parent = obj.Parent.ID
	}
	if parent != "" && !safeObjectID(parent) {
		return errors.New("invalid comment parent identity")
	}
	if parent == obj.ID {
		return errors.New("comment cannot parent itself")
	}
	existing, err := r.imp.store.StoredMessagesContext(ctx, r.source.ID, r.objectFormat(), []string{obj.ID, parent, root})
	if err != nil {
		return err
	}
	prior, exists := existing[obj.ID]
	meta := objectMetadata{Version: objectMetadataVersion, AppID: r.app, PageID: r.page, PostID: root, ParentID: parent}
	var previousMetadata sql.NullString
	if r.imp.instagram != "" {
		meta.PostID, meta.MediaID, meta.InstagramAccountID = "", root, r.imp.instagram
	}
	if exists {
		previousMetadata, err = r.imp.store.GetMessageMetadataContext(ctx, prior.ID)
		if err != nil {
			return err
		}
		var storedMeta objectMetadata
		if !previousMetadata.Valid || json.Unmarshal([]byte(previousMetadata.String), &storedMeta) != nil || !storedMeta.matches(r.app, r.page, r.imp.instagram, root) || storedMeta.ParentID != parent {
			return errors.New("comment object already belongs to a different root or parent")
		}
		if storedMeta.Version != objectMetadataVersion {
			return errors.New("unsupported comment object metadata version")
		}
		meta = storedMeta
	}
	switch edge {
	case Posts, instagramMediaEdge:
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
			return errors.New("comment root is not archived")
		}
		parentRow, ok := existing[parent]
		if !ok {
			return errors.New("comment parent is not archived")
		}
		if rootRow.ConversationID != parentRow.ConversationID || (exists && prior.ConversationID != parentRow.ConversationID) {
			return errors.New("comment parent belongs to a different conversation")
		}
		savedRoot, err := r.imp.store.GetMessageMetadataContext(ctx, rootRow.ID)
		if err != nil {
			return err
		}
		var rootMeta objectMetadata
		if !savedRoot.Valid || json.Unmarshal([]byte(savedRoot.String), &rootMeta) != nil || !rootMeta.matches(r.app, r.page, r.imp.instagram, root) || rootMeta.ParentID != "" {
			return errors.New("comment root belongs to a different context or has a parent")
		}
		if rootMeta.Version != objectMetadataVersion {
			return errors.New("unsupported comment root metadata version")
		}
		if parent != root {
			saved, err := r.imp.store.GetMessageMetadataContext(ctx, parentRow.ID)
			if err != nil {
				return err
			}
			var parentMeta objectMetadata
			if !saved.Valid || json.Unmarshal([]byte(saved.String), &parentMeta) != nil || !parentMeta.matches(r.app, r.page, r.imp.instagram, root) {
				return errors.New("comment parent belongs to a different conversation")
			}
			if parentMeta.Version != objectMetadataVersion {
				return errors.New("unsupported comment parent metadata version")
			}
		}
		reply = sql.NullInt64{Int64: parentRow.ID, Valid: true}
	}
	sender, fromName, self, err := r.messageActor(ctx, obj)
	if err != nil {
		return err
	}
	metadata, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	var original any = obj
	if obj.InstagramMedia != nil {
		original = obj.InstagramMedia
	} else if obj.InstagramComment != nil {
		original = obj.InstagramComment
	}
	raw, err := json.Marshal(original)
	if err != nil {
		return err
	}
	title := ""
	if parent == "" {
		title = obj.Message
		if title == "" {
			title = "Facebook post " + root
			if r.imp.instagram != "" {
				title = "Instagram media " + root
			}
		}
	}
	row := archivedObject{
		sourceID: r.source.ID, messageType: r.imp.sourceType(), format: r.objectFormat(),
		obj: obj, conversation: root, title: title, reply: reply,
		sender: sender, fromName: fromName, self: self, metadata: metadata, raw: raw,
	}
	var priorRow *store.StoredMessage
	if exists {
		priorRow = &prior
	}
	return archiveObject(ctx, r.imp.store, r.sum, row, priorRow, previousMetadata)
}

// archivedObject is one post, media or comment row. The comment and mention
// importers differ in identity and metadata, then share this write.
type archivedObject struct {
	sourceID            int64
	messageType, format string
	obj                 Object
	conversation, title string
	subject             string
	reply               sql.NullInt64
	sender              int64
	fromName            string
	self                bool
	metadata, raw       []byte
}

// archiveObject writes o unless the archived row already holds the same
// content, and counts the result. prior is the row stored for o.obj.ID, if any.
func archiveObject(ctx context.Context, st *store.Store, sum *ImportSummary, o archivedObject, prior *store.StoredMessage, priorMetadata sql.NullString) error {
	sender, fromName, self := o.sender, o.fromName, o.self
	recipients := []store.RecipientSet{}
	participants := []store.ConversationParticipantRef{}
	if prior != nil && prior.SenderID.Valid && (sender == 0 || sender == prior.SenderID.Int64 && fromName == "") {
		sender = prior.SenderID.Int64
		self = prior.SourceIsFromMe.Valid && prior.SourceIsFromMe.Bool
		// Preserve stronger prior evidence when the current Graph projection omits
		// its author or author name. An absent field is not a provider retraction.
		previous, err := st.GetMessagesSummariesByIDsContext(ctx, []int64{prior.ID})
		if err != nil {
			return err
		}
		if len(previous) > 0 {
			fromName = previous[0].FromName
		}
	}
	if sender != 0 {
		recipients = append(recipients, store.RecipientSet{Type: "from", ParticipantIDs: []int64{sender}, DisplayNames: []string{fromName}})
		participants = append(participants, store.ConversationParticipantRef{ParticipantID: sender, Role: "member"})
	}
	var sent sql.NullTime
	if o.obj.CreatedTime != "" {
		parsed, err := time.Parse("2006-01-02T15:04:05-0700", o.obj.CreatedTime)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, o.obj.CreatedTime)
		}
		if err != nil {
			return errors.New("invalid comment creation time")
		}
		sent = sql.NullTime{Time: parsed, Valid: true}
	} else if prior != nil {
		sent = prior.SentAt
	}
	text := o.obj.Message
	msg := &store.Message{SourceID: o.sourceID, SourceMessageID: o.obj.ID, MessageType: o.messageType,
		SentAt: sent, SenderID: sql.NullInt64{Int64: sender, Valid: sender != 0}, IsFromMe: self,
		Subject: sql.NullString{String: o.subject, Valid: o.subject != ""}, ReplyToMessageID: o.reply,
		Snippet: sql.NullString{String: text, Valid: true}, SizeEstimate: int64(len(text)),
	}
	data := &store.MessagePersistData{
		Message: msg, Conversation: &store.ConversationPersistData{SourceConversationID: o.conversation, ConversationType: "public_comments", Title: o.title, Participants: participants, PreserveExistingParticipants: true},
		Metadata: &sql.NullString{String: string(o.metadata), Valid: true}, BodyText: sql.NullString{String: text, Valid: true},
		RawMIME: o.raw, RawFormat: o.format, Recipients: recipients, PreserveLabels: true,
		FTS: &store.FTSDoc{Subject: o.subject, Body: text, FromAddr: fromName},
	}
	unchanged := false
	if prior != nil {
		var err error
		unchanged, err = messageUnchanged(ctx, st, *prior, priorMetadata, data)
		if err != nil {
			return err
		}
	}
	if unchanged {
		conversationID, err := st.EnsureConversationWithTypeContext(ctx, o.sourceID, o.conversation, data.Conversation.ConversationType, o.title)
		if err != nil {
			return err
		}
		unchanged = conversationID == prior.ConversationID
		if unchanged {
			for _, member := range participants {
				if err := st.EnsureConversationParticipantContext(ctx, conversationID, member.ParticipantID, member.Role); err != nil {
					return err
				}
			}
		}
	}
	if !unchanged {
		if _, err := st.PersistMessageContext(ctx, data); err != nil {
			return err
		}
	}
	sum.MessagesProcessed++
	switch {
	case prior == nil:
		sum.MessagesAdded++
	case !unchanged:
		sum.MessagesUpdated++
	}
	return nil
}

func messageUnchanged(ctx context.Context, st *store.Store, prior store.StoredMessage, metadata sql.NullString, data *store.MessagePersistData) (bool, error) {
	msg := data.Message
	if prior.SenderID != msg.SenderID || !prior.SourceIsFromMe.Valid || prior.SourceIsFromMe.Bool != msg.IsFromMe ||
		prior.MessageType != msg.MessageType || prior.Subject != msg.Subject || prior.Snippet != msg.Snippet ||
		prior.SizeEstimate != msg.SizeEstimate || prior.InternalDate != msg.InternalDate ||
		prior.SentAt.Valid != msg.SentAt.Valid || (prior.SentAt.Valid && !prior.SentAt.Time.Equal(msg.SentAt.Time)) ||
		!meetingarchive.JSONEvidenceEqual(prior.Raw, data.RawMIME) ||
		!meetingarchive.JSONEvidenceEqual([]byte(metadata.String), []byte(data.Metadata.String)) {
		return false, nil
	}
	contentMatches, err := st.MessageContentMatchesContext(ctx, prior.ID, data.BodyText, *data.FTS)
	if err != nil || !contentMatches {
		return false, err
	}
	return st.MessageRecipientsMatchContext(ctx, prior.ID, data.Recipients)
}
