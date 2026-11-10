package store

import (
	"context"
	"errors"
	"fmt"
)

// Reply states. A reply stays "sending" when msgvault cannot tell whether
// Meta posted it, so an identical reply is refused until it is cleared.
const (
	MetaCommentReplySending = "sending"
	MetaCommentReplySent    = "sent"
	MetaCommentReplyFailed  = "failed"
)

// MetaCommentReply is a public reply msgvault sent, or tried to send, to an
// archived Facebook or Instagram comment.
type MetaCommentReply struct {
	ID    int64
	State string
}

// LatestMetaCommentReplyContext returns the newest reply with this body to
// the comment that did not fail. It wraps sql.ErrNoRows when there is none.
func (s *Store) LatestMetaCommentReplyContext(ctx context.Context, sourceID int64, commentID, body string) (MetaCommentReply, error) {
	var reply MetaCommentReply
	err := s.db.QueryRowContext(ctx, `
		SELECT id, state FROM meta_comment_replies
		WHERE source_id = ? AND comment_id = ? AND body = ? AND state <> 'failed'
		ORDER BY id DESC LIMIT 1`, sourceID, commentID, body).Scan(&reply.ID, &reply.State)
	if err != nil {
		return MetaCommentReply{}, fmt.Errorf("read comment reply: %w", err)
	}
	return reply, nil
}

// StartMetaCommentReplyContext records a reply before it is sent.
func (s *Store) StartMetaCommentReplyContext(ctx context.Context, sourceID int64, commentID, body string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO meta_comment_replies (source_id, comment_id, body, state)
		VALUES (?, ?, ?, 'sending') RETURNING id`, sourceID, commentID, body).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("record comment reply: %w", err)
	}
	return id, nil
}

// FinishMetaCommentReplyContext records the outcome of a reply that is still
// sending. replyID is Meta's ID for a sent reply.
func (s *Store) FinishMetaCommentReplyContext(ctx context.Context, id int64, state, replyID string) error {
	if state != MetaCommentReplySent && state != MetaCommentReplyFailed {
		return errors.New("invalid comment reply state")
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE meta_comment_replies SET state = ?, reply_id = NULLIF(?, ''), updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND state = 'sending'`, state, replyID, id)
	if err != nil {
		return fmt.Errorf("finish comment reply: %w", err)
	}
	return nil
}
