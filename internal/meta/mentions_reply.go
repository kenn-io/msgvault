package meta

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/url"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type MentionTarget struct {
	IG, MediaID, CommentID string
	SourceID, MessageID    int64
}

func mentionActionCheck(action ActionKind) error {
	if action == ActionPublicReply {
		return nil
	}
	failure := actionFailure("unsupported_source", nil, nil)
	failure.Message = "only public replies are supported for Instagram mentions"
	if action == ActionPrivateReply {
		failure.Message = "private replies are not supported for Instagram mentions"
	}
	return failure
}

func mentionTarget(t Target) MentionTarget {
	comment := t.CommentID
	if comment == t.RootID {
		comment = ""
	}
	return MentionTarget{IG: t.Asset, MediaID: t.RootID, CommentID: comment, SourceID: t.SourceID, MessageID: t.MessageID}
}

func MentionReplyTarget(ctx context.Context, st *store.Store, messageID int64, action ActionKind) (MentionTarget, error) {
	if err := mentionActionCheck(action); err != nil {
		return MentionTarget{}, err
	}
	t, err := ResolveTarget(ctx, st, messageID)
	if err != nil {
		return MentionTarget{}, err
	}
	if t.MessageType != MentionsSourceType {
		return MentionTarget{}, actionFailure("unsupported_source", nil, nil)
	}
	return mentionTarget(t), nil
}

func validMentionProvenance(m mentionMetadata, source string) bool {
	return m.Version == 1 && ValidAccountID(m.AppID) && ValidAccountID(m.PageID) && ValidAccountID(m.IGUserID) && ValidAccountID(m.MediaID) && source == InstagramSourceIdentifier(m.AppID, m.PageID, m.IGUserID)
}

func resolveMentionActionTarget(m store.MetaCommentTarget, src *store.Source, allowDeleted bool) (Target, error) {
	if src.HistoryOnly || src.MergedIntoSourceID != 0 {
		return Target{}, actionFailure("stale_identity", nil, store.ErrSourceRetired)
	}
	var metadata mentionMetadata
	if m.MessageType != MentionsSourceType || src.SourceType != MentionsSourceType || json.Unmarshal([]byte(m.Metadata), &metadata) != nil || !validMentionProvenance(metadata, src.Identifier) || metadata.Reply || m.IsFromMe {
		return Target{}, actionFailure("stale_identity", nil, nil)
	}
	if m.DeletedFromSource && !allowDeleted {
		return Target{}, actionFailure("target_deleted", nil, nil)
	}
	if metadata.CommentID == "" {
		if m.CommentID != metadata.MediaID || m.ParentMessageID != 0 {
			return Target{}, actionFailure("stale_identity", nil, nil)
		}
		if !metadata.CaptionMention {
			failure := actionFailure("unsupported_source", nil, nil)
			failure.Message = "Instagram does not support commenting on tagged photos"
			return Target{}, failure
		}
	} else if !ValidAccountID(metadata.CommentID) || !metadata.CommentMention || m.CommentID != metadata.CommentID || m.ParentMessageID == 0 || m.ParentCommentID != metadata.MediaID {
		return Target{}, actionFailure("stale_identity", nil, nil)
	}
	name := src.DisplayName.String
	if name == "" {
		name = src.Identifier
	}
	return Target{MessageID: m.MessageID, SourceID: m.SourceID, SourceIdentifier: src.Identifier, MessageType: MentionsSourceType, App: metadata.AppID, Asset: metadata.IGUserID, RootID: metadata.MediaID, CommentID: m.CommentID, ParentID: m.ParentCommentID, AssetName: name, Deleted: m.DeletedFromSource, SentAt: m.SentAt.Time}, nil
}

func (c *Client) ReplyToMention(ctx context.Context, target MentionTarget, message string) (string, error) {
	if !ValidAccountID(target.IG) || !ValidAccountID(target.MediaID) || (target.CommentID != "" && !ValidAccountID(target.CommentID)) {
		return "", errors.New("invalid Instagram mention reply identity")
	}
	form := url.Values{"media_id": {target.MediaID}, "message": {message}}
	if target.CommentID != "" {
		form.Set("comment_id", target.CommentID)
	}
	var out struct {
		ID string `json:"id"`
	}
	err := c.post(ctx, target.IG+"/mentions", form, &out)
	if err == nil && (!ValidAccountID(out.ID) || out.ID == target.MediaID || out.ID == target.CommentID) {
		return "", errors.New("instagram mention reply acceptance is unknown")
	}
	return out.ID, err
}

