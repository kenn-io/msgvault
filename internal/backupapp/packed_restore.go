package backupapp

import (
	"context"
	"database/sql"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/msgvault/internal/store"
)

// PackedRestoreTarget supplies msgvault's production pack policy and opens
// authority only against the unpublished SQLite database Kit passes to it.
type PackedRestoreTarget struct {
	limits packstore.Limits
}

// NewPackedRestoreTarget constructs the narrow backup restore adapter. A zero
// Limits value selects the production defaults; nonzero values support bounded
// compatibility tests and future application policy changes.
func NewPackedRestoreTarget(limits packstore.Limits) *PackedRestoreTarget {
	if limits == (packstore.Limits{}) {
		limits = packstore.DefaultLimits()
	}
	return &PackedRestoreTarget{limits: limits}
}

// Limits implements backup.PackedContentTarget.
func (t *PackedRestoreTarget) Limits() packstore.Limits {
	if t == nil || t.limits == (packstore.Limits{}) {
		return packstore.DefaultLimits()
	}
	return t.limits
}

// OpenRestoreCatalog implements backup.PackedContentTarget.
func (t *PackedRestoreTarget) OpenRestoreCatalog(
	ctx context.Context,
	db *sql.DB,
) (packstore.RestoreCatalog, error) {
	return store.NewRestorePackCatalog(ctx, db)
}
