package meta

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

const mentionMediaFields = "id,caption,media_type,timestamp,username,owner{id}"

const mentionFieldsParameter = "fields"

var ErrMentionsRefused = errors.New("instagram mentions request refused: the cause may be missing instagram_basic, instagram_manage_comments or pages_read_engagement (plus ads_read for Business Manager Page roles), a Page token where a User token is needed, deleted or private media, disabled comments, an object that is not a mention, or rate limiting")

type IGMentionMedia struct {
	ID        string  `json:"id"`
	Caption   string  `json:"caption"`
	MediaType string  `json:"media_type"`
	Timestamp string  `json:"timestamp"`
	Username  string  `json:"username"`
	Owner     *IGUser `json:"owner,omitempty"`
}

type IGMentionComment struct {
	ID        string          `json:"id"`
	Text      string          `json:"text"`
	Timestamp string          `json:"timestamp"`
	Media     *IGMentionMedia `json:"media"`
}

type MediaPage struct {
	Data  []IGMentionMedia
	After string
}

func mentionReadError(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) && (apiErr.Status == 400 || apiErr.Status == 403) {
		return fmt.Errorf("%w: %w", ErrMentionsRefused, err)
	}
	return err
}

func (m IGMentionMedia) valid() bool {
	return ValidAccountID(m.ID) && (m.Owner == nil || ValidAccountID(m.Owner.ID))
}

func (c *Client) MentionedMedia(ctx context.Context, ig, mediaID string) (IGMentionMedia, error) {
	if !ValidAccountID(ig) || !ValidAccountID(mediaID) {
		return IGMentionMedia{}, errors.New("invalid Instagram mention identity")
	}
	var wire struct {
		ID    string         `json:"id"`
		Media IGMentionMedia `json:"mentioned_media"`
	}
	err := c.get(ctx, ig, url.Values{mentionFieldsParameter: {"mentioned_media.media_id(" + mediaID + "){" + mentionMediaFields + "}"}}, &wire)
	if err != nil {
		return IGMentionMedia{}, mentionReadError(err)
	}
	if (wire.ID != "" && wire.ID != ig) || wire.Media.ID != mediaID || !wire.Media.valid() {
		return IGMentionMedia{}, errors.New("instagram mention identity mismatch")
	}
	return wire.Media, nil
}

func (c *Client) MentionedComment(ctx context.Context, ig, commentID string) (IGMentionComment, error) {
	if !ValidAccountID(ig) || !ValidAccountID(commentID) {
		return IGMentionComment{}, errors.New("invalid Instagram mention identity")
	}
	var wire struct {
		ID      string           `json:"id"`
		Comment IGMentionComment `json:"mentioned_comment"`
	}
	err := c.get(ctx, ig, url.Values{mentionFieldsParameter: {"mentioned_comment.comment_id(" + commentID + "){id,text,timestamp,media{" + mentionMediaFields + "}}"}}, &wire)
	if err != nil {
		return IGMentionComment{}, mentionReadError(err)
	}
	if (wire.ID != "" && wire.ID != ig) || wire.Comment.ID != commentID || wire.Comment.Media == nil || !wire.Comment.Media.valid() {
		return IGMentionComment{}, errors.New("instagram mention identity mismatch")
	}
	return wire.Comment, nil
}

func (c *Client) Tags(ctx context.Context, ig, after string) (MediaPage, error) {
	if !ValidAccountID(ig) {
		return MediaPage{}, errors.New("invalid Instagram account identity")
	}
	page, err := readObjectPage(ctx, c, ig+"/tags", after, url.Values{mentionFieldsParameter: {mentionMediaFields}, "limit": {"100"}}, func(m IGMentionMedia) string { return m.ID }, true)
	if err != nil {
		return MediaPage{}, mentionReadError(err)
	}
	for _, m := range page.Data {
		if !m.valid() {
			return MediaPage{}, errors.New("invalid Instagram tagged media identity")
		}
	}
	return MediaPage(page), nil
}

const MentionsSourceType = "instagram_mentions"
const mentionRawFormat = "instagram-mention-json"

type mentionScope struct {
	SourceID      int64
	App, Page, IG string
	Summary       *ImportSummary
}

type mentionSurface uint8

const (
	surfaceTagged mentionSurface = iota + 1
	surfaceCaptionMention
	surfaceCommentMention
)

type mentionMetadata struct {
	Version        int    `json:"version"`
	AppID          string `json:"app_id"`
	PageID         string `json:"page_id"`
	IGUserID       string `json:"ig_user_id"`
	MediaID        string `json:"media_id"`
	CommentID      string `json:"comment_id,omitempty"`
	ParentKnown    bool   `json:"parent_known"`
	Tagged         bool   `json:"tagged"`
	CaptionMention bool   `json:"caption_mention"`
	CommentMention bool   `json:"comment_mention"`
	Reply          bool   `json:"reply"`
	OwnerUsername  string `json:"owner_username,omitempty"`
	MediaType      string `json:"media_type,omitempty"`
}

