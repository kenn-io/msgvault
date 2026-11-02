package meta

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

const rawFormat = "facebook-comment-json"
const objectMetadataVersion = 1

var errInvalidObject = errors.New("invalid comment object")

type objectMetadata struct {
	Hidden             bool   `json:"hidden,omitempty"`
	BodyTime           int64  `json:"body_time,omitempty"`
	StateTime          int64  `json:"state_time,omitempty"`
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
			return fmt.Errorf("%w: instagram media owner mismatch", errInvalidObject)
		}
	}
	if obj.Parent != nil && obj.Parent.ID != "" {
		parent = obj.Parent.ID
	}
	if parent != "" && !safeObjectID(parent) {
		return fmt.Errorf("%w: invalid comment parent identity", errInvalidObject)
	}
	if parent == obj.ID {
		return fmt.Errorf("%w: comment cannot parent itself", errInvalidObject)
	}
	existing, err := r.imp.store.StoredMessagesContext(ctx, r.source.ID, r.objectFormat(), []string{obj.ID, parent, root})
	if err != nil {
		return err
	}
	prior, exists := existing[obj.ID]
	meta := objectMetadata{Version: objectMetadataVersion, AppID: r.app, PageID: r.page, PostID: root, ParentID: parent}
	if r.imp.instagram != "" {
		meta.PostID, meta.MediaID, meta.InstagramAccountID = "", root, r.imp.instagram
	}
	if exists {
		saved, err := r.imp.store.GetMessageMetadataContext(ctx, prior.ID)
		if err != nil {
			return err
		}
		var storedMeta objectMetadata
		if !saved.Valid || json.Unmarshal([]byte(saved.String), &storedMeta) != nil || !storedMeta.matches(r.app, r.page, r.imp.instagram, root) || storedMeta.ParentID != parent {
			return fmt.Errorf("%w: comment object already belongs to a different root or parent", errInvalidObject)
		}
		if storedMeta.Version != objectMetadataVersion {
			return fmt.Errorf("%w: unsupported comment object metadata version", errInvalidObject)
		}
		meta = storedMeta
	}
	meta.BodyTime = max(meta.BodyTime, r.bodyTime)
	meta.StateTime = max(meta.StateTime, r.stateTime)
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
			return fmt.Errorf("%w: comment root is not archived", errInvalidObject)
		}
		parentRow, ok := existing[parent]
		if !ok {
			return fmt.Errorf("%w: comment parent is not archived", errInvalidObject)
		}
		if rootRow.ConversationID != parentRow.ConversationID || (exists && prior.ConversationID != parentRow.ConversationID) {
			return fmt.Errorf("%w: comment parent belongs to a different conversation", errInvalidObject)
		}
		savedRoot, err := r.imp.store.GetMessageMetadataContext(ctx, rootRow.ID)
		if err != nil {
			return err
		}
		var rootMeta objectMetadata
		if !savedRoot.Valid || json.Unmarshal([]byte(savedRoot.String), &rootMeta) != nil || !rootMeta.matches(r.app, r.page, r.imp.instagram, root) || rootMeta.ParentID != "" {
			return fmt.Errorf("%w: comment root belongs to a different context or has a parent", errInvalidObject)
		}
		if rootMeta.Version != objectMetadataVersion {
			return fmt.Errorf("%w: unsupported comment root metadata version", errInvalidObject)
		}
		if parent != root {
			saved, err := r.imp.store.GetMessageMetadataContext(ctx, parentRow.ID)
			if err != nil {
				return err
			}
			var parentMeta objectMetadata
			if !saved.Valid || json.Unmarshal([]byte(saved.String), &parentMeta) != nil || !parentMeta.matches(r.app, r.page, r.imp.instagram, root) {
				return fmt.Errorf("%w: comment parent belongs to a different conversation", errInvalidObject)
			}
			if parentMeta.Version != objectMetadataVersion {
				return fmt.Errorf("%w: unsupported comment parent metadata version", errInvalidObject)
			}
		}
		reply = sql.NullInt64{Int64: parentRow.ID, Valid: true}
	}
	sender, fromName, self, err := r.messageActor(ctx, obj)
	if err != nil {
		return err
	}
	recipients := []store.RecipientSet{}
	participants := []store.ConversationParticipantRef{}
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
			return fmt.Errorf("%w: invalid comment creation time", errInvalidObject)
		}
		sent = sql.NullTime{Time: parsed, Valid: true}
	} else if exists {
		sent = prior.SentAt
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
	msg := &store.Message{SourceID: r.source.ID, SourceMessageID: obj.ID, MessageType: r.imp.sourceType(),
		SentAt: sent, SenderID: sql.NullInt64{Int64: sender, Valid: sender != 0}, IsFromMe: self,
		ReplyToMessageID: reply, Snippet: sql.NullString{String: obj.Message, Valid: true}, SizeEstimate: int64(len(obj.Message)),
	}
	_, err = r.imp.store.PersistMessageContext(ctx, &store.MessagePersistData{
		Message: msg, Conversation: &store.ConversationPersistData{SourceConversationID: root, ConversationType: "public_comments", Title: title, Participants: participants, PreserveExistingParticipants: true},
		Metadata: &sql.NullString{String: string(metadata), Valid: true}, BodyText: sql.NullString{String: obj.Message, Valid: true},
		RawMIME: raw, RawFormat: r.objectFormat(), Recipients: recipients, PreserveLabels: true,
		FTS: &store.FTSDoc{Body: obj.Message, FromAddr: fromName},
	})
	if err != nil {
		return err
	}
	r.sum.MessagesProcessed++
	if !exists {
		r.sum.MessagesAdded++
	}
	return nil
}

func (imp *Importer) ArchiveReply(ctx context.Context, app, page string, obj Object, root string) (int64, error) {
	if !safeObjectID(obj.ID) || obj.CreatedTime == "" || obj.Parent == nil || !safeObjectID(obj.Parent.ID) {
		return 0, errInvalidObject
	}
	source, err := imp.store.GetSourceByTypeAndIdentifier(SourceType, SourceIdentifier(app, page))
	if err != nil {
		return 0, err
	}
	if source.HistoryOnly || source.MergedIntoSourceID != 0 {
		return 0, store.ErrSourceRetired
	}
	run := importRun{imp: imp, app: app, page: page, source: source, sum: &ImportSummary{}}
	if err = run.persist(ctx, obj, root, "", ""); err != nil {
		return 0, err
	}
	rows, err := imp.store.StoredMessagesContext(ctx, source.ID, rawFormat, []string{obj.ID})
	if err != nil {
		return 0, err
	}
	return rows[obj.ID].ID, nil
}
