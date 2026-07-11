package backupapp

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/msgvault/internal/store"
)

// PackedRestoreTarget supplies msgvault's production pack policy and opens
// authority only against the unpublished SQLite database Kit passes to it.
type PackedRestoreTarget struct {
	limits      packstore.Limits
	coordinator *packstore.Coordinator
}

// NewPackedRestoreTarget constructs the narrow backup restore adapter. A zero
// Limits value selects the production defaults; nonzero values support bounded
// compatibility tests and future application policy changes.
func NewPackedRestoreTarget(limits packstore.Limits) *PackedRestoreTarget {
	if limits == (packstore.Limits{}) {
		limits = packstore.DefaultLimits()
	}
	return &PackedRestoreTarget{
		limits:      limits,
		coordinator: packstore.NewCoordinator(),
	}
}

// Limits implements backup.PackedContentTarget.
func (t *PackedRestoreTarget) Limits() packstore.Limits {
	if t == nil || t.limits == (packstore.Limits{}) {
		return packstore.DefaultLimits()
	}
	return t.limits
}

// AcquireRestoreLease implements backup.PackedContentTarget. Backup restore is
// a standalone local command, so it has no co-resident maintainer; this
// target-owned coordinator still gives Kit the required mutation-lease
// lifecycle and keeps the adapter ready for a future shared in-process owner.
func (t *PackedRestoreTarget) AcquireRestoreLease(ctx context.Context) (*packstore.Lease, error) {
	lease, err := t.coordinator.AcquireMutation(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire packed restore mutation lease: %w", err)
	}
	return lease, nil
}

// OpenRestoreCatalog implements backup.PackedContentTarget.
func (t *PackedRestoreTarget) OpenRestoreCatalog(
	ctx context.Context,
	db *sql.DB,
) (packstore.RestoreCatalog, error) {
	return store.NewRestorePackCatalog(ctx, db)
}