func persistMentionReply(ctx context.Context, st *store.Store, scope mentionScope, target MentionTarget, id, message string, sentAt time.Time) (int64, error) {
	if !ValidAccountID(id) || id == target.MediaID || id == target.CommentID || scope.SourceID != target.SourceID || scope.IG != target.IG {
		return 0, errors.New("invalid Instagram mention reply identity")
	}
	m, err := st.MetaCommentTargetContext(ctx, target.MessageID)
	if err != nil {
		return 0, err
	}
	source, err := st.GetSourceByIDContext(ctx, scope.SourceID)
	if err != nil {
		return 0, err
	}
	bound, err := resolveMentionActionTarget(m, source, true)
	if err != nil {
		return 0, err
	}
	if mentionTarget(bound) != target || source.Identifier != InstagramSourceIdentifier(scope.App, scope.Page, scope.IG) {
		return 0, actionFailure("stale_identity", nil, nil)
	}
	parent := target.MediaID
	if target.CommentID != "" {
		parent = target.CommentID
	}
	metadata := mentionMetadata{Version: 1, AppID: scope.App, PageID: scope.Page, IGUserID: scope.IG, MediaID: target.MediaID, CommentID: id, ParentKnown: true, Reply: true}
	obj := Object{ID: id, Message: message, CreatedTime: sentAt.UTC().Format(time.RFC3339), InstagramMedia: &IGMedia{Owner: &IGUser{ID: scope.IG, Username: source.DisplayName.String}}}
	projection := &persistOptions{SourceType: MentionsSourceType, RawFormat: mentionRawFormat, Raw: obj, FromAddr: source.DisplayName.String}
	projection.Metadata = func(ctx context.Context, row int64, object, parentID string) ([]byte, error) {
		if object == target.MediaID && parentID == "" {
			saved, err := st.GetMessageMetadataContext(ctx, row)
			if err != nil {
				return nil, err
			}
			var root mentionMetadata
			if json.Unmarshal([]byte(saved.String), &root) != nil || !validMentionProvenance(root, source.Identifier) || root.MediaID != target.MediaID || root.CommentID != "" || root.Reply {
				return nil, actionFailure("stale_identity", nil, nil)
			}
			return []byte(saved.String), nil
		}
		if object != id || parentID != parent {
			return nil, actionFailure("stale_identity", nil, nil)
		}
		if row != 0 {
			old, err := st.MetaCommentTargetContext(ctx, row)
			if err != nil {
				return nil, err
			}
			var prior mentionMetadata
			if json.Unmarshal([]byte(old.Metadata), &prior) != nil || !validMentionProvenance(prior, source.Identifier) || prior.MediaID != target.MediaID || prior.CommentID != id || !prior.Reply || old.ParentCommentID != parent || !old.IsFromMe {
				return nil, actionFailure("stale_identity", nil, nil)
			}
		}
		return json.Marshal(metadata)
	}
	run := &importRun{imp: &Importer{store: st, instagram: scope.IG}, source: source, app: scope.App, page: scope.Page, sum: &ImportSummary{}}
	if err := run.persist(ctx, obj, target.MediaID, parent, "", projection); err != nil {
		return 0, err
	}
	return projection.MessageID, nil
}

func (a *Actions) dispatchMentionReply(ctx context.Context, t Target, req ActionRequest, result *ActionResult, row store.MetaCommentAction, api ActionAPI) (*ActionResult, error) {
	mentionAPI, ok := api.(interface {
		ReplyToMention(ctx context.Context, target MentionTarget, message string) (string, error)
	})
	if !ok {
		return a.finish(ctx, t, result, row, errors.New("configured API does not support Instagram mentions"), false)
	}
	id, err := mentionAPI.ReplyToMention(ctx, mentionTarget(t), req.Body)
	return a.finish(ctx, t, result, row, err, true, store.MetaCommentActionOutcome{RemoteID: id})
}

func (a *Actions) archiveMentionReceipt(ctx context.Context, t Target, result *ActionResult, row store.MetaCommentAction) error {
	if result.Receipt.RemoteID == "" || result.Receipt.ArchiveComplete {
		return nil
	}
	tuple, ok := ParseInstagramSourceIdentifier(t.SourceIdentifier)
	if !ok {
		return actionFailure("stale_identity", result, nil)
	}
	sentAt := row.CreatedAt
	if row.DispatchedAt != nil {
		sentAt = *row.DispatchedAt
	}
	id, err := persistMentionReply(ctx, a.st, mentionScope{SourceID: t.SourceID, App: tuple.AppID, Page: tuple.PageID, IG: tuple.AccountID}, mentionTarget(t), result.Receipt.RemoteID, result.Disclosure.Body, sentAt)
	if err == nil {
		result.Receipt.ReplyMessageID, result.Receipt.ArchiveComplete = id, true
	}
	return err
}
