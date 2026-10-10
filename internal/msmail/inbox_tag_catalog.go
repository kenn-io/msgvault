package msmail

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/msgraph"
)

var _ inboxcontrol.TagCatalogProvider = (*InboxProvider)(nil)

// TagCatalog reads existing master categories with the current token. Graph
// requires MailboxSettings.Read; this lookup never requests additional scopes.
// Category assignment uses displayName, not the master-list object's ID:
// https://learn.microsoft.com/en-us/graph/api/resources/outlookcategory
func (p *InboxProvider) TagCatalog(ctx context.Context, source inboxcontrol.SourceIdentity) ([]emailtags.Tag, error) {
	if p.client == nil || source.Validate() != nil || source != p.source || source.SourceType != SourceType {
		return nil, inboxcontrol.ErrDenied
	}
	if err := p.account(ctx); err != nil {
		return nil, err
	}
	base, err := url.Parse(p.client.BaseURL())
	if err != nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	const path = "/me/outlook/masterCategories"
	next := path + "?$select=id,displayName"
	visited := map[string]bool{}
	names := map[string]bool{}
	tags := []emailtags.Tag{}
	for pages := 0; next != ""; pages++ {
		if pages == 16 || visited[next] {
			return nil, inboxcontrol.ErrUnavailable
		}
		visited[next] = true
		u, err := url.Parse(next)
		if err != nil || u.User != nil || u.Fragment != "" || u.Host != "" && !u.IsAbs() {
			return nil, inboxcontrol.ErrUnavailable
		}
		if u.IsAbs() {
			if !strings.EqualFold(u.Scheme, base.Scheme) || !strings.EqualFold(u.Host, base.Host) || u.Path != base.Path+path {
				return nil, inboxcontrol.ErrUnavailable
			}
		} else if u.Path != path {
			return nil, inboxcontrol.ErrUnavailable
		}
		var page struct {
			Value *[]struct {
				Name string `json:"displayName"`
			} `json:"value"`
			Next string `json:"@odata.nextLink"`
		}
		if err := p.client.GetJSONOnce(ctx, next, &page, 1<<20); err != nil {
			if errors.Is(err, msgraph.ErrForbidden) {
				return nil, inboxcontrol.ErrDenied
			}
			return nil, inboxcontrol.ErrUnavailable
		}
		if page.Value == nil || len(tags)+len(*page.Value) > 1000 {
			return nil, inboxcontrol.ErrUnavailable
		}
		for _, category := range *page.Value {
			if _, err := emailtags.Normalize(emailtags.Change{Add: []string{category.Name}}, false); err != nil || names[category.Name] {
				return nil, inboxcontrol.ErrUnavailable
			}
			names[category.Name] = true
			tags = append(tags, emailtags.Tag{ID: category.Name, Name: category.Name})
		}
		next = page.Next
	}
	slices.SortFunc(tags, func(a, b emailtags.Tag) int { return strings.Compare(a.ID, b.ID) })
	return tags, nil
}
