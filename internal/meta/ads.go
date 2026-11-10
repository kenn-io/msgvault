package meta

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
)

// adCreativesEdge visits the Instagram media used by an ad account's
// creatives. The account's media list can omit media made only for ads.
const adCreativesEdge PostEdge = "adcreatives"

const instagramMediaFields = "id,caption,timestamp,permalink,owner{id}"

var instagramAdEdges = [...]PostEdge{instagramMediaEdge, adCreativesEdge}

type AdCreative struct {
	ID                        string `json:"id"`
	EffectiveInstagramMediaID string `json:"effective_instagram_media_id,omitempty"`
}

type AdCreativePage struct {
	Data  []AdCreative
	After string
}

// AdCreatives reads one page of an ad account's creatives. account is the
// decimal ad account ID without the "act_" prefix.
func (c *Client) AdCreatives(ctx context.Context, account, after string) (AdCreativePage, error) {
	if !ValidAccountID(account) {
		return AdCreativePage{}, errors.New("invalid ad account ID")
	}
	page, err := readObjectPage(ctx, c, "act_"+account+"/adcreatives", after, url.Values{
		"fields": {"id,effective_instagram_media_id"}, "limit": {"100"},
	}, func(creative AdCreative) string { return creative.ID })
	if err != nil {
		return AdCreativePage{}, err
	}
	for _, creative := range page.Data {
		if id := creative.EffectiveInstagramMediaID; id != "" && !safeObjectID(id) {
			return AdCreativePage{}, errors.New("invalid Instagram media ID in ad creative")
		}
	}
	return AdCreativePage(page), nil
}

func (c *Client) InstagramMediaByID(ctx context.Context, id string) (IGMedia, error) {
	if !safeObjectID(id) {
		return IGMedia{}, errors.New("invalid Instagram media ID")
	}
	var media IGMedia
	if err := c.get(ctx, url.PathEscape(id), url.Values{"fields": {instagramMediaFields}}, &media); err != nil {
		return IGMedia{}, err
	}
	if media.ID != id {
		return IGMedia{}, errors.New("instagram media identity mismatch")
	}
	return media, nil
}

// ImportInstagramAds imports like ImportInstagram, then also visits the
// Instagram media used by adAccount's creatives. ads reads the creatives;
// media and comments are still read with the Page token.
func (imp *Importer) ImportInstagramAds(ctx context.Context, app, page, account, adAccount string, ads *Client, opts ImportOptions) (*ImportSummary, error) {
	if !ValidAccountID(adAccount) || ads == nil {
		return nil, errors.New("invalid ad account or ad client")
	}
	selected, err := imp.forInstagram(account)
	if err != nil {
		return nil, err
	}
	selected.adAccount, selected.ads = adAccount, ads
	return selected.Import(ctx, app, page, opts)
}

// readAdCreatives returns one unresolved post per distinct media ID on the
// page. adMedia reads each one before it is archived.
func (r *importRun) readAdCreatives(ctx context.Context, after string) (ObjectPage, error) {
	page, err := r.imp.ads.AdCreatives(ctx, r.imp.adAccount, after)
	if err != nil {
		return ObjectPage{}, err
	}
	result := ObjectPage{After: page.After}
	for _, creative := range page.Data {
		id := creative.EffectiveInstagramMediaID
		if id == "" || slices.ContainsFunc(result.Data, func(o Object) bool { return o.ID == id }) {
			continue
		}
		result.Data = append(result.Data, Object{ID: id})
	}
	return result, nil
}

// adMedia reads the media an ad uses with the Page token and reports whether
// the configured account owns it. Ads can promote other accounts' media; those
// comments do not belong to this source, so the media is skipped.
func (r *importRun) adMedia(ctx context.Context, id string) (Object, bool, error) {
	var media IGMedia
	_, err := r.request(ctx, adCreativesEdge, "", func() (ObjectPage, error) {
		var err error
		media, err = r.imp.igAPI.InstagramMediaByID(ctx, id)
		return ObjectPage{}, err
	})
	// Retrying cannot make media readable once Graph says it is missing or the
	// Page token lacks access, and failing would stop every later creative.
	// Graph also reports rate limits as HTTP 400, so other errors still fail.
	if apiErr, ok := errors.AsType[*APIError](err); ok && mediaUnreadable(apiErr) {
		slog.WarnContext(ctx, "skipping Instagram ad media the Page token cannot read", "media_id", id, "status", apiErr.Status, "code", apiErr.Code)
		return Object{}, false, nil
	}
	if err != nil {
		return Object{}, false, err
	}
	if media.Owner == nil || media.Owner.ID != r.imp.instagram {
		slog.WarnContext(ctx, "skipping Instagram ad media the configured account does not own", "media_id", id)
		return Object{}, false, nil
	}
	return Object{ID: media.ID, Message: media.Caption, CreatedTime: media.Timestamp, InstagramMedia: &media}, true, nil
}

// mediaUnreadable reports Graph's "object missing" (100) and permission
// (10, 200-299) codes, or a bare 403/404 with no code.
func mediaUnreadable(e *APIError) bool {
	switch {
	case e.Code == 100 || e.Code == 10 || (e.Code >= 200 && e.Code <= 299):
		return true
	case e.Code == 0:
		return e.Status == http.StatusForbidden || e.Status == http.StatusNotFound
	default:
		return false
	}
}
