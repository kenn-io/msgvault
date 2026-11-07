package youtube

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type commentMetadata struct {
	Provider          string    `json:"provider"`
	ChannelID         string    `json:"channel_id"`
	VideoID           string    `json:"video_id"`
	CommentID         string    `json:"comment_id,omitempty"`
	TopLevelCommentID string    `json:"top_level_comment_id,omitempty"`
	ThreadID          string    `json:"thread_id,omitempty"`
	CanReply          *bool     `json:"can_reply,omitempty"`
	ModerationStatus  string    `json:"moderation_status,omitempty"`
	StatusObservedAt  time.Time `json:"status_observed_at,omitzero"`
	PrivacyStatus     string    `json:"privacy_status,omitempty"`
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

func persistVideo(ctx context.Context, st *store.Store, sourceID int64, channelID string, v Video) (int64, messageChange, error) {
	if !validID(v.ID) || v.Snippet.ChannelID != channelID {
		return 0, messageUnchanged, errors.New("YouTube video does not belong to configured channel")
	}
	pid, err := st.EnsureParticipantByIdentifierContext(ctx, SourceType, SourceIdentifier(channelID), "")
	if err != nil {
		return 0, messageUnchanged, err
	}
	meta, err := json.Marshal(commentMetadata{Provider: SourceType, ChannelID: channelID, VideoID: v.ID, PrivacyStatus: v.Status.PrivacyStatus})
	if err != nil {
		return 0, messageUnchanged, err
	}
	body := v.Snippet.Description + "\n\nhttps://www.youtube.com/watch?v=" + v.ID
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
		Message:      &store.Message{SourceID: sourceID, SourceMessageID: "video:" + v.ID, MessageType: SourceType, SenderID: sql.NullInt64{Int64: pid, Valid: true}, IsFromMe: true, SentAt: sql.NullTime{Time: v.Snippet.PublishedAt, Valid: !v.Snippet.PublishedAt.IsZero()}, Subject: sql.NullString{String: v.Snippet.Title, Valid: true}, Snippet: sql.NullString{String: snippet(body), Valid: true}, SizeEstimate: int64(len(body)), PreserveAttachmentStats: true},
		Conversation: &store.ConversationPersistData{SourceConversationID: v.ID, ConversationType: "group_chat", Title: v.Snippet.Title, Participants: []store.ConversationParticipantRef{{ParticipantID: pid, Role: "member"}}, PreserveExistingParticipants: true},
		Metadata:     &sql.NullString{String: string(meta), Valid: true}, BodyText: sql.NullString{String: body, Valid: true}, RawMIME: raw, RawFormat: "youtube_json", LabelIDs: labelIDs,
		FTS: &store.FTSDoc{Subject: v.Snippet.Title, Body: body}, Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{pid}}},
	}
	storedMessages, err := st.StoredMessagesContext(ctx, sourceID, data.RawFormat, []string{"video:" + v.ID})
	if err != nil {
		return 0, messageUnchanged, err
	}
	change := messageAdded
	if stored, ok := storedMessages["video:"+v.ID]; ok {
		change = messageUpdated
		conversationID, err := st.EnsureConversation(ctx, sourceID, v.ID, v.Snippet.Title)
		if err != nil {
			return 0, messageUnchanged, err
		}
		matches, _, err := youtubeMessageSnapshotMatches(ctx, st, sourceID, v.ID, conversationID, data, stored)
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
) (matches, matchesForStats bool, err error) {
	message := *data.Message
	message.ConversationID = conversationID
	messageMatches := stored.Matches(&message, data.RawMIME) &&
		stored.Subject == message.Subject &&
		stored.ReceivedAt == message.ReceivedAt &&
		stored.ReplyToMessageID == message.ReplyToMessageID

	recipientsMatch, err := st.MessageRecipientsMatchContext(ctx, stored.ID, data.Recipients)
	if err != nil {
		return false, false, err
	}
	if recipientsMatch {
		data.Recipients = nil
	}
	labels, err := st.MessageLabelIDsContext(ctx, stored.ID)
	if err != nil {
		return false, false, err
	}
	wantedLabels := slices.Clone(data.LabelIDs)
	slices.Sort(labels)
	slices.Sort(wantedLabels)
	labelsMatch := slices.Equal(labels, wantedLabels)
	if labelsMatch {
		data.PreserveLabels = true
	}
	if !messageMatches {
		return false, false, nil
	}

	contentMatches, err := st.MessageContentMatchesContext(ctx, stored.ID, data.BodyText, *data.FTS)
	if err != nil {
		return false, false, err
	}
	metadataMatches := true
	metadataMatchesForStats := true
	if data.Metadata != nil {
		metadata, err := st.GetMessageMetadataContext(ctx, stored.ID)
		if err != nil {
			return false, false, err
		}
		metadataMatches = metadata == *data.Metadata || youtubeMetadataMatches(metadata, *data.Metadata, false)
		metadataMatchesForStats = metadataMatches || youtubeMetadataMatches(metadata, *data.Metadata, true)
	}

	titles, err := st.ConversationTitlesBatchContext(ctx, sourceID, []string{sourceConversationID})
	if err != nil {
		return false, false, err
	}
	titleMatches := data.Conversation == nil || data.Conversation.Title == "" || titles[sourceConversationID] == data.Conversation.Title
	return messageMatches && contentMatches && metadataMatches && recipientsMatch && labelsMatch && titleMatches,
		messageMatches && contentMatches && metadataMatchesForStats && recipientsMatch && labelsMatch && titleMatches, nil
}

