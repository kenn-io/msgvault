package youtube

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
)

type replyMetadata struct {
	Provider         string `json:"provider"`
	ChannelID        string `json:"channel_id"`
	VideoID          string `json:"video_id"`
	CommentID        string `json:"comment_id"`
	ParentID         string `json:"top_level_comment_id"`
	ThreadID         string `json:"thread_id"`
	CanReply         *bool  `json:"can_reply"`
	ModerationStatus string `json:"moderation_status"`
}

func readReplyMetadata(ctx context.Context, st *store.Store, id int64) (replyMetadata, error) {
	raw, err := st.GetMessageMetadataContext(ctx, id)
	if err != nil {
		return replyMetadata{}, err
	}
	var meta replyMetadata
	if !raw.Valid || json.Unmarshal([]byte(raw.String), &meta) != nil || meta.Provider != "youtube" || !validCommentID(meta.CommentID) || !validCommentID(meta.ParentID) {
		return meta, errors.New("message lacks archived YouTube comment identity")
	}
	return meta, nil
}

// ResolveReplyTarget validates local source/video/parent identity and preserves
// the parent's evidence independently of a nested target's moderation status.
// It reads only the archive and makes no provider calls.
func ResolveReplyTarget(ctx context.Context, st *store.Store, messageID int64) (ReplyTarget, error) {
	message, err := st.GetMessageContext(ctx, messageID)
	if err != nil {
		return ReplyTarget{}, fmt.Errorf("read reply target: %w", err)
	}
	if message.DeletedAt != nil {
		return ReplyTarget{}, errors.New("deleted comment is not replyable")
	}
	source, err := st.GetSourceByIDContext(ctx, message.SourceID)
	if err != nil {
		return ReplyTarget{}, err
	}
	if source.SourceType != "youtube" || source.MergedIntoSourceID != 0 {
		return ReplyTarget{}, errors.New("reply target must belong to an active YouTube source")
	}
	meta, err := readReplyMetadata(ctx, st, messageID)
	if err != nil {
		return ReplyTarget{}, err
	}
	if source.Identifier != SourceIdentifier(meta.ChannelID) || message.SourceConversationID != meta.VideoID || message.SourceMessageID != meta.CommentID {
		return ReplyTarget{}, errors.New("YouTube reply target source or video identity mismatch")
	}
	parents, err := st.MessageExistsBatchContext(ctx, source.ID, []string{meta.ParentID})
	if err != nil {
		return ReplyTarget{}, err
	}
	parentID, ok := parents[meta.ParentID]
	if !ok {
		return ReplyTarget{}, errors.New("top-level YouTube parent is not archived")
	}
	parent, err := st.GetMessageContext(ctx, parentID)
	if err != nil {
		return ReplyTarget{}, err
	}
	parentMeta, err := readReplyMetadata(ctx, st, parentID)
	if err != nil {
		return ReplyTarget{}, err
	}
	if parent.DeletedAt != nil || parent.ConversationID != message.ConversationID || parentMeta.ChannelID != meta.ChannelID || parentMeta.VideoID != meta.VideoID || parentMeta.CommentID != meta.ParentID || parentMeta.ParentID != meta.ParentID || parentMeta.CanReply == nil {
		return ReplyTarget{}, errors.New("top-level parent lacks matching ownership or reply eligibility evidence")
	}
	target := ReplyTarget{MessageID: messageID, SourceID: source.ID, ChannelID: meta.ChannelID, VideoID: meta.VideoID, CommentID: meta.CommentID, ParentID: meta.ParentID, ThreadID: parentMeta.ThreadID, CanReply: *parentMeta.CanReply, ParentStatus: parentMeta.ModerationStatus}
	// Reuse validation without creating a receipt or requiring a reply body here.
	if err = validateReplyIdentity(target, "preview"); err != nil {
		return ReplyTarget{}, err
	}
	return target, nil
}
