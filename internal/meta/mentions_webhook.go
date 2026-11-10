package meta

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"

	"go.kenn.io/msgvault/internal/store"
)

const (
	mentionCaption = "caption_mention"
	mentionComment = "comment_mention"
)

// MentionReader reads the media and comments that mention an account.
type MentionReader interface {
	MentionedMedia(ctx context.Context, account, mediaID string) (IGMedia, error)
	MentionedComment(ctx context.Context, account, commentID string) (MentionedComment, error)
}

// MentionedComment is a comment that mentions the account, with its media.
type MentionedComment struct {
	ID        string   `json:"id"`
	Text      string   `json:"text"`
	Timestamp string   `json:"timestamp"`
	Media     *IGMedia `json:"media,omitempty"`
}

// MentionedMedia reads media whose caption mentions account. Meta allows
// this read only through the mentioned account's node.
func (c *Client) MentionedMedia(ctx context.Context, account, mediaID string) (IGMedia, error) {
	if !ValidAccountID(account) || !ValidAccountID(mediaID) {
		return IGMedia{}, errors.New("invalid Instagram mention identity")
	}
	var wire struct {
		Media IGMedia `json:"mentioned_media"`
	}
	if err := c.get(ctx, account, url.Values{fieldsParam: {"mentioned_media.media_id(" + mediaID + "){" + mentionMediaFields + "}"}}, &wire); err != nil {
		return IGMedia{}, err
	}
	if wire.Media.ID != mediaID {
		return IGMedia{}, errors.New("instagram mention identity mismatch")
	}
	return wire.Media, nil
}

// MentionedComment reads a comment that mentions account, with its media.
func (c *Client) MentionedComment(ctx context.Context, account, commentID string) (MentionedComment, error) {
	if !ValidAccountID(account) || !ValidAccountID(commentID) {
		return MentionedComment{}, errors.New("invalid Instagram mention identity")
	}
	var wire struct {
		Comment MentionedComment `json:"mentioned_comment"`
	}
	if err := c.get(ctx, account, url.Values{fieldsParam: {"mentioned_comment.comment_id(" + commentID + "){id,text,timestamp,media{" + mentionMediaFields + "}}"}}, &wire); err != nil {
		return MentionedComment{}, err
	}
	if wire.Comment.ID != commentID || wire.Comment.Media == nil || !ValidAccountID(wire.Comment.Media.ID) {
		return MentionedComment{}, errors.New("instagram mention identity mismatch")
	}
	return wire.Comment, nil
}

// ApplyMentions archives the media and comments that Instagram mentions
// webhook changes name into the mentions source. A caption mention names only
// media; a comment mention names the comment and its media. Each is read
// through the account's node, which Meta allows only for content that
// mentions the account. Content on the account's own media is skipped,
// because it belongs to the comments source.
func ApplyMentions(ctx context.Context, st *store.Store, reader MentionReader, app, page, account string, values []jsontext.Value) error {
	type change struct {
		MediaID   string `json:"media_id"`
		CommentID string `json:"comment_id"`
	}
	changes := make([]change, 0, len(values))
	for _, value := range values {
		var c change
		if err := json.Unmarshal(value, &c); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidChange, err)
		}
		if !ValidAccountID(c.MediaID) || (c.CommentID != "" && !ValidAccountID(c.CommentID)) {
			return fmt.Errorf("%w: invalid mention identity", ErrInvalidChange)
		}
		changes = append(changes, c)
	}
	return applyWebhook(ctx, st, MentionsSourceType, InstagramSourceIdentifier(app, page, account), func(st *store.Store, source *store.Source, sum *ImportSummary) error {
		run := mentionRun{store: st, sourceID: source.ID, app: app, page: page, account: account, sum: sum}
		for _, c := range changes {
			if c.CommentID == "" {
				media, err := reader.MentionedMedia(ctx, account, c.MediaID)
				if err != nil {
					return fmt.Errorf("read mentioned media %q: %w", c.MediaID, err)
				}
				if run.own(media) {
					continue
				}
				if err := run.persistMedia(ctx, media, mentionCaption); err != nil {
					return err
				}
				continue
			}
			comment, err := reader.MentionedComment(ctx, account, c.CommentID)
			if err != nil {
				return fmt.Errorf("read mentioned comment %q: %w", c.CommentID, err)
			}
			if comment.Media.ID != c.MediaID {
				return fmt.Errorf("%w: comment %q is not on media %q", ErrInvalidChange, c.CommentID, c.MediaID)
			}
			if run.own(*comment.Media) {
				continue
			}
			if err := run.persistComment(ctx, comment); err != nil {
				return err
			}
		}
		return nil
	})
}

// own reports whether media belongs to the connected account. Meta returns
// owner only for the account's own media.
func (r *mentionRun) own(media IGMedia) bool {
	return media.Owner != nil && media.Owner.ID == r.account
}

// persistComment archives a comment that mentions the account as a reply to
// its media, archiving the media first when it is not archived yet.
func (r *mentionRun) persistComment(ctx context.Context, comment MentionedComment) error {
	media := *comment.Media
	existing, err := r.store.StoredMessagesContext(ctx, r.sourceID, mentionRawFormat, []string{media.ID})
	if err != nil {
		return err
	}
	if _, ok := existing[media.ID]; !ok {
		if err := r.persistMedia(ctx, media, mentionComment); err != nil {
			return err
		}
		if existing, err = r.store.StoredMessagesContext(ctx, r.sourceID, mentionRawFormat, []string{media.ID}); err != nil {
			return err
		}
	}
	comment.Media = nil
	obj := archivedObject{
		obj:          Object{ID: comment.ID, Message: comment.Text, CreatedTime: comment.Timestamp},
		conversation: media.ID, reply: sql.NullInt64{Int64: existing[media.ID].ID, Valid: true},
	}
	return r.write(ctx, obj, mentionMetadata{MediaID: media.ID, CommentID: comment.ID, Kind: mentionComment}, comment)
}
