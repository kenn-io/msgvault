package peoplebrowser

import (
	"context"

	"go.kenn.io/msgvault/internal/store"
)

// DirectoryLister exposes the Store-owned durable people Directory page.
// Implementations must preserve the query and page semantics of the owner.
type DirectoryLister interface {
	ListDirectoryPeople(ctx context.Context, query store.DirectoryPeopleQuery) (*store.DirectoryPeoplePage, error)
}
