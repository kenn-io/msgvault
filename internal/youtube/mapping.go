package youtube

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	yt "google.golang.org/api/youtube/v3"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

type commentMetadata struct {
	Provider          string `json:"provider"`
	ChannelID         string `json:"channel_id"`
	VideoID           string `json:"video_id"`
	CommentID         string `json:"comment_id,omitempty"`
	TopLevelCommentID string `json:"top_level_comment_id,omitempty"`
	ModerationStatus  string `json:"moderation_status,omitempty"`
	PrivacyStatus     string `json:"privacy_status,omitempty"`
}

type messageChange uint8

const (
	messageUnchanged messageChange = iota
	messageAdded
	messageUpdated
)

func validCommentID(id string) bool {
	if id == "" || len(id) > 512 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

func label(kind, value string) store.MessageLabelRef {
	name := "YouTube " + kind + ": " + value
	if kind == "moderation" {
		switch value {
		case "heldForReview":
			name = "YouTube: held for review"
		case "likelySpam":
			name = "YouTube: likely spam"
		}
	}
	return store.MessageLabelRef{SourceLabelID: "youtube:" + kind + ":" + value, Info: store.LabelInfo{Name: name, Type: "user"}}
}

func resolveLabels(ctx context.Context, st *store.Store, sourceID int64, refs []store.MessageLabelRef) ([]int64, error) {
	labels := map[string]store.LabelInfo{}
	for _, ref := range refs {
		labels[ref.SourceLabelID] = ref.Info
	}
	resolved, err := st.EnsureLabelsBatch(ctx, sourceID, labels)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, resolved[ref.SourceLabelID])
	}
	return ids, nil
}

func persistVideoSDK(ctx context.Context, st *store.Store, sourceID int64, channelID string, v *yt.Video) (int64, messageChange, error) {
	if v.Status == nil {
		return 0, messageUnchanged, errors.New("YouTube video status is missing")
	}
	published, err := providerTime(v.Snippet.PublishedAt)
	if err != nil {
		return 0, messageUnchanged, err
	}
	pid, err := st.EnsureParticipantByIdentifierContext(ctx, SourceType, SourceIdentifier(channelID), "")
	if err != nil {
		return 0, messageUnchanged, err
	}
	meta, err := json.Marshal(commentMetadata{Provider: SourceType, ChannelID: channelID, VideoID: v.Id, PrivacyStatus: v.Status.PrivacyStatus})
	if err != nil {
		return 0, messageUnchanged, err
	}
	body := v.Snippet.Description + "\n\nhttps://www.youtube.com/watch?v=" + v.Id
	raw, err := json.Marshal(v)
	if err != nil {
		return 0, messageUnchanged, err
	}
	labels := []store.MessageLabelRef{}
	if v.Status.PrivacyStatus != "" {
		labels = append(labels, label("privacy", v.Status.PrivacyStatus))
	}
	labelIDs, err := resolveLabels(ctx, st, sourceID, labels)
	if err != nil {
		return 0, messageUnchanged, err
	}
	data := &store.MessagePersistData{
		Message:      &store.Message{SourceID: sourceID, SourceMessageID: "video:" + v.Id, MessageType: SourceType, SenderID: sql.NullInt64{Int64: pid, Valid: true}, IsFromMe: true, SentAt: sql.NullTime{Time: published, Valid: !published.IsZero()}, Subject: sql.NullString{String: v.Snippet.Title, Valid: true}, Snippet: sql.NullString{String: meetingarchive.Snippet(body), Valid: true}, SizeEstimate: int64(len(body)), PreserveAttachmentStats: true},
		Conversation: &store.ConversationPersistData{SourceConversationID: v.Id, ConversationType: "group_chat", Title: v.Snippet.Title, Participants: []store.ConversationParticipantRef{{ParticipantID: pid, Role: "member"}}, PreserveExistingParticipants: true},
		Metadata:     &sql.NullString{String: string(meta), Valid: true}, BodyText: sql.NullString{String: body, Valid: true}, RawMIME: raw, RawFormat: "youtube_json", LabelIDs: labelIDs,
		FTS: &store.FTSDoc{Subject: v.Snippet.Title, Body: body}, Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{pid}}},
	}
	storedMessages, err := st.StoredMessagesContext(ctx, sourceID, data.RawFormat, []string{"video:" + v.Id})
	if err != nil {
		return 0, messageUnchanged, err
	}
	change := messageAdded
	if stored, ok := storedMessages["video:"+v.Id]; ok {
		change = messageUpdated
		conversationID, err := st.EnsureConversation(ctx, sourceID, v.Id, v.Snippet.Title)
		if err != nil {
			return 0, messageUnchanged, err
		}
		matches, err := youtubeMessageSnapshotMatches(ctx, st, sourceID, v.Id, conversationID, data, stored)
		if err != nil {
			return 0, messageUnchanged, err
		}
		if matches {
			if err := st.EnsureConversationParticipantContext(ctx, conversationID, pid, "member"); err != nil {
				return 0, messageUnchanged, err
			}
			return stored.ID, messageUnchanged, nil
		}
	}
	id, err := st.PersistMessageContext(ctx, data)
	if err != nil {
		return 0, messageUnchanged, err
	}
	return id, change, nil
}

