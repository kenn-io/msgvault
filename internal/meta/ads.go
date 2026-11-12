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

// fieldsParam is the Graph query parameter that selects response fields.
const fieldsParam = "fields"

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
		fieldsParam: {"id,effective_instagram_media_id"}, "limit": {"100"},
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
	if err := c.get(ctx, url.PathEscape(id), url.Values{fieldsParam: {instagramMediaFields}}, &media); err != nil {
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
	sum, err := selected.Import(ctx, app, page, opts)
	// Scheduled syncs discard the summary, so the daemon log is where the
	// skipped count reaches the user.
	if sum != nil && sum.AdMediaSkipped > 0 {
		slog.InfoContext(ctx, "Instagram ad media skipped", "source", InstagramSourceIdentifier(app, page, account), "count", sum.AdMediaSkipped)
	}
	return sum, err
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
	// Retrying cannot make media readable once Graph says it does not exist
	// or the Page token lacks access, and failing would stop every later
	// creative. Graph also reports rate limits and malformed requests as
	// HTTP 400, so every other error still fails the run.
	if apiErr, ok := errors.AsType[*APIError](err); ok && mediaUnreadable(apiErr) {
		slog.WarnContext(ctx, "skipping Instagram ad media the Page token cannot read", "media_id", id, "status", apiErr.Status, "code", apiErr.Code, "subcode", apiErr.Subcode)
		r.sum.AdMediaSkipped++
		return Object{}, false, nil
	}
	if err != nil {
		return Object{}, false, err
	}
	if media.Owner == nil || media.Owner.ID == "" {
		slog.WarnContext(ctx, "skipping Instagram ad media because Graph returned no owner for it", "media_id", id)
		r.sum.AdMediaSkipped++
		return Object{}, false, nil
	}
	if media.Owner.ID != r.imp.instagram {
		// Expected on an ad account shared with other Instagram accounts.
		slog.DebugContext(ctx, "skipping Instagram ad media owned by another account", "media_id", id)
		r.sum.AdMediaSkipped++
		return Object{}, false, nil
	}
	return Object{ID: media.ID, Message: media.Caption, CreatedTime: media.Timestamp, InstagramMedia: &media}, true, nil
}

// mediaUnreadable reports errors that mean the media cannot be read by this
// token: code 100 with subcode 33, which Meta documents as an object that does
// not exist, cannot be loaded due to missing permissions, or does not support
// the operation; the permission codes 10 and 200-299; or a bare 403/404 with
// no code. Code 100 alone is Meta's general invalid-parameter error, so any
// other subcode means a rejected request and is not skipped.
func mediaUnreadable(e *APIError) bool {
	switch {
	case e.Code == 100:
		return e.Subcode == 33
	case e.Code == 10 || (e.Code >= 200 && e.Code <= 299):
		return true
	case e.Code == 0:
		return e.Status == http.StatusForbidden || e.Status == http.StatusNotFound
	default:
		return false
	}
}