// PostgreSQL JSONB may reorder object keys and whitespace, so compare the
// decoded YouTube metadata values rather than their serialized strings.
func youtubeMetadataMatches(previous, current sql.NullString, ignoreStatusObservation bool) bool {
	if !previous.Valid || !current.Valid {
		return previous == current
	}
	var before, after commentMetadata
	if json.Unmarshal([]byte(previous.String), &before) != nil || json.Unmarshal([]byte(current.String), &after) != nil {
		return false
	}
	if before.Provider != SourceType || after.Provider != SourceType {
		return false
	}
	return before.Provider == after.Provider &&
		before.ChannelID == after.ChannelID &&
		before.VideoID == after.VideoID &&
		before.CommentID == after.CommentID &&
		before.TopLevelCommentID == after.TopLevelCommentID &&
		before.ThreadID == after.ThreadID &&
		boolPointersEqual(before.CanReply, after.CanReply) &&
		before.ModerationStatus == after.ModerationStatus &&
		before.PrivacyStatus == after.PrivacyStatus &&
		(ignoreStatusObservation || before.StatusObservedAt.Equal(after.StatusObservedAt))
}

func boolPointersEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func snippet(s string) string { r := []rune(strings.TrimSpace(s)); return string(r[:min(len(r), 200)]) }

// PersistComment archives an individual comment and refreshes its conversation
// statistics. Import pages use persistComment and refresh once after the page.
func PersistComment(ctx context.Context, st *store.Store, sourceID int64, channelID string, v Video, c Comment, status string) (int64, error) {
	id, _, err := persistComment(ctx, st, sourceID, channelID, v, c)
	if err != nil {
		return id, err
	}
	return id, st.RecomputeConversationStatsForMessageContext(ctx, id)
}

