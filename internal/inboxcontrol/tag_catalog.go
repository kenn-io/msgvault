package inboxcontrol

import (
	"context"

	"go.kenn.io/msgvault/internal/emailtags"
)

// TagCatalogProvider supplies existing native tag identities for owner mapping
// validation. Lookup is read-only and excludes protected location/read markers.
// A provider without a native catalog cannot validate or provision a mapping.
type TagCatalogProvider interface {
	TagCatalog(ctx context.Context, source SourceIdentity) ([]emailtags.Tag, error)
}
