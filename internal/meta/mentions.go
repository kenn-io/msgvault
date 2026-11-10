package meta

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"

	"go.kenn.io/msgvault/internal/store"
)

// MentionsSourceType holds media and comments by other accounts that tag or
// mention the connected Instagram account. It is separate from
// InstagramSourceType because the account does not own this content.
const MentionsSourceType = "instagram_mentions"

const mentionRawFormat = "instagram-mention-json"

// mentionTag marks media in which another account tagged the connected account.
const mentionTag = "tag"

// mentionMediaFields are the fields read on media by other accounts. Meta
// returns owner only for the account's own media.
const mentionMediaFields = "id,caption,timestamp,permalink,username,owner{id}"

type MentionAPI interface {
	VerifyPage(ctx context.Context, pageID string) (Page, error)
	LinkedInstagramAccount(ctx context.Context, pageID, accountID string) (IGUser, error)
	Tags(ctx context.Context, accountID, after string) (IGMediaPage, error)
}

// Tags reads media in which other accounts tagged accountID. Meta returns the
// author's username but no owner ID for media the account did not create.
func (c *Client) Tags(ctx context.Context, accountID, after string) (IGMediaPage, error) {
	if !ValidAccountID(accountID) {
		return IGMediaPage{}, errors.New("invalid selected Instagram account ID")
	}
	page, err := readObjectPage(ctx, c, accountID+"/tags", after, url.Values{
		fieldsParam: {mentionMediaFields}, "limit": {"100"},
	}, func(media IGMedia) string { return media.ID }, true)
	return IGMediaPage(page), err
}

type MentionImporter struct {
	store *store.Store
	api   MentionAPI
}

func NewMentionImporter(st *store.Store, api MentionAPI) *MentionImporter {
	return &MentionImporter{store: st, api: api}
}

// Register verifies the Page and its linked account before any archive write.
func (imp *MentionImporter) Register(ctx context.Context, app, page, account string) (*store.Source, error) {
	if !ValidAccountID(app) || !ValidAccountID(page) || !ValidAccountID(account) {
		return nil, errors.New("invalid Facebook app, Page or Instagram account ID")
	}
	if _, err := imp.api.VerifyPage(ctx, page); err != nil {
		return nil, err
	}
	linked, err := imp.api.LinkedInstagramAccount(ctx, page, account)
	if err != nil {
		return nil, err
	}
	return registerSource(ctx, imp.store, MentionsSourceType, InstagramSourceIdentifier(app, page, account),
		instagramActorIdentifier(app, page, account, "ig_user", account), "instagram_page_token", linked.Username)
}

// ImportTags archives tagged media one page at a time. The checkpoint is the
// next page cursor, so a run that stops early resumes where it stopped.
func (imp *MentionImporter) ImportTags(ctx context.Context, app, page, account string, opts ImportOptions) (*ImportSummary, error) {
	if !ValidAccountID(app) || !ValidAccountID(page) || !ValidAccountID(account) {
		return nil, errors.New("invalid Instagram mentions import options")
	}
	return pollPages(ctx, imp.store, MentionsSourceType, InstagramSourceIdentifier(app, page, account), opts, func(st *store.Store, source *store.Source, sum *ImportSummary, after string) (string, error) {
		tagged, err := imp.api.Tags(ctx, account, after)
		if err != nil {
			return "", err
		}
		run := mentionRun{store: st, sourceID: source.ID, app: app, page: page, account: account, sum: sum}
		for _, media := range tagged.Data {
			if run.own(media) {
				continue
			}
			if err := run.persistMedia(ctx, media, mentionTag); err != nil {
				return "", fmt.Errorf("instagram tagged media %q: %w", media.ID, err)
			}
		}
		return tagged.After, nil
	})
}

type mentionRun struct {
	store              *store.Store
	sourceID           int64
	app, page, account string
	sum                *ImportSummary
}

// mentionMetadata records how an object reached the archive. A later
// delivery of the same object replaces Kind.
type mentionMetadata struct {
	AppID              string `json:"app_id"`
	PageID             string `json:"page_id"`
	InstagramAccountID string `json:"instagram_account_id"`
	MediaID            string `json:"media_id"`
	CommentID          string `json:"comment_id,omitempty"`
	Kind               string `json:"kind"`
}

// persistMedia archives media by another account as the root of its own
// conversation. The author's handle is the subject because Meta returns no
// owner ID that could identify a participant.
func (r *mentionRun) persistMedia(ctx context.Context, media IGMedia, kind string) error {
	title := media.Caption
	if title == "" {
		title = "Instagram media " + media.ID
	}
	subject := ""
	if media.Username != "" {
		subject = "@" + media.Username
	}
	obj := archivedObject{obj: Object{ID: media.ID, Message: media.Caption, CreatedTime: media.Timestamp}, conversation: media.ID, title: title, subject: subject, fromName: subject}
	return r.write(ctx, obj, mentionMetadata{MediaID: media.ID, Kind: kind}, media)
}

// write archives obj in the mentions source with meta and the raw object.
func (r *mentionRun) write(ctx context.Context, obj archivedObject, meta mentionMetadata, raw any) error {
	existing, err := r.store.StoredMessagesContext(ctx, r.sourceID, mentionRawFormat, []string{obj.obj.ID})
	if err != nil {
		return err
	}
	var prior *store.StoredMessage
	var priorMetadata sql.NullString
	if row, ok := existing[obj.obj.ID]; ok {
		prior = &row
		if priorMetadata, err = r.store.GetMessageMetadataContext(ctx, row.ID); err != nil {
			return err
		}
	}
	meta.AppID, meta.PageID, meta.InstagramAccountID = r.app, r.page, r.account
	if obj.metadata, err = json.Marshal(meta); err != nil {
		return err
	}
	if obj.raw, err = json.Marshal(raw); err != nil {
		return err
	}
	obj.sourceID, obj.messageType, obj.format = r.sourceID, MentionsSourceType, mentionRawFormat
	return archiveObject(ctx, r.store, r.sum, obj, prior, priorMetadata)
}