func (m mentionMetadata) encode(ctx context.Context, st *store.Store, rowID int64, object, parent string) ([]byte, error) {
	if object == m.MediaID && parent == "" {
		m.CommentID, m.CommentMention = "", false
	} else if object != m.CommentID || parent != m.MediaID {
		return nil, errors.New("instagram mention parent mismatch")
	}
	if rowID != 0 {
		saved, err := st.GetMessageMetadataContext(ctx, rowID)
		if err != nil {
			return nil, err
		}
		var old mentionMetadata
		if !saved.Valid || json.Unmarshal([]byte(saved.String), &old) != nil || old.Version != 1 {
			return nil, errors.New("unsupported Instagram mention metadata version")
		}
		if old.AppID != m.AppID || old.PageID != m.PageID || old.IGUserID != m.IGUserID || old.MediaID != m.MediaID || old.CommentID != m.CommentID {
			return nil, errors.New("instagram mention provenance mismatch")
		}
		m.Tagged = m.Tagged || old.Tagged
		m.CaptionMention = m.CaptionMention || old.CaptionMention
		m.CommentMention = m.CommentMention || old.CommentMention
	}
	return json.Marshal(m)
}

func persistMentionMedia(ctx context.Context, st *store.Store, s mentionScope, m IGMentionMedia, surface mentionSurface) (int64, error) {
	return persistMention(ctx, st, s, m, nil, surface)
}

func persistMentionComment(ctx context.Context, st *store.Store, s mentionScope, c IGMentionComment) (int64, error) {
	if c.Media == nil || !ValidAccountID(c.ID) {
		return 0, errors.New("invalid Instagram mention comment")
	}
	// Validate replay provenance before updating even the media root.
	rows, err := st.StoredMessagesContext(ctx, s.SourceID, mentionRawFormat, []string{c.ID, c.Media.ID})
	if err != nil {
		return 0, err
	}
	meta := mentionMetadata{Version: 1, AppID: s.App, PageID: s.Page, IGUserID: s.IG, MediaID: c.Media.ID, CommentID: c.ID}
	if _, err = meta.encode(ctx, st, rows[c.ID].ID, c.ID, c.Media.ID); err != nil {
		return 0, err
	}
	// A comment expansion may omit media fields. Create a missing root, but
	// leave a fuller tag/caption projection intact on later comment deliveries.
	if rows[c.Media.ID].ID == 0 {
		if _, err = persistMentionMedia(ctx, st, s, *c.Media, 0); err != nil {
			return 0, err
		}
	}
	return persistMention(ctx, st, s, *c.Media, &c, surfaceCommentMention)
}

func persistMention(ctx context.Context, st *store.Store, s mentionScope, m IGMentionMedia, c *IGMentionComment, surface mentionSurface) (int64, error) {
	if !m.valid() {
		return 0, errors.New("invalid Instagram mention media")
	}
	meta := mentionMetadata{Version: 1, AppID: s.App, PageID: s.Page, IGUserID: s.IG, MediaID: m.ID,
		Tagged: surface == surfaceTagged, CaptionMention: surface == surfaceCaptionMention, CommentMention: surface == surfaceCommentMention,
		OwnerUsername: m.Username, MediaType: m.MediaType}
	obj, parent, title := Object{ID: m.ID, Message: m.Caption, CreatedTime: m.Timestamp}, "", "Instagram media "+m.ID
	if m.Caption != "" {
		title = "@" + m.Username + ": " + strings.SplitN(m.Caption, "\n", 2)[0]
		runes := []rune(title)
		title = string(runes[:min(120, len(runes))])
	}
	opt := &persistOptions{SourceType: MentionsSourceType, RawFormat: mentionRawFormat, Raw: m, Title: title, FromAddr: m.Username}
	// Keep the unlinked handle in a canonical field used by FTS reconstruction.
	if m.Username != "" {
		opt.Subject = "@" + m.Username
	}
	if c != nil {
		obj, parent = Object{ID: c.ID, Message: c.Text, CreatedTime: c.Timestamp}, m.ID
		meta.CommentID = c.ID
		opt.Raw, opt.Title, opt.FromAddr = c, "", ""
		opt.Subject = ""
	}
	opt.Metadata = func(ctx context.Context, row int64, id, parent string) ([]byte, error) {
		return meta.encode(ctx, st, row, id, parent)
	}
	sum := s.Summary
	if sum == nil {
		sum = &ImportSummary{}
	}
	r := &importRun{imp: &Importer{store: st}, source: &store.Source{ID: s.SourceID}, app: s.App, page: s.Page, sum: sum}
	err := r.persist(ctx, obj, m.ID, parent, "", opt)
	return opt.MessageID, err
}