func youtubeMessageSnapshotMatches(
	ctx context.Context,
	st *store.Store,
	sourceID int64,
	sourceConversationID string,
	conversationID int64,
	data *store.MessagePersistData,
	stored store.StoredMessage,
) (bool, error) {
	message := *data.Message
	message.ConversationID = conversationID
	messageMatches := stored.Matches(&message, data.RawMIME) &&
		stored.Subject == message.Subject &&
		stored.ReplyToMessageID == message.ReplyToMessageID

	recipientsMatch, err := st.MessageRecipientsMatchContext(ctx, stored.ID, data.Recipients)
	if err != nil {
		return false, err
	}
	if recipientsMatch {
		data.Recipients = nil
	}
	labels, err := st.MessageLabelIDsContext(ctx, stored.ID)
	if err != nil {
		return false, err
	}
	wantedLabels := slices.Clone(data.LabelIDs)
	slices.Sort(labels)
	slices.Sort(wantedLabels)
	labelsMatch := slices.Equal(labels, wantedLabels)
	if labelsMatch {
		data.PreserveLabels = true
	}
	if !messageMatches {
		return false, nil
	}

	contentMatches, err := st.MessageContentMatchesContext(ctx, stored.ID, data.BodyText, *data.FTS)
	if err != nil {
		return false, err
	}
	metadataMatches := true
	if data.Metadata != nil {
		metadata, err := st.GetMessageMetadataContext(ctx, stored.ID)
		if err != nil {
			return false, err
		}
		metadataMatches = metadata.Valid == data.Metadata.Valid && (!metadata.Valid || meetingarchive.JSONEvidenceEqual([]byte(metadata.String), []byte(data.Metadata.String)))
	}

	titles, err := st.ConversationTitlesBatchContext(ctx, sourceID, []string{sourceConversationID})
	if err != nil {
		return false, err
	}
	titleMatches := data.Conversation == nil || data.Conversation.Title == "" || titles[sourceConversationID] == data.Conversation.Title
	return messageMatches && contentMatches && metadataMatches && recipientsMatch && labelsMatch && titleMatches, nil
}

