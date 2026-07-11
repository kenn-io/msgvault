package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
)

const (
	restorePackA = "01hzy3v7q8r9s0t1a2v3w4x5y6"
	restorePackB = "01hzy3v7q8r9s0t1a2v3w4x5y7"
)

func TestRestorePackCatalogCreatesOnlyPackSchema(t *testing.T) {
	db := openHistoricalRestoreDB(t)

	_, err := NewRestorePackCatalog(context.Background(), db)
	require.NoError(t, err)

	assert.Equal(t, []string{"attachment_pack_index", "attachment_packs", "attachments"},
		listSQLiteObjects(t, db, "table"))
	assert.Equal(t, []string{"idx_attachment_pack_index_pack"},
		listSQLiteObjects(t, db, "index", "sqlite_autoindex_%"))
	assert.Empty(t, listSQLiteObjects(t, db, "trigger"))
}

func TestLooseMetadataClearDoesNotCreatePackSchema(t *testing.T) {
	db := openHistoricalRestoreDB(t)
	st := &Store{db: newLoggedDB(db, nil), dialect: &SQLiteDialect{}}

	require.NoError(t, st.ClearAttachmentPackMetadata())

	assert.Equal(t, []string{"attachments"}, listSQLiteObjects(t, db, "table"))
	assert.Empty(t, listSQLiteObjects(t, db, "index", "sqlite_autoindex_%"))
}

func TestRestorePackCatalogReplacesMetadataAtomically(t *testing.T) {
	db := openHistoricalRestoreDB(t)
	hashA := restoreHash("a1")
	hashB := restoreHash("b2")
	upperA := strings.ToUpper(hashA)
	insertHistoricalAttachment(t, db, 1, 10, upperA, "legacy/a", "", "")
	insertHistoricalAttachment(t, db, 2, 10, hashA, "legacy/a-duplicate", hashB, "legacy/thumb")
	catalog := openRestoreCatalog(t, db)
	insertOldPackMetadata(t, db, restoreHash("ff"))
	createdAt := time.Date(2026, 7, 11, 19, 30, 0, 0, time.UTC)
	record := restorePackRecord(3, 900, createdAt)
	adoptions := []packstore.Adoption{
		{Entry: restoreIndexEntry(hashA, restorePackA, 6, 100, 120, 0, 11), OriginalHashes: []string{upperA, hashA}},
		{Entry: restoreIndexEntry(hashB, restorePackA, 106, 200, 240, 1, 22), OriginalHashes: []string{hashB}},
	}

	require.NoError(t, catalog.ReplaceRestoredPacks(context.Background(), []packstore.PackRecord{record}, adoptions))

	assert.Equal(t, []string{restorePackA}, queryStrings(t, db, `SELECT pack_id FROM attachment_packs ORDER BY pack_id`))
	assert.Equal(t, []string{hashA, hashB}, queryStrings(t, db, `SELECT blob_hash FROM attachment_pack_index ORDER BY blob_hash`))
	var got packstore.IndexEntry
	require.NoError(t, db.QueryRow(`
		SELECT blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c
		FROM attachment_pack_index WHERE blob_hash = ?`, hashB).Scan(
		&got.Hash, &got.PackID, &got.Offset, &got.StoredLen, &got.RawLen, &got.Flags, &got.CRC32C))
	assert.Equal(t, adoptions[1].Entry, got)
	assert.Equal(t, []string{upperA, hashA}, queryStrings(t, db,
		`SELECT content_hash FROM attachments WHERE message_id = 10 ORDER BY id`),
		"restore catalog must not rewrite case-equivalent attachment aliases")
	assert.Equal(t, []string{"legacy/a", "legacy/a-duplicate"}, queryStrings(t, db,
		`SELECT storage_path FROM attachments WHERE message_id = 10 ORDER BY id`))
}

