package query

import "context"

// CollectionScope is the read-only projection used by clients that can
// present named source collections. SourceIDs is non-nil and empty for an
// explicitly empty collection, which means match nothing.
type CollectionScope struct {
	Name      string
	SourceIDs []int64
}

// CollectionScopeLister is an optional query capability. It projects the
// existing store collection catalog without widening the query.Engine
// contract.
type CollectionScopeLister interface {
	ListCollectionScopes(ctx context.Context) ([]CollectionScope, error)
}
