package peoplebrowser

import (
	"context"

	"go.kenn.io/msgvault/internal/query"
)

// RelationshipLister exposes the ranked relationship view separately from
// Backend so older People test doubles and embedders can keep the original
// profile/activity contract. The daemon-backed implementation joins durable
// Directory cadence into each row.
type RelationshipLister interface {
	ListRelationships(ctx context.Context, request RelationshipListRequest) (*RelationshipListPage, error)
}

type RelationshipListRequest struct {
	Cadence       string
	DueWithinDays *int
	Cursor        string
	Limit         int
	ShowAll       bool
}

type RelationshipListPage struct {
	Rows             []query.RelationshipRow `json:"rows"`
	TotalCount       int64                   `json:"total_count"`
	CacheRevision    string                  `json:"cache_revision"`
	IdentityRevision int64                   `json:"identity_revision"`
	NextCursor       string                  `json:"next_cursor,omitempty"`
}
