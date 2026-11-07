package backupapp

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
)

func TestPackedRestoreTargetUsesDefaultAndCustomLimits(t *testing.T) {
	assert.Equal(t, packstore.DefaultLimits(), NewPackedRestoreTarget(packstore.Limits{}).Limits())

	custom := packstore.DefaultLimits()
	custom.BlobBytes = 12345
	custom.PackEntries = 77
	assert.Equal(t, custom, NewPackedRestoreTarget(custom).Limits())
}

func TestPackedRestoreTargetAcquiresMutationLease(t *testing.T) {
	require := require.New(t)
	target := NewPackedRestoreTarget(packstore.DefaultLimits())

	lease, err := target.AcquireRestoreLease(t.Context())
	require.NoError(err)
	require.NoError(lease.ValidateMutation())
	require.NoError(lease.Release())
	assert.ErrorIs(t, lease.ValidateMutation(), packstore.ErrLeaseReleased)
}

func TestPackedRestoreTargetWrapsLeaseAcquisitionFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	lease, err := NewPackedRestoreTarget(packstore.DefaultLimits()).AcquireRestoreLease(ctx)

	assert.Nil(t, lease)
	require.ErrorIs(t, err, context.Canceled)
	assert.ErrorContains(t, err, "acquire packed restore mutation lease")
}

func TestPackedRestoreTargetOpensOnlyStagedCatalog(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	staged := openPackedRestoreTestDB(t, "staged.db")
	other := openPackedRestoreTestDB(t, "other.db")
	_, err := other.ExecContext(t.Context(), `CREATE TABLE live_marker (id INTEGER PRIMARY KEY)`)
	require.NoError(err)
	target := NewPackedRestoreTarget(packstore.DefaultLimits())

	catalog, err := target.OpenRestoreCatalog(t.Context(), staged)
	require.NoError(err)
	require.NoError(catalog.ReplaceRestoredPacks(t.Context(), nil, nil))

	assert.Equal(2, countPackedRestoreTables(t, staged))
	assert.Zero(countPackedRestoreTables(t, other), "target must not open or migrate any other database")
	var markers int
	require.NoError(other.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='live_marker'`).Scan(&markers))
	assert.Equal(1, markers)
}

func openPackedRestoreTestDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), name))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `CREATE TABLE attachments (
		id INTEGER PRIMARY KEY,
		content_hash TEXT,
		thumbnail_hash TEXT
	)`)
	require.NoError(t, err)
	return db
}

func countPackedRestoreTables(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name IN ('attachment_pack_index', 'attachment_packs')`).Scan(&count))
	return count
}
