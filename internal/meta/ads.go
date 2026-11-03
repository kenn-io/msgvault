package meta

import (
	"context"
	"errors"
	"fmt"
	"net/url"
)

const adCreativesEdge PostEdge = "adcreatives"
const instagramMediaFields = "id,caption,timestamp,permalink,owner{id}"

var instagramAdEdges = [...]PostEdge{instagramMediaEdge, adCreativesEdge}

type AdCreative struct {
	ID                        string `json:"id"`
	AccountID                 string `json:"account_id"`
	EffectiveInstagramMediaID string `json:"effective_instagram_media_id"`
}

type AdCreativePage struct {
	Data  []AdCreative
	After string
}

func (c *Client) AdCreatives(ctx context.Context, account, after string) (AdCreativePage, error) {
	if !ValidAccountID(account) {
		return AdCreativePage{}, errors.New("invalid selected ad account ID")
	}
	page, err := readObjectPage(ctx, c, "act_"+account+"/adcreatives", after, url.Values{
		"fields": {"id,account_id,effective_instagram_media_id"}, "limit": {"100"},
	}, func(creative AdCreative) string { return creative.ID })
	if err != nil {
		return AdCreativePage{}, err
	}
	for _, creative := range page.Data {
		if id := creative.EffectiveInstagramMediaID; id != "" && !safeObjectID(id) {
			return AdCreativePage{}, errors.New("invalid effective Instagram media ID")
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

// ImportInstagramAds adds only an explicitly selected ad-account read boundary.
// Canonical media and comments still use the verified Page-context client.
func (imp *Importer) ImportInstagramAds(ctx context.Context, app, page, account, adAccount string, ad *Client, opts ImportOptions) (*ImportSummary, error) {
	if !ValidAccountID(adAccount) || ad == nil {
		return nil, errors.New("invalid selected ad account or client")
	}
	selected, err := imp.forInstagram(account)
	if err != nil {
		return nil, err
	}
	selected.adAccount, selected.adClient = adAccount, ad
	return selected.Import(ctx, app, page, opts)
}

func (r *importRun) readAdCreatives(ctx context.Context, after string) (ObjectPage, error) {
	page, err := r.imp.adClient.AdCreatives(ctx, r.imp.adAccount, after)
	if err != nil {
		return ObjectPage{}, err
	}
	result := ObjectPage{After: page.After}
	for i := range page.Data {
		creative := &page.Data[i]
		result.Data = append(result.Data, Object{ID: creative.ID, AdCreative: creative})
	}
	return result, nil
}

func (r *importRun) resolveAdMedia(ctx context.Context, candidate Object) (Object, bool, error) {
	creative := candidate.AdCreative
	if creative.AccountID != r.imp.adAccount || creative.EffectiveInstagramMediaID == "" {
		return Object{}, false, nil
	}
	page, err := r.request(ctx, adCreativesEdge, "", func() (ObjectPage, error) {
		media, err := r.imp.igAPI.InstagramMediaByID(ctx, creative.EffectiveInstagramMediaID)
		return ObjectPage{Data: []Object{{ID: media.ID, Message: media.Caption, CreatedTime: media.Timestamp, InstagramMedia: &media, AdCreative: creative}}}, err
	})
	if err != nil {
		return Object{}, false, fmt.Errorf("media %q for creative %q: %w", creative.EffectiveInstagramMediaID, creative.ID, err)
	}
	media := page.Data[0].InstagramMedia
	if media.Owner == nil || media.Owner.ID != r.imp.instagram {
		return Object{}, false, nil
	}
	return page.Data[0], true, nil
}
