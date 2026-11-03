package meta

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

const instagramCommentFields = "id,text,timestamp,from{id,username,self_ig_scoped_id},parent_id"
const InstagramSourceType = "instagram_comments"
const instagramMediaEdge PostEdge = "media"

var instagramEdges = [...]PostEdge{instagramMediaEdge}

type instagramAPI interface {
	LinkedInstagramAccount(ctx context.Context, pageID, accountID string) (IGUser, error)
	InstagramMedia(ctx context.Context, accountID, after string) (IGMediaPage, error)
	InstagramComments(ctx context.Context, mediaID, after string) (IGCommentPage, error)
	InstagramReplies(ctx context.Context, commentID, after string) (IGCommentPage, error)
}

func InstagramSourceIdentifier(app, page, account string) string {
	return SourceIdentifier(app, page) + ":instagram:" + account
}

type InstagramSourceID struct {
	AppID, PageID, AccountID string
}

func ParseInstagramSourceIdentifier(identifier string) (InstagramSourceID, bool) {
	parts := strings.Split(identifier, ":")
	if len(parts) != 6 || parts[4] != "instagram" || !ValidAccountID(parts[5]) {
		return InstagramSourceID{}, false
	}
	app, page, ok := ParseSourceIdentifier(strings.Join(parts[:4], ":"))
	if !ok {
		return InstagramSourceID{}, false
	}
	return InstagramSourceID{AppID: app, PageID: page, AccountID: parts[5]}, true
}

func (imp *Importer) forInstagram(account string) (*Importer, error) {
	api, ok := imp.api.(instagramAPI)
	if !ok || !ValidAccountID(account) {
		return nil, errors.New("instagram client or configured account unavailable")
	}
	selected := *imp
	selected.instagram, selected.igAPI = account, api
	return &selected, nil
}

func (imp *Importer) RegisterInstagram(ctx context.Context, app, page, account string) (*store.Source, error) {
	selected, err := imp.forInstagram(account)
	if err != nil {
		return nil, err
	}
	return selected.Register(ctx, app, page)
}

func (imp *Importer) ImportInstagram(ctx context.Context, app, page, account string, opts ImportOptions) (*ImportSummary, error) {
	selected, err := imp.forInstagram(account)
	if err != nil {
		return nil, err
	}
	return selected.Import(ctx, app, page, opts)
}

func (imp *Importer) sourceType() string {
	if imp.instagram != "" {
		return InstagramSourceType
	}
	return SourceType
}

func (imp *Importer) sourceIdentifier(app, page string) string {
	if imp.instagram != "" {
		return InstagramSourceIdentifier(app, page, imp.instagram)
	}
	return SourceIdentifier(app, page)
}

func (r *importRun) providerName() string {
	if r.imp.instagram != "" {
		return "instagram"
	}
	return "facebook"
}

func (r *importRun) readPosts(ctx context.Context, edge PostEdge, after string) (ObjectPage, error) {
	if r.imp.instagram == "" {
		return r.imp.api.Posts(ctx, r.page, edge, after)
	}
	page, err := r.imp.igAPI.InstagramMedia(ctx, r.imp.instagram, after)
	if err != nil {
		return ObjectPage{}, err
	}
	result := ObjectPage{After: page.After}
	for i := range page.Data {
		media := &page.Data[i]
		result.Data = append(result.Data, Object{ID: media.ID, Message: media.Caption, CreatedTime: media.Timestamp, InstagramMedia: media})
	}
	return result, nil
}

func (r *importRun) readComments(ctx context.Context, frame commentFrame) (ObjectPage, error) {
	if r.imp.instagram == "" {
		return r.imp.api.Comments(ctx, frame.Parent, frame.After)
	}
	var page IGCommentPage
	var err error
	if frame.Parent == frame.Root {
		page, err = r.imp.igAPI.InstagramComments(ctx, frame.Parent, frame.After)
	} else {
		page, err = r.imp.igAPI.InstagramReplies(ctx, frame.Parent, frame.After)
	}
	if err != nil {
		return ObjectPage{}, err
	}
	result := ObjectPage{After: page.After}
	for i := range page.Data {
		comment := &page.Data[i]
		object := Object{ID: comment.ID, Message: comment.Text, CreatedTime: comment.Timestamp, InstagramComment: comment}
		if comment.ParentID != "" {
			object.Parent = &Actor{ID: comment.ParentID}
		}
		result.Data = append(result.Data, object)
	}
	return result, nil
}

// LinkedInstagramAccount performs one read after separate Page verification.
// Keeping the reads separate lets the importer charge both to its budget.
func (c *Client) LinkedInstagramAccount(ctx context.Context, pageID, accountID string) (IGUser, error) {
	if !ValidAccountID(pageID) || !ValidAccountID(accountID) {
		return IGUser{}, errors.New("invalid selected Page or Instagram account ID")
	}
	var wire struct {
		ID      string  `json:"id"`
		Account *IGUser `json:"instagram_business_account"`
	}
	if err := c.get(ctx, pageID, url.Values{"fields": {"id,instagram_business_account{id,username}"}}, &wire); err != nil {
		return IGUser{}, err
	}
	if wire.ID != pageID || wire.Account == nil || wire.Account.ID != accountID {
		return IGUser{}, errors.New("instagram linked account unavailable or identity mismatch")
	}
	return *wire.Account, nil
}

func (c *Client) InstagramMedia(ctx context.Context, accountID, after string) (IGMediaPage, error) {
	if !ValidAccountID(accountID) {
		return IGMediaPage{}, errors.New("invalid selected Instagram account ID")
	}
	page, err := readObjectPage(ctx, c, accountID+"/media", after, url.Values{
		"fields": {"id,caption,timestamp,permalink,owner{id}"}, "limit": {"100"},
	}, func(media IGMedia) string { return media.ID })
	return IGMediaPage(page), err
}

func (c *Client) InstagramComments(ctx context.Context, mediaID, after string) (IGCommentPage, error) {
	if !safeObjectID(mediaID) {
		return IGCommentPage{}, errors.New("invalid Graph object ID")
	}
	page, err := readObjectPage(ctx, c, url.PathEscape(mediaID)+"/comments", after, url.Values{
		"fields": {instagramCommentFields}, "limit": {"100"},
	}, func(comment IGComment) string { return comment.ID })
	return IGCommentPage(page), err
}

func (c *Client) InstagramReplies(ctx context.Context, commentID, after string) (IGCommentPage, error) {
	if !safeObjectID(commentID) {
		return IGCommentPage{}, errors.New("invalid Graph object ID")
	}
	page, err := readObjectPage(ctx, c, url.PathEscape(commentID)+"/replies", after, url.Values{
		"fields": {instagramCommentFields}, "limit": {"100"},
	}, func(comment IGComment) string { return comment.ID })
	return IGCommentPage(page), err
}
