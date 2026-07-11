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

func TestPackedRestoreTargetOpensOnlyStagedCatalog(t *testing.T) {
	staged := openPackedRestoreTestDB(t, "staged.db")
	other := openPackedRestoreTestDB(t, "other.db")
	_, err := other.Exec(`CREATE TABLE live_marker (id INTEGER PRIMARY KEY)`)
	require.NoError(t, err)
	target := NewPackedRestoreTarget(packstore.DefaultLimits())

	catalog, err := target.OpenRestoreCatalog(context.Background(), staged)
	require.NoError(t, err)
	require.NoError(t, catalog.ReplaceRestoredPacks(context.Background(), nil, nil))

	assert.Equal(t, 2, countPackedRestoreTables(t, staged))
	assert.Zero(t, countPackedRestoreTables(t, other), "target must not open or migrate any other database")
	var markers int
	require.NoError(t, other.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='live_marker'`).Scan(&markers))
	assert.Equal(t, 1, markers)
}

func openPackedRestoreTestDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), name))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.Exec(`CREATE TABLE attachments (
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
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name IN ('attachment_pack_index', 'attachment_packs')`).Scan(&count))
	return count
}
