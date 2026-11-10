package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	"go.kenn.io/msgvault/internal/store"
)

var (
	// ErrReplySent means this exact reply was already posted to the comment.
	ErrReplySent = errors.New("this reply was already posted to the comment")
	// ErrReplyUnknown means an earlier send of this exact reply may have been posted.
	ErrReplyUnknown = errors.New("an earlier send of this reply has an unknown outcome")
)

// ReplyToComment posts body as a public reply to target and archives the
// reply. The send is recorded before Meta is called, so when Meta's answer is
// lost an identical reply is refused until ClearUnknownReply.
func ReplyToComment(ctx context.Context, st *store.Store, client *Client, target CommentTarget, body string) (replyID string, retErr error) {
	platform, ok := commentPlatforms[target.SourceType]
	if !ok || !safeObjectID(target.CommentID) || body == "" {
		return "", errors.New("invalid comment reply")
	}
	execution, source, err := lockActiveSource(ctx, st, target.SourceID)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	prior, err := st.LatestMetaCommentReplyContext(ctx, target.SourceID, target.CommentID, body)
	switch {
	case err == nil && prior.State == store.MetaCommentReplySent:
		return "", ErrReplySent
	case err == nil:
		return "", ErrReplyUnknown
	case !errors.Is(err, sql.ErrNoRows):
		return "", err
	}
	record, err := st.StartMetaCommentReplyContext(ctx, target.SourceID, target.CommentID, body)
	if err != nil {
		return "", err
	}
	var posted struct {
		ID string `json:"id"`
	}
	err = client.post(ctx, url.PathEscape(target.CommentID)+"/"+platform.replyEdge, url.Values{"message": {body}}, &posted)
	// Record what Meta did even if the caller has gone away.
	local := context.WithoutCancel(ctx)
	if apiErr, ok := errors.AsType[*APIError](err); ok && apiErr.Status < 500 {
		// Meta refused the reply, so sending it again cannot duplicate it.
		return "", errors.Join(err, st.FinishMetaCommentReplyContext(local, record, store.MetaCommentReplyFailed, ""))
	}
	if err == nil && !safeObjectID(posted.ID) {
		err = errors.New("meta did not return the reply's ID")
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrReplyUnknown, err)
	}
	if err := st.FinishMetaCommentReplyContext(local, record, store.MetaCommentReplySent, posted.ID); err != nil {
		return posted.ID, err
	}
	// Archive the reply as sync will see it. Meta decides its parent: a
	// Facebook reply to a reply joins the top-level comment's thread.
	reply, err := client.readComment(local, target.SourceType, posted.ID)
	if err == nil && reply.ID != posted.ID {
		err = errors.New("meta returned a different comment")
	}
	if err == nil {
		err = archiveReply(local, st, source, target, reply)
	}
	if err != nil {
		return posted.ID, fmt.Errorf("reply %s was posted but not archived; the next sync archives it: %w", posted.ID, err)
	}
	return posted.ID, nil
}

// ClearUnknownReply lets an identical reply be sent again after the owner
// has checked on Meta that the earlier send did not post.
func ClearUnknownReply(ctx context.Context, st *store.Store, target CommentTarget, body string) (retErr error) {
	execution, _, err := lockActiveSource(ctx, st, target.SourceID)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	prior, err := st.LatestMetaCommentReplyContext(ctx, target.SourceID, target.CommentID, body)
	if errors.Is(err, sql.ErrNoRows) || err == nil && prior.State != store.MetaCommentReplySending {
		return errors.New("no reply with this text has an unknown outcome")
	}
	if err != nil {
		return err
	}
	return st.FinishMetaCommentReplyContext(ctx, prior.ID, store.MetaCommentReplyFailed, "")
}

// readComment reads one comment with the fields sync reads.
func (c *Client) readComment(ctx context.Context, sourceType, id string) (Object, error) {
	if !safeObjectID(id) {
		return Object{}, errors.New("invalid Graph object ID")
	}
	if sourceType == InstagramSourceType {
		var comment IGComment
		if err := c.get(ctx, url.PathEscape(id), url.Values{"fields": {instagramCommentFields}}, &comment); err != nil {
			return Object{}, err
		}
		return instagramCommentObject(&comment), nil
	}
	var comment Object
	err := c.get(ctx, url.PathEscape(id), url.Values{"fields": {facebookCommentFields}}, &comment)
	return comment, err
}

func archiveReply(ctx context.Context, st *store.Store, source *store.Source, target CommentTarget, reply Object) error {
	m := target.metadata
	root, edge := m.PostID, Posts
	switch {
	case m.InstagramAccountID != "":
		root, edge = m.MediaID, instagramMediaEdge
	case m.Visitor:
		edge = VisitorPosts
	case m.Ad && !m.Organic:
		edge = AdsPosts
	}
	run := &importRun{imp: &Importer{store: st, instagram: m.InstagramAccountID}, app: m.AppID, page: m.PageID, source: source, sum: &ImportSummary{}}
	return run.persist(ctx, reply, root, target.CommentID, edge)
}