func TestRestorePackCatalogRejectsNonSnapshotHash(t *testing.T) {
	db := openHistoricalRestoreDB(t)
	liveHash := restoreHash("c3")
	outsideHash := restoreHash("d4")
	insertHistoricalAttachment(t, db, 1, 1, liveHash, "live", "", "")
	catalog := openRestoreCatalog(t, db)
	insertOldPackMetadata(t, db, liveHash)
	record := restorePackRecord(1, 100, time.Now().UTC())

	err := catalog.ReplaceRestoredPacks(context.Background(), []packstore.PackRecord{record}, []packstore.Adoption{
		{Entry: restoreIndexEntry(outsideHash, restorePackA, 6, 100, 100, 0, 1)},
	})

	require.ErrorContains(t, err, "not referenced")
	assert.Equal(t, []string{restorePackB}, queryStrings(t, db, `SELECT pack_id FROM attachment_packs`),
		"prevalidation must preserve prior authority")
}

func TestRestorePackCatalogKeepsFullFooterTotals(t *testing.T) {
	db := openHistoricalRestoreDB(t)
	hash := restoreHash("e5")
	insertHistoricalAttachment(t, db, 1, 1, hash, "live", "", "")
	catalog := openRestoreCatalog(t, db)
	record := restorePackRecord(17, 987654, time.Now().UTC())

	require.NoError(t, catalog.ReplaceRestoredPacks(context.Background(), []packstore.PackRecord{record}, []packstore.Adoption{
		{Entry: restoreIndexEntry(hash, restorePackA, 6, 50, 75, 1, 42)},
	}))

	var entries, stored int64
	require.NoError(t, db.QueryRow(`SELECT entry_count, stored_bytes FROM attachment_packs WHERE pack_id = ?`, restorePackA).
		Scan(&entries, &stored))
	assert.Equal(t, record.EntryCount, entries)
	assert.Equal(t, record.StoredBytes, stored)
}

func TestRestorePackCatalogEmptyReplacementClearsMetadata(t *testing.T) {
	db := openHistoricalRestoreDB(t)
	catalog := openRestoreCatalog(t, db)
	insertOldPackMetadata(t, db, restoreHash("9a"))

	require.NoError(t, catalog.ReplaceRestoredPacks(context.Background(), nil, nil))

	assert.Empty(t, queryStrings(t, db, `SELECT pack_id FROM attachment_packs`))
	assert.Empty(t, queryStrings(t, db, `SELECT blob_hash FROM attachment_pack_index`))
}

func TestRestorePackCatalogRollbackPreservesPriorMetadata(t *testing.T) {
	db := openHistoricalRestoreDB(t)
	oldHash := restoreHash("f6")
	newHash := restoreHash("07")
	insertHistoricalAttachment(t, db, 1, 1, newHash, "new", "", "")
	catalog := openRestoreCatalog(t, db)
	insertOldPackMetadata(t, db, oldHash)
	_, err := db.Exec(`CREATE TRIGGER fail_restored_pack_index
		BEFORE INSERT ON attachment_pack_index
		WHEN NEW.blob_hash = '` + newHash + `'
		BEGIN SELECT RAISE(ABORT, 'forced restore index failure'); END`)
	require.NoError(t, err)

	err = catalog.ReplaceRestoredPacks(context.Background(), []packstore.PackRecord{
		restorePackRecord(1, 100, time.Now().UTC()),
	}, []packstore.Adoption{{Entry: restoreIndexEntry(newHash, restorePackA, 6, 100, 100, 0, 1)}})

	require.ErrorContains(t, err, "forced restore index failure")
	assert.Equal(t, []string{restorePackB}, queryStrings(t, db, `SELECT pack_id FROM attachment_packs`))
	assert.Equal(t, []string{oldHash}, queryStrings(t, db, `SELECT blob_hash FROM attachment_pack_index`))
}

