package meta

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"go.kenn.io/msgvault/internal/store"
)

// CommentAction is a change msgvault can make to an archived comment on Meta.
type CommentAction string

const (
	ActionHide   CommentAction = "hide"
	ActionUnhide CommentAction = "unhide"
	ActionDelete CommentAction = "delete"
	ActionReply  CommentAction = "reply"
)

// ProfileAction is the profile `actions` entry that allows a. Unhide only
// undoes hide, so one opt-in covers both.
func (a CommentAction) ProfileAction() string {
	if a == ActionUnhide {
		return string(ActionHide)
	}
	return string(a)
}

// commentPlatform holds the only differences between Facebook and Instagram
// comment writes.
type commentPlatform struct {
	name, hideParam, replyEdge string
}

var commentPlatforms = map[string]commentPlatform{
	SourceType:          {name: "Facebook", hideParam: "is_hidden", replyEdge: "comments"},
	InstagramSourceType: {name: "Instagram", hideParam: "hide", replyEdge: "replies"},
}

// CommentTarget is an archived comment, read from the archive alone.
type CommentTarget struct {
	MessageID, SourceID          int64
	SourceType, SourceIdentifier string
	// CommentID is Meta's native comment ID.
	CommentID, Author, Text string
	Hidden                  bool
	metadata                objectMetadata
}

// Provider names the platform for messages to the user.
func (t CommentTarget) Provider() string { return commentPlatforms[t.SourceType].name }

// ResolveCommentTarget finds an archived Facebook or Instagram comment by its
// archive message ID. Posts, media captions, comments deleted from Meta and
// messages from other sources are refused.
func ResolveCommentTarget(ctx context.Context, st *store.Store, messageID int64) (CommentTarget, error) {
	found, err := st.GetMessagesSummariesByIDsContext(ctx, []int64{messageID})
	if err != nil {
		return CommentTarget{}, err
	}
	if len(found) == 0 {
		return CommentTarget{}, fmt.Errorf("message %d: %w", messageID, store.ErrMessageNotFound)
	}
	msg := found[0]
	source, err := st.GetSourceByIDContext(ctx, msg.SourceID)
	if err != nil {
		return CommentTarget{}, err
	}
	if _, ok := commentPlatforms[source.SourceType]; !ok {
		return CommentTarget{}, fmt.Errorf("message %d is not a Facebook or Instagram comment", messageID)
	}
	saved, err := st.GetMessageMetadataContext(ctx, messageID)
	if err != nil {
		return CommentTarget{}, err
	}
	var metadata objectMetadata
	if !saved.Valid || json.Unmarshal([]byte(saved.String), &metadata) != nil {
		return CommentTarget{}, fmt.Errorf("message %d has no readable comment metadata", messageID)
	}
	if metadata.ParentID == "" {
		return CommentTarget{}, fmt.Errorf("message %d is a post or media caption, not a comment", messageID)
	}
	return CommentTarget{
		MessageID: messageID, SourceID: source.ID, SourceType: source.SourceType, SourceIdentifier: source.Identifier,
		CommentID: msg.SourceMessageID, Author: msg.FromName, Text: msg.Snippet, Hidden: metadata.Hidden, metadata: metadata,
	}, nil
}

// ModerateComment applies action on Meta, then records it in the archive. It
// holds the source's sync lock so a running sync cannot overwrite the result.
func ModerateComment(ctx context.Context, st *store.Store, client *Client, target CommentTarget, action CommentAction) (retErr error) {
	platform, ok := commentPlatforms[target.SourceType]
	if !ok || !safeObjectID(target.CommentID) {
		return errors.New("invalid comment target")
	}
	execution, _, err := lockActiveSource(ctx, st, target.SourceID)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	var result struct {
		Success bool `json:"success"`
	}
	path := url.PathEscape(target.CommentID)
	switch action {
	case ActionHide, ActionUnhide:
		err = client.post(ctx, path, url.Values{platform.hideParam: {strconv.FormatBool(action == ActionHide)}}, &result)
	case ActionDelete:
		err = client.delete(ctx, path, &result)
	default:
		return fmt.Errorf("unknown comment action %q", action)
	}
	if err != nil {
		return err
	}
	if !result.Success {
		return errors.New("meta did not confirm the comment change")
	}
	// Meta has applied the change. Record it even if the caller has gone away,
	// so the archive does not disagree with Meta.
	local := context.WithoutCancel(ctx)
	if action == ActionDelete {
		return st.MarkMessageDeletedContext(local, target.SourceID, target.CommentID)
	}
	return setCommentHidden(local, st, target.MessageID, action == ActionHide)
}

// lockActiveSource takes the source's sync lock so a running sync cannot
// overwrite the archived result, and refuses a retired source.
func lockActiveSource(ctx context.Context, st *store.Store, sourceID int64) (*store.SyncExecution, *store.Source, error) {
	execution, err := st.AcquireSyncExecutionContext(ctx, sourceID)
	if err != nil {
		return nil, nil, err
	}
	source, err := st.GetSourceByIDContext(ctx, sourceID)
	if err == nil && (source.HistoryOnly || source.MergedIntoSourceID != 0) {
		err = store.ErrSourceRetired
	}
	if err != nil {
		return nil, nil, errors.Join(err, execution.Release())
	}
	return execution, source, nil
}

func setCommentHidden(ctx context.Context, st *store.Store, messageID int64, hidden bool) error {
	saved, err := st.GetMessageMetadataContext(ctx, messageID)
	if err != nil {
		return err
	}
	var metadata objectMetadata
	if err := json.Unmarshal([]byte(saved.String), &metadata); err != nil {
		return fmt.Errorf("read comment metadata: %w", err)
	}
	metadata.Hidden = hidden
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	return st.SetMessageMetadataContext(ctx, messageID, sql.NullString{String: string(data), Valid: true})
}
