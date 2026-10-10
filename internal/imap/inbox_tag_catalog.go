package imap

import (
	"context"
	"slices"
	"strings"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

var _ inboxcontrol.TagCatalogProvider = (*InboxProvider)(nil)

// TagCatalog observes existing persistent INBOX keywords. Wildcard support
// permits changing an existing keyword, never provisioning a GTD tag.
func (p *InboxProvider) TagCatalog(ctx context.Context, source inboxcontrol.SourceIdentity) ([]emailtags.Tag, error) {
	if p.client == nil || p.client.config == nil || source.Validate() != nil || source != p.source || source.SourceType != "imap" || source.SourceIdentifier != p.client.config.Identifier() || source.AccountID != p.client.config.Username {
		return nil, inboxcontrol.ErrDenied
	}
	tags := []emailtags.Tag{}
	err := p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		selected, err := conn.Select("INBOX", &imapapi.SelectOptions{ReadOnly: true}).Wait()
		if err != nil || selected.UIDValidity == 0 {
			return inboxcontrol.ErrUnavailable
		}
		permanent := make([]string, len(selected.PermanentFlags))
		for i, flag := range selected.PermanentFlags {
			permanent[i] = string(flag)
		}
		seen := []string{}
		add := func(keyword string) error {
			if strings.HasPrefix(keyword, "\\") {
				return nil
			}
			if validateKeyword(keyword) != nil {
				return inboxcontrol.ErrUnavailable
			}
			if _, err := emailtags.Normalize(emailtags.Change{Add: []string{keyword}}, true); err != nil {
				return inboxcontrol.ErrUnavailable
			}
			if !emailtags.Contains(seen, keyword, true) {
				seen = append(seen, keyword)
				tags = append(tags, emailtags.Tag{ID: keyword, Name: keyword})
			}
			return nil
		}
		for _, keyword := range permanent {
			if err := add(keyword); err != nil {
				return err
			}
		}
		for _, flag := range selected.Flags {
			keyword := string(flag)
			if selected.PermanentFlags == nil || emailtags.Contains(permanent, string(imapapi.FlagWildcard), true) || emailtags.Contains(permanent, keyword, true) {
				if err := add(keyword); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(tags, func(a, b emailtags.Tag) int { return strings.Compare(a.ID, b.ID) })
	return tags, nil
}