func TestRestorePackCatalogUsesRestoreTime(t *testing.T) {
	db := openHistoricalRestoreDB(t)
	hash := restoreHash("18")
	insertHistoricalAttachment(t, db, 1, 1, "", "", strings.ToUpper(hash), "thumb")
	catalog := openRestoreCatalog(t, db)
	restoreTime := time.Date(2026, 7, 11, 20, 15, 42, 0, time.FixedZone("offset", -5*60*60))

	require.NoError(t, catalog.ReplaceRestoredPacks(context.Background(), []packstore.PackRecord{
		restorePackRecord(1, 100, restoreTime),
	}, []packstore.Adoption{{Entry: restoreIndexEntry(hash, restorePackA, 6, 100, 100, 0, 1)}}))

	var created string
	require.NoError(t, db.QueryRow(`SELECT created_at FROM attachment_packs WHERE pack_id = ?`, restorePackA).Scan(&created))
	assert.Equal(t, restoreTime.UTC().Format(time.RFC3339), created)
}

func openHistoricalRestoreDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "historical.db")+"?_foreign_keys=ON")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.Exec(`CREATE TABLE attachments (
		id INTEGER PRIMARY KEY,
		message_id INTEGER NOT NULL,
		content_hash TEXT,
		storage_path TEXT,
		thumbnail_hash TEXT,
		thumbnail_path TEXT
	)`)
	require.NoError(t, err)
	return db
}

func openRestoreCatalog(t *testing.T, db *sql.DB) packstore.RestoreCatalog {
	t.Helper()
	catalog, err := NewRestorePackCatalog(context.Background(), db)
	require.NoError(t, err)
	return catalog
}

func listSQLiteObjects(t *testing.T, db *sql.DB, kind string, excludedLike ...string) []string {
	t.Helper()
	query := `SELECT name FROM sqlite_master WHERE type = ?`
	args := []any{kind}
	var querySb190 strings.Builder
	for _, pattern := range excludedLike {
		querySb190.WriteString(` AND name NOT LIKE ?`)
		args = append(args, pattern)
	}
	query += querySb190.String()
	query += ` ORDER BY name`
	return queryStrings(t, db, query, args...)
}

func queryStrings(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(query, args...)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var values []string
	for rows.Next() {
		var value string
		require.NoError(t, rows.Scan(&value))
		values = append(values, value)
	}
	require.NoError(t, rows.Err())
	return values
}

func insertHistoricalAttachment(t *testing.T, db *sql.DB, id, messageID int64, hash, storage, thumbHash, thumbPath string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO attachments
		(id, message_id, content_hash, storage_path, thumbnail_hash, thumbnail_path)
		VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''))`,
		id, messageID, hash, storage, thumbHash, thumbPath)
	require.NoError(t, err)
}

func insertOldPackMetadata(t *testing.T, db *sql.DB, hash string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO attachment_packs (pack_id, entry_count, stored_bytes, created_at)
		VALUES (?, 1, 100, '2026-01-01T00:00:00Z')`, restorePackB)
	require.NoError(t, err)
	entry := restoreIndexEntry(hash, restorePackB, 6, 100, 100, 0, 1)
	_, err = db.Exec(`INSERT INTO attachment_pack_index
		(blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, entry.Hash, entry.PackID, entry.Offset,
		entry.StoredLen, entry.RawLen, entry.Flags, entry.CRC32C)
	require.NoError(t, err)
}

func restorePackRecord(entries, stored int64, created time.Time) packstore.PackRecord {
	return packstore.PackRecord{PackID: restorePackA, EntryCount: entries, StoredBytes: stored, CreatedAt: created}
}

func restoreIndexEntry(hash, packID string, offset, stored, raw int64, flags uint8, crc uint32) packstore.IndexEntry {
	return packstore.IndexEntry{Hash: packstore.Hash(hash), PackID: packID, Offset: offset,
		StoredLen: stored, RawLen: raw, Flags: flags, CRC32C: crc}
}

func restoreHash(prefix string) string {
	return prefix + strings.Repeat("0", 64-len(prefix))
}
