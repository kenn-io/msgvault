package meta

import (
	"bytes"
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

var errInvalidObject = errors.New("invalid comment object")

type objectMetadata struct {
	BodyReadRequest    int    `json:"body_read_request,omitempty"`
	BodySyncID         int64  `json:"body_sync_id,omitempty"`
	Hidden             *bool  `json:"hidden,omitempty"`
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

// fallbackConversationTitle provides a readable label without exposing a
// provider object's internal ID.
func fallbackConversationTitle(_ string, instagram bool) string {
	if instagram {
		return "Instagram media"
	}
	return "Facebook post"
}

func (r *importRun) persist(ctx context.Context, obj Object, root, parent string, edge PostEdge) error {
	deleted, err := r.deletedByAction(ctx, obj.ID)
	if err != nil || deleted {
		return err
	}
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
	if r.imp.instagram != "" {
		meta.PostID, meta.MediaID, meta.InstagramAccountID = "", root, r.imp.instagram
	}
	var previousMetadata []byte
	read := responseRead{SyncID: r.readSyncID, Request: r.readRequest}
	if exists {
		savedMeta, err := r.imp.store.GetMessageMetadataContext(ctx, prior.ID)
		if err != nil {
			return err
		}
		var storedMeta objectMetadata
		if !savedMeta.Valid || json.Unmarshal([]byte(savedMeta.String), &storedMeta) != nil || !storedMeta.matches(r.app, r.page, r.imp.instagram, root) || storedMeta.ParentID != parent {
			return errors.New("comment object already belongs to a different root or parent")
		}
		if storedMeta.Version != objectMetadataVersion {
			return errors.New("unsupported comment object metadata version")
		}
		if r.state != nil {
			// Read legacy metadata too, so already cached checkpoints retain
			// their ordering when polling switches to operational markers.
			recorded, err := r.loadResponseRead(ctx, obj.ID, responseRead{SyncID: storedMeta.BodySyncID, Request: storedMeta.BodyReadRequest})
			if err != nil {
				return err
			}
			if recorded.newer(read) {
				obj, err = mergeCachedEvidence(prior, obj)
				if err != nil {
					return err
				}
				read = recorded
			}
		}
		meta = storedMeta
		previousMetadata, err = json.Marshal(storedMeta)
		if err != nil {
			return err
		}
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
	recipients := []store.RecipientSet{}
	participants := []store.ConversationParticipantRef{}
	if exists && prior.SenderID.Valid {
		if sender == 0 {
			sender = prior.SenderID.Int64
			self = prior.SourceIsFromMe.Valid && prior.SourceIsFromMe.Bool
		}
		// Missing author fields do not retract evidence for the same sender.
		if sender == prior.SenderID.Int64 && fromName == "" {
			previous, err := r.imp.store.GetMessagesSummariesByIDsContext(ctx, []int64{prior.ID})
			if err != nil {
				return err
			}
			if len(previous) > 0 {
				fromName = previous[0].FromName
			}
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
			return errors.New("invalid comment creation time")
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
			title = fallbackConversationTitle(root, r.imp.instagram != "")
		}
	}
	msg := &store.Message{SourceID: r.source.ID, SourceMessageID: obj.ID, MessageType: r.imp.sourceType(),
		SentAt: sent, SenderID: sql.NullInt64{Int64: sender, Valid: sender != 0}, IsFromMe: self,
		ReplyToMessageID: reply, Snippet: sql.NullString{String: obj.Message, Valid: true}, SizeEstimate: int64(len(obj.Message)),
	}
	data := &store.MessagePersistData{
		Message: msg, Conversation: &store.ConversationPersistData{SourceConversationID: root, ConversationType: "public_comments", Title: title, Participants: participants, PreserveExistingParticipants: true},
		Metadata: &sql.NullString{String: string(metadata), Valid: true}, BodyText: sql.NullString{String: obj.Message, Valid: true},
		RawMIME: raw, RawFormat: r.objectFormat(), Recipients: recipients, PreserveLabels: true,
		FTS: &store.FTSDoc{Body: obj.Message, FromAddr: fromName},
	}
	if exists {
		unchanged, err := r.snapshotMatches(ctx, prior, data)
		if err != nil {
			return err
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
		if unchanged {
			if err = r.saveResponseRead(ctx, obj.ID, read); err != nil {
				return err
			}
			// Receipt ordering and node freshness remain durable even when the
			// provider returns the same body and author snapshot.
			if !bytes.Equal(previousMetadata, metadata) {
				if err = r.imp.store.SetMessageMetadataContext(ctx, prior.ID, *data.Metadata); err != nil {
					return err
				}
			}
			r.sum.MessagesProcessed++
			return nil
		}
	}
	// Record the observation before content: a failed marker write must not
	// leave a new body exposed to an older response on the next resume.
	if err = r.saveResponseRead(ctx, obj.ID, read); err != nil {
		return err
	}
	_, err = r.imp.store.PersistMessageContext(ctx, data)
	if err != nil {
		return err
	}
	r.sum.MessagesProcessed++
	if !exists {
		r.sum.MessagesAdded++
	} else {
		r.sum.MessagesUpdated++
	}
	return nil
}

// Cached responses can fill missing identity and creation evidence without
// replacing the newer provider body or any already known sender/date.
func mergeCachedEvidence(prior store.StoredMessage, cached Object) (Object, error) {
	var current Object
	var err error
	switch {
	case cached.InstagramMedia != nil:
		media := new(IGMedia)
		err = json.Unmarshal(prior.Raw, media)
		if !prior.SenderID.Valid && (media.Owner == nil || media.Owner.ID == "") {
			media.Owner = cached.InstagramMedia.Owner
		}
		current = Object{ID: media.ID, Message: media.Caption, CreatedTime: media.Timestamp, InstagramMedia: media}
	case cached.InstagramComment != nil:
		comment := new(IGComment)
		err = json.Unmarshal(prior.Raw, comment)
		if !prior.SenderID.Valid && (comment.From == nil || comment.From.ID == "") {
			comment.From = cached.InstagramComment.From
		}
		current = Object{ID: comment.ID, Message: comment.Text, CreatedTime: comment.Timestamp, InstagramComment: comment}
	default:
		err = json.Unmarshal(prior.Raw, &current)
		if !prior.SenderID.Valid && (current.From == nil || current.From.ID == "") {
			current.From = cached.From
		}
	}
	if err != nil || current.ID != cached.ID {
		return Object{}, errors.New("invalid archived object evidence")
	}
	if !prior.SentAt.Valid && current.CreatedTime == "" {
		current.CreatedTime = cached.CreatedTime
		if current.InstagramMedia != nil {
			current.InstagramMedia.Timestamp = cached.CreatedTime
		}
		if current.InstagramComment != nil {
			current.InstagramComment.Timestamp = cached.CreatedTime
		}
	}
	return current, nil
}

func (r *importRun) snapshotMatches(ctx context.Context, prior store.StoredMessage, data *store.MessagePersistData) (bool, error) {
	msg := data.Message
	if prior.SenderID != msg.SenderID || !prior.SourceIsFromMe.Valid || prior.SourceIsFromMe.Bool != msg.IsFromMe ||
		prior.MessageType != msg.MessageType || prior.Subject != msg.Subject || prior.InternalDate != msg.InternalDate ||
		prior.SentAt.Valid != msg.SentAt.Valid || (prior.SentAt.Valid && !prior.SentAt.Time.Equal(msg.SentAt.Time)) ||
		prior.Snippet != msg.Snippet || prior.SizeEstimate != msg.SizeEstimate || !meetingarchive.JSONEvidenceEqual(prior.Raw, data.RawMIME) {
		return false, nil
	}
	matches, err := r.imp.store.MessageContentMatchesContext(ctx, prior.ID, data.BodyText, *data.FTS)
	if err != nil || !matches {
		return matches, err
	}
	return r.imp.store.MessageRecipientsMatchContext(ctx, prior.ID, data.Recipients)
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