// persistComment stores a comment without recomputing statistics. Import pages
// call it repeatedly, then refresh their one affected video conversation once.
func persistComment(ctx context.Context, st *store.Store, sourceID int64, channelID string, v Video, c Comment) (int64, messageChange, error) {
	if !validCommentID(c.ID) || !validID(v.ID) || !validID(channelID) || c.Snippet.VideoID != "" && c.Snippet.VideoID != v.ID {
		return 0, messageUnchanged, errors.New("invalid YouTube comment identity")
	}
	source, err := st.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return 0, messageUnchanged, err
	}
	if source.SourceType != SourceType || source.Identifier != SourceIdentifier(channelID) {
		return 0, messageUnchanged, errors.New("YouTube comment source mismatch")
	}
	parent := "video:" + v.ID
	top := c.ID
	if c.Snippet.ParentID != "" {
		if !validCommentID(c.Snippet.ParentID) {
			return 0, messageUnchanged, errors.New("invalid YouTube comment parent")
		}
		parent = c.Snippet.ParentID
		top = parent
	}
	meta := commentMetadata{Provider: SourceType, ChannelID: channelID, VideoID: v.ID, CommentID: c.ID, TopLevelCommentID: top, ThreadID: c.ThreadID, CanReply: c.CanReply}
	prior, err := st.MessageMetadataBatch(ctx, sourceID, []string{c.ID, parent, "video:" + v.ID})
	if err != nil {
		return 0, messageUnchanged, err
	}
	var previous commentMetadata
	previousExists := false
	if old, ok := prior[c.ID]; ok {
		if json.Unmarshal([]byte(old.Metadata.String), &previous) != nil {
			return 0, messageUnchanged, errors.New("invalid archived YouTube metadata")
		}
		previousExists = true
		if previous.VideoID != v.ID || previous.ChannelID != channelID || previous.TopLevelCommentID != top {
			return 0, messageUnchanged, errors.New("YouTube comment identity changed")
		}
		meta.ModerationStatus = previous.ModerationStatus
		meta.StatusObservedAt = previous.StatusObservedAt
		if meta.ThreadID == "" {
			meta.ThreadID = previous.ThreadID
		}
		if meta.CanReply == nil {
			meta.CanReply = previous.CanReply
		}
	}
	if parent == c.ID {
		return 0, messageUnchanged, errors.New("YouTube comment cannot parent itself")
	}
	parentRow, ok := prior[parent]
	if !ok {
		return 0, messageUnchanged, errors.New("YouTube comment parent is not archived")
	}
	var parentMeta commentMetadata
	if json.Unmarshal([]byte(parentRow.Metadata.String), &parentMeta) != nil || parentMeta.VideoID != v.ID || parentMeta.ChannelID != channelID {
		return 0, messageUnchanged, errors.New("YouTube comment parent source/video mismatch")
	}
	if c.Snippet.ParentID != "" && (parentMeta.CommentID != parent || parentMeta.TopLevelCommentID != parent) {
		return 0, messageUnchanged, errors.New("YouTube reply parent is not a top-level comment")
	}
	storedMessages, err := st.StoredMessagesContext(ctx, sourceID, "youtube_json", []string{"video:" + v.ID, c.ID})
	if err != nil {
		return 0, messageUnchanged, err
	}
	root, ok := storedMessages["video:"+v.ID]
	if !ok {
		return 0, messageUnchanged, errors.New("YouTube video root is not archived")
	}
	v.Snippet.Title = root.Subject.String
	switch c.Snippet.ModerationStatus {
	case "published", "heldForReview", "likelySpam", "rejected":
		meta.ModerationStatus = c.Snippet.ModerationStatus
		meta.StatusObservedAt = time.Now().UTC()
	case "":
	default:
		return 0, messageUnchanged, errors.New("unknown YouTube moderation status")
	}
	var pid int64
	if c.Snippet.AuthorChannelID.Value != "" {
		key := SourceIdentifier(c.Snippet.AuthorChannelID.Value)
		if key == "" {
			return 0, messageUnchanged, errors.New("invalid YouTube author identity")
		}
		pid, err = st.EnsureParticipantByIdentifierContext(ctx, SourceType, key, c.Snippet.AuthorDisplayName)
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
		recipients[0].DisplayNames = []string{c.Snippet.AuthorDisplayName}
		members = append(members, store.ConversationParticipantRef{ParticipantID: pid, Role: "member"})
	}
	data := &store.MessagePersistData{
		Message:      &store.Message{SourceID: sourceID, SourceMessageID: c.ID, MessageType: SourceType, SentAt: sql.NullTime{Time: c.Snippet.PublishedAt, Valid: !c.Snippet.PublishedAt.IsZero()}, SenderID: sql.NullInt64{Int64: pid, Valid: pid != 0}, ReplyToMessageID: sql.NullInt64{Int64: parentRow.ID, Valid: true}, IsFromMe: c.Snippet.AuthorChannelID.Value == channelID, Subject: sql.NullString{String: v.Snippet.Title, Valid: true}, Snippet: sql.NullString{String: snippet(text), Valid: text != ""}, SizeEstimate: int64(len(text)), PreserveAttachmentStats: true},
		Conversation: &store.ConversationPersistData{SourceConversationID: v.ID, ConversationType: "group_chat", Title: v.Snippet.Title, Participants: members, PreserveExistingParticipants: true}, Metadata: &sql.NullString{String: string(encoded), Valid: true}, BodyText: sql.NullString{String: text, Valid: text != ""}, RawMIME: raw, RawFormat: "youtube_json", LabelIDs: labelIDs, Recipients: recipients, FTS: &store.FTSDoc{Subject: v.Snippet.Title, Body: text, FromAddr: c.Snippet.AuthorDisplayName},
	}
	replyToNeedsRepair := false
	change := messageAdded
	if stored, ok := storedMessages[c.ID]; ok {
		change = messageUpdated
		replyToNeedsRepair = stored.ReplyToMessageID != data.Message.ReplyToMessageID
		matches, matchesForStats, err := youtubeMessageSnapshotMatches(ctx, st, sourceID, v.ID, root.ConversationID, data, stored)
		if err != nil {
			return 0, messageUnchanged, err
		}
		if matchesForStats {
			for _, member := range members {
				if err := st.EnsureConversationParticipantContext(ctx, root.ConversationID, member.ParticipantID, member.Role); err != nil {
					return 0, messageUnchanged, err
				}
			}
			if !matches {
				// Only the observation time differs; keep content snapshots untouched.
				if err := st.SetMessageMetadataContext(ctx, stored.ID, *data.Metadata); err != nil {
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
		if err := st.SetReplyToContext(ctx, sourceID, c.ID, parent); err != nil {
			return id, change, err
		}
	}
	return id, change, nil
}
