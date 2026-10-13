package gmail

import (
	"context"

	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

var _ inboxcontrol.TagCatalogProvider = (*InboxProvider)(nil)

// TagCatalog uses Gmail's native user-label identities. Folders validates the
// bound account and catalog evidence; system labels never become GTD tags.
func (p *InboxProvider) TagCatalog(ctx context.Context, source inboxcontrol.SourceIdentity) ([]emailtags.MessageTag, error) {
	labels, err := p.Folders(ctx, source)
	if err != nil {
		return nil, err
	}
	tags := make([]emailtags.MessageTag, len(labels))
	for i, label := range labels {
		tags[i] = emailtags.MessageTag{ID: label.ID, Name: label.Name}
	}
	return tags, nil
}
