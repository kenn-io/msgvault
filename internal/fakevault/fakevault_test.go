package fakevault_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/fakevault"
)

func generate(t *testing.T, dir string, messages, attachmentBytes int64,
	seed uint64, appendMode bool) *fakevault.Summary {
	t.Helper()
	sum, err := fakevault.Generate(context.Background(), fakevault.Options{
		Dir:             dir,
		Messages:        messages,
		AttachmentBytes: attachmentBytes,
		Seed:            seed,
		Append:          appendMode,
	})
	require.NoError(t, err)
	return sum
}

func openVaultDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "msgvault.db")+"?mode=ro")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

// verifyAttachmentTree proves the property backup depends on: every
// content-bearing attachments row references a file that exists at the
// canonical <hash[:2]>/<hash> path and whose bytes hash to content_hash.
// A mismatch would make backup.Create fail on the generated vault.
func verifyAttachmentTree(t *testing.T, dir string) (hashes []string) {
	t.Helper()
	db := openVaultDB(t, dir)
	rows, err := db.Query(`SELECT content_hash, storage_path, size FROM attachments
		UNION SELECT thumbnail_hash, thumbnail_path, NULL FROM attachments
		WHERE thumbnail_hash IS NOT NULL`)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
	}()
	for rows.Next() {
		var hash, storagePath string
		var size sql.NullInt64
		require.NoError(t, rows.Scan(&hash, &storagePath, &size))
		require.Equal(t, hash[:2]+"/"+hash, storagePath)
		content, err := os.ReadFile(filepath.Join(dir, "attachments",
			filepath.FromSlash(storagePath)))
		require.NoError(t, err, "attachment %s must exist on disk", hash)
		sum := sha256.Sum256(content)
		require.Equal(t, hash, hex.EncodeToString(sum[:]),
			"attachment bytes must match their recorded content hash")
		if size.Valid {
			require.Equal(t, size.Int64, int64(len(content)))
		}
		hashes = append(hashes, hash)
	}
	slices.Sort(hashes)
	return hashes
}

func countRows(t *testing.T, db *sql.DB, table string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&n))
	return n
}

// TestGenerateProducesBackupValidVault is the contract test: a generated
// vault must survive the full backup engine round trip — create, then
// restore with its stats-reproduction and per-page hash proof — because
// benchmarking backup on a vault the engine rejects would measure nothing.
func TestGenerateProducesBackupValidVault(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	dir := t.TempDir()
	sum := generate(t, dir, 250, 1<<21, 42, false)
	assert.Equal(int64(250), sum.Messages)
	assert.Positive(sum.AttachmentRows)
	assert.Positive(sum.AttachmentBlobs)
	verifyAttachmentTree(t, dir)

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	app := backupapp.New("fakevault-test")
	opts := backup.CreateOptions{
		DBPath:     filepath.Join(dir, "msgvault.db"),
		ContentDir: filepath.Join(dir, "attachments"),
		DataDir:    dir,
	}
	m, err := backup.Create(ctx, repo, app, opts)
	require.NoError(err)

	// Extend the vault and snapshot again, so the incremental path
	// (page deltas, parent chains, attachment dedup) is what a harness
	// exercising --append will actually hit.
	_ = generate(t, dir, 50, 1<<20, 42, true)
	m2, err := backup.Create(ctx, repo, app, opts)
	require.NoError(err)
	assert.Equal(m.SnapshotID, m2.ParentID)

	res, err := backup.Restore(ctx, repo, app, backup.RestoreOptions{
		TargetDir: filepath.Join(t.TempDir(), "restored"),
	})
	require.NoError(err)
	assert.Equal(m2.SnapshotID, res.SnapshotID)

	db := openVaultDB(t, dir)
	assert.Equal(int64(300), countRows(t, db, "messages"))
}

// TestGenerateIsDeterministic pins the seed contract: two runs with the
// same parameters must produce the same row counts and the same attachment
// blob set, so benchmark results are comparable across machines and runs.
func TestGenerateIsDeterministic(t *testing.T) {
	assert := assert.New(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	sumA := generate(t, dirA, 200, 1<<21, 7, false)
	sumB := generate(t, dirB, 200, 1<<21, 7, false)
	assert.Equal(sumA, sumB)
	assert.Equal(verifyAttachmentTree(t, dirA), verifyAttachmentTree(t, dirB))

	dbA, dbB := openVaultDB(t, dirA), openVaultDB(t, dirB)
	for _, table := range []string{"conversations", "attachments",
		"message_recipients", "message_labels", "message_raw"} {
		assert.Equal(countRows(t, dbA, table), countRows(t, dbB, table), table)
	}

	// A different seed must actually change the content.
	dirC := t.TempDir()
	generate(t, dirC, 200, 1<<21, 8, false)
	assert.NotEqual(verifyAttachmentTree(t, dirA), verifyAttachmentTree(t, dirC),
		"different seeds must produce different attachment content")
}

// TestAppendExtendsVault pins the append contract: message indexes continue,
// dimension seeding is idempotent, and the attachment tree stays consistent.
func TestAppendExtendsVault(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	generate(t, dir, 150, 1<<20, 3, false)
	sum := generate(t, dir, 100, 1<<20, 3, true)
	assert.Equal(int64(250), sum.Messages)
	verifyAttachmentTree(t, dir)

	db := openVaultDB(t, dir)
	var distinct int64
	require.NoError(db.QueryRow(
		"SELECT COUNT(DISTINCT source_message_id) FROM messages").Scan(&distinct))
	assert.Equal(int64(250), distinct, "append must continue message indexes, not repeat them")
	assert.Equal(int64(3), countRows(t, db, "sources"), "dimension seeding must be idempotent")

	var orphans int64
	require.NoError(db.QueryRow(`SELECT COUNT(*) FROM messages m
		LEFT JOIN conversations c ON c.id = m.conversation_id
		WHERE c.id IS NULL`).Scan(&orphans))
	assert.Zero(orphans, "every message must reference an existing conversation")
}

func TestGenerateTargetStateGuards(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	generate(t, dir, 10, 0, 1, false)

	_, err := fakevault.Generate(context.Background(), fakevault.Options{
		Dir: dir, Messages: 10, Seed: 1,
	})
	require.ErrorContains(err, "already exists")

	_, err = fakevault.Generate(context.Background(), fakevault.Options{
		Dir: t.TempDir(), Messages: 10, Seed: 1, Append: true,
	})
	require.ErrorContains(err, "cannot append")
}