// persistComment stores a comment without recomputing statistics. Import pages
// call it repeatedly, then refresh their one affected video conversation once.
func persistCommentSDK(ctx context.Context, st *store.Store, sourceID int64, channelID string, videoID string, c *yt.Comment) (int64, messageChange, error) {
	if c == nil || c.Snippet == nil || !validCommentID(c.Id) || c.Snippet.VideoId != "" && c.Snippet.VideoId != videoID {
		return 0, messageUnchanged, errors.New("invalid YouTube comment identity")
	}
	parent := "video:" + videoID
	top := c.Id
	if c.Snippet.ParentId != "" {
		if !validCommentID(c.Snippet.ParentId) {
			return 0, messageUnchanged, errors.New("invalid YouTube comment parent")
		}
		parent = c.Snippet.ParentId
		top = parent
	}
	meta := commentMetadata{Provider: SourceType, ChannelID: channelID, VideoID: videoID, CommentID: c.Id, TopLevelCommentID: top}
	prior, err := st.MessageMetadataBatch(ctx, sourceID, []string{c.Id, parent, "video:" + videoID})
	if err != nil {
		return 0, messageUnchanged, err
	}
	var previous commentMetadata
	previousExists := false
	if old, ok := prior[c.Id]; ok {
		if json.Unmarshal([]byte(old.Metadata.String), &previous) != nil {
			return 0, messageUnchanged, errors.New("invalid archived YouTube metadata")
		}
		previousExists = true
		if previous.VideoID != videoID || previous.ChannelID != channelID || previous.TopLevelCommentID != top {
			return 0, messageUnchanged, errors.New("YouTube comment identity changed")
		}
		meta.ModerationStatus = previous.ModerationStatus
	}
	if parent == c.Id {
		return 0, messageUnchanged, errors.New("YouTube comment cannot parent itself")
	}
	parentRow, ok := prior[parent]
	if !ok {
		return 0, messageUnchanged, errors.New("YouTube comment parent is not archived")
	}
	storedMessages, err := st.StoredMessagesContext(ctx, sourceID, "youtube_json", []string{"video:" + videoID, c.Id})
	if err != nil {
		return 0, messageUnchanged, err
	}
	root, ok := storedMessages["video:"+videoID]
	if !ok {
		return 0, messageUnchanged, errors.New("YouTube video root is not archived")
	}
	title := root.Subject.String
	switch c.Snippet.ModerationStatus {
	case "published", "heldForReview", "likelySpam", "rejected":
		meta.ModerationStatus = c.Snippet.ModerationStatus
	}
	published, err := providerTime(c.Snippet.PublishedAt)
	if err != nil {
		return 0, messageUnchanged, err
	}
	authorID := ""
	if c.Snippet.AuthorChannelId != nil {
		authorID = c.Snippet.AuthorChannelId.Value
	}
	var pid int64
	fromName := c.Snippet.AuthorDisplayName
	self := authorID == channelID
	if prior, ok := storedMessages[c.Id]; ok && prior.SenderID.Valid && fromName == "" {
		emptyName, err := st.MessageRecipientsMatchContext(ctx, prior.ID, []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{prior.SenderID.Int64}, DisplayNames: []string{""}}})
		if err != nil {
			return 0, messageUnchanged, err
		}
		if !emptyName {
			summaries, err := st.GetMessagesSummariesByIDsContext(ctx, []int64{prior.ID})
			if err != nil {
				return 0, messageUnchanged, err
			}
			if len(summaries) > 0 {
				fromName = summaries[0].FromName
			}
		}
	}
	if authorID != "" {
		key := SourceIdentifier(authorID)
		if key == "" {
			return 0, messageUnchanged, errors.New("invalid YouTube author identity")
		}
		pid, err = st.EnsureParticipantByIdentifierContext(ctx, SourceType, key, fromName)
		if err != nil {
			return 0, messageUnchanged, err
		}
	}
	if prior, ok := storedMessages[c.Id]; ok && prior.SenderID.Valid {
		if pid == 0 {
			pid = prior.SenderID.Int64
			self = prior.SourceIsFromMe.Valid && prior.SourceIsFromMe.Bool
		} else if pid != prior.SenderID.Int64 {
			key := SourceIdentifier(channelID) + "/comment/" + c.Id
			anonymous, _, err := st.ParticipantByIdentifierContext(ctx, "youtube_comment_author", key)
			if err != nil {
				return 0, messageUnchanged, err
			}
			if anonymous == prior.SenderID.Int64 {
				identity, err := st.GetParticipantIdentityContext(ctx, []int64{anonymous})
				if err != nil {
					return 0, messageUnchanged, err
				}
				if len(identity.Identifiers) == 1 {
					if err := st.MergeParticipantsContext(ctx, anonymous, pid); err != nil {
						return 0, messageUnchanged, err
					}
				}
			}
		}
	}
	if pid == 0 && fromName != "" {
		// A comment-scoped participant keeps the displayed name without linking
		// separate comments whose authors have no channel ID.
		key := SourceIdentifier(channelID) + "/comment/" + c.Id
		pid, err = st.EnsureParticipantByIdentifierContext(ctx, "youtube_comment_author", key, fromName)
		if err != nil {
			return 0, messageUnchanged, err
		}
	}

	text := c.Snippet.TextOriginal
	if text == "" {
		text = c.Snippet.TextDisplay
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return 0, messageUnchanged, err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return 0, messageUnchanged, err
	}
	labels := []store.MessageLabelRef{}
	if meta.ModerationStatus != "" {
		labels = append(labels, label("moderation", meta.ModerationStatus))
	}
	labelIDs, err := resolveLabels(ctx, st, sourceID, labels)
	if err != nil {
		return 0, messageUnchanged, err
	}
	recipients := []store.RecipientSet{{Type: "from"}}
	members := []store.ConversationParticipantRef{}
	if pid != 0 {
		recipients[0].ParticipantIDs = []int64{pid}
		recipients[0].DisplayNames = []string{fromName}
		members = append(members, store.ConversationParticipantRef{ParticipantID: pid, Role: "member"})
	}
	data := &store.MessagePersistData{
		Message:      &store.Message{SourceID: sourceID, SourceMessageID: c.Id, MessageType: SourceType, SentAt: sql.NullTime{Time: published, Valid: !published.IsZero()}, SenderID: sql.NullInt64{Int64: pid, Valid: pid != 0}, ReplyToMessageID: sql.NullInt64{Int64: parentRow.ID, Valid: true}, IsFromMe: self, Subject: sql.NullString{String: title, Valid: true}, Snippet: sql.NullString{String: meetingarchive.Snippet(text), Valid: text != ""}, SizeEstimate: int64(len(text)), PreserveAttachmentStats: true},
		Conversation: &store.ConversationPersistData{SourceConversationID: videoID, ConversationType: "group_chat", Title: title, Participants: members, PreserveExistingParticipants: true}, Metadata: &sql.NullString{String: string(encoded), Valid: true}, BodyText: sql.NullString{String: text, Valid: text != ""}, RawMIME: raw, RawFormat: "youtube_json", LabelIDs: labelIDs, Recipients: recipients, FTS: &store.FTSDoc{Subject: title, Body: text, FromAddr: fromName},
	}
	replyToNeedsRepair := false
	change := messageAdded
	if stored, ok := storedMessages[c.Id]; ok {
		change = messageUpdated
		replyToNeedsRepair = stored.ReplyToMessageID != data.Message.ReplyToMessageID
		matches, err := youtubeMessageSnapshotMatches(ctx, st, sourceID, videoID, root.ConversationID, data, stored)
		if err != nil {
			return 0, messageUnchanged, err
		}
		if matches {
			for _, member := range members {
				if err := st.EnsureConversationParticipantContext(ctx, root.ConversationID, member.ParticipantID, member.Role); err != nil {
					return 0, messageUnchanged, err
				}
			}

			return stored.ID, messageUnchanged, nil
		}
	} else if previousExists {
		change = messageUpdated
	}
	id, err := st.PersistMessageContext(ctx, data)
	if err != nil {
		return 0, messageUnchanged, err
	}
	if replyToNeedsRepair {
		if err := st.SetReplyToContext(ctx, sourceID, c.Id, parent); err != nil {
			return id, change, err
		}
	}
	return id, change, nil
}

func providerTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, errors.New("invalid YouTube timestamp")
	}
	return parsed, nil
}
