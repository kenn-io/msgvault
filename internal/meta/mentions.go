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
		"fields": {"id,caption,timestamp,permalink,username,owner{id}"}, "limit": {"100"},
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
func (imp *MentionImporter) ImportTags(ctx context.Context, app, page, account string, opts ImportOptions) (sum *ImportSummary, retErr error) {
	if !ValidAccountID(app) || !ValidAccountID(page) || !ValidAccountID(account) || opts.MaxRequests < 0 {
		return nil, errors.New("invalid Instagram mentions import options")
	}
	source, execution, err := acquireSource(ctx, imp.store, MentionsSourceType, InstagramSourceIdentifier(app, page, account))
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	after := ""
	if !opts.Full {
		if after, err = savedCheckpoint(ctx, imp.store, source.ID); err != nil {
			return nil, err
		}
	}
	syncID, err := execution.StartSyncContext(ctx, MentionsSourceType, "")
	if err != nil {
		return nil, err
	}
	run := mentionRun{store: imp.store.ScopedToSync(source.ID, syncID), sourceID: source.ID, app: app, page: page, account: account, sum: &ImportSummary{}}
	sum = run.sum
	done := false
	checkpoint := func() *store.Checkpoint {
		cp := &store.Checkpoint{PageToken: after, MessagesAdded: int64(sum.MessagesAdded), MessagesUpdated: int64(sum.MessagesUpdated), MessagesProcessed: int64(sum.MessagesProcessed)}
		if done {
			cp.PageToken = ""
		}
		return cp
	}
	defer func() {
		sum.Partial = retErr != nil || !done
		retErr = finishSync(ctx, imp.store, source.ID, syncID, checkpoint(), sum.Partial, retErr)
	}()
	maximum := opts.MaxRequests
	if maximum == 0 {
		maximum = DefaultMaxRequests
	}
	for !done && sum.Requests < maximum {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		sum.Requests++
		tagged, err := imp.api.Tags(ctx, account, after)
		var apiErr *APIError
		if after != "" && errors.As(err, &apiErr) && apiErr.Status == 400 {
			return sum, fmt.Errorf("instagram tags continuation request failed; rerun with --full: %w", err)
		} else if err != nil {
			return sum, err
		}
		for _, media := range tagged.Data {
			// The account's own media belongs to its comments source.
			if media.Owner != nil && media.Owner.ID == account {
				continue
			}
			if err := run.persistMedia(ctx, media, mentionTag); err != nil {
				return sum, fmt.Errorf("instagram tagged media %q: %w", media.ID, err)
			}
		}
		after, done = tagged.After, tagged.After == ""
		if err := run.store.UpdateSyncCheckpointContext(ctx, syncID, checkpoint()); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

type mentionRun struct {
	store              *store.Store
	sourceID           int64
	app, page, account string
	sum                *ImportSummary
}

// mentionMetadata records how the media reached the archive. A later
// delivery of the same object replaces Kind.
type mentionMetadata struct {
	AppID              string `json:"app_id"`
	PageID             string `json:"page_id"`
	InstagramAccountID string `json:"instagram_account_id"`
	MediaID            string `json:"media_id"`
	Kind               string `json:"kind"`
}

// persistMedia archives media by another account as the root of its own
// conversation. The author's handle is the subject because Meta returns no
// owner ID that could identify a participant.
func (r *mentionRun) persistMedia(ctx context.Context, media IGMedia, kind string) error {
	existing, err := r.store.StoredMessagesContext(ctx, r.sourceID, mentionRawFormat, []string{media.ID})
	if err != nil {
		return err
	}
	var prior *store.StoredMessage
	var priorMetadata sql.NullString
	if row, ok := existing[media.ID]; ok {
		prior = &row
		if priorMetadata, err = r.store.GetMessageMetadataContext(ctx, row.ID); err != nil {
			return err
		}
	}
	metadata, err := json.Marshal(mentionMetadata{AppID: r.app, PageID: r.page, InstagramAccountID: r.account, MediaID: media.ID, Kind: kind})
	if err != nil {
		return err
	}
	raw, err := json.Marshal(media)
	if err != nil {
		return err
	}
	title := media.Caption
	if title == "" {
		title = "Instagram media " + media.ID
	}
	subject := ""
	if media.Username != "" {
		subject = "@" + media.Username
	}
	return archiveObject(ctx, r.store, r.sum, archivedObject{
		sourceID: r.sourceID, messageType: MentionsSourceType, format: mentionRawFormat,
		obj:          Object{ID: media.ID, Message: media.Caption, CreatedTime: media.Timestamp},
		conversation: media.ID, title: title, subject: subject, fromName: subject,
		metadata: metadata, raw: raw,
	}, prior, priorMetadata)
}
