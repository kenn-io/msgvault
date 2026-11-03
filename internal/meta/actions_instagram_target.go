package meta

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
)

func resolveInstagramActionTarget(ctx context.Context, st *store.Store, m store.MetaCommentTarget, src *store.Source) (Target, error) {
	var metadata objectMetadata
	if json.Unmarshal([]byte(m.Metadata), &metadata) != nil || metadata.Version != objectMetadataVersion || !ValidAccountID(metadata.AppID) || !ValidAccountID(metadata.PageID) || !ValidAccountID(metadata.InstagramAccountID) || src.Identifier != InstagramSourceIdentifier(metadata.AppID, metadata.PageID, metadata.InstagramAccountID) || !safeObjectID(m.CommentID) || !safeObjectID(metadata.MediaID) || metadata.PostID != "" || metadata.ParentID == "" || metadata.ParentID != m.ParentCommentID {
		return Target{}, actionFailure("stale_identity", nil, nil)
	}
	if !metadata.Organic || metadata.Ad || metadata.Visitor {
		return Target{}, actionFailure("unsupported_source", nil, nil)
	}
	raw, err := st.GetMessageRawContext(ctx, m.MessageID)
	if err != nil {
		return Target{}, err
	}
	var obj IGComment
	if len(raw) > 0 && json.Unmarshal(raw, &obj) != nil {
		return Target{}, actionFailure("stale_identity", nil, nil)
	}
	t := Target{MessageID: m.MessageID, SourceID: m.SourceID, SourceIdentifier: src.Identifier, MessageType: m.MessageType, App: metadata.AppID, Asset: metadata.InstagramAccountID, CommentID: m.CommentID, RootID: metadata.MediaID, ParentID: metadata.ParentID, IsFromMe: m.IsFromMe, Deleted: m.DeletedFromSource, SentAt: m.SentAt.Time, AssetName: src.DisplayName.String}
	if t.AssetName == "" {
		t.AssetName = src.Identifier
	}
	if obj.From != nil {
		t.AuthorID, t.AuthorName = obj.From.ID, obj.From.Username
	}
	if err := restoreArchivedActionAuthor(ctx, st, &t, m.SenderID, InstagramSourceType, instagramActorIdentifier(t.App, metadata.PageID, t.Asset, "ig_comment_from_user", "")); err != nil {
		return t, err
	}
	return t, nil
}
func (a *Actions) instagramActionChecks(ctx context.Context, t Target, kind ActionKind, result *ActionResult) error {
	if t.MessageType != InstagramSourceType {
		return nil
	}
	if kind == ActionHide && t.IsFromMe {
		return actionFailure("not_hideable", result, nil)
	}
	if !isReply(kind) {
		return nil
	}
	m, err := a.st.MetaCommentTargetContext(ctx, t.MessageID)
	if err != nil {
		return err
	}
	if t.ParentID != t.RootID {
		failure := actionFailure("ig_target_not_top_level", result, nil)
		failure.Message = fmt.Sprintf("Reply to the top-level Instagram comment with message ID %d", m.ParentMessageID)
		return failure
	}
	var metadata objectMetadata
	if err = json.Unmarshal([]byte(m.Metadata), &metadata); err != nil {
		return err
	}
	if metadata.Hidden != nil && *metadata.Hidden {
		return actionFailure("target_hidden", result, nil)
	}
	return nil
}

func actionProvider(t Target) string {
	if t.MessageType == InstagramSourceType || t.MessageType == MentionsSourceType {
		return "instagram"
	}
	return "facebook"
}
