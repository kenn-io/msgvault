package backup_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/backup"
	"go.kenn.io/msgvault/internal/backupapp"
)

// seedCompatArchive builds a tiny msgvault-shaped archive: a SQLite DB with
// exactly the tables the backup stats/content queries touch, plus two
// attachment blobs (one with a thumbnail) at canonical <aa>/<hash> paths.
// All content is synthetic.
func seedCompatArchive(t *testing.T, dir string) (string, string) {
	t.Helper()
	dbPath := filepath.Join(dir, "msgvault.db")
	attDir := filepath.Join(dir, "attachments")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	for _, stmt := range []string{
		`CREATE TABLE messages (id INTEGER PRIMARY KEY, sent_at TEXT)`,
		`CREATE TABLE conversations (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE sources (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE account_identities (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE labels (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE attachments (id INTEGER PRIMARY KEY,
			content_hash TEXT, storage_path TEXT,
			thumbnail_hash TEXT, thumbnail_path TEXT, size INTEGER)`,
		`INSERT INTO messages (sent_at) VALUES
			('2024-01-01T00:00:00Z'), ('2024-06-01T00:00:00Z')`,
		`INSERT INTO conversations DEFAULT VALUES`,
		`INSERT INTO sources DEFAULT VALUES`,
		`INSERT INTO account_identities DEFAULT VALUES`,
		`INSERT INTO labels DEFAULT VALUES`,
	} {
		_, err := db.Exec(stmt)
		require.NoError(t, err, "seed statement: %s", stmt)
	}

	writeBlob := func(content string) string {
		sum := sha256.Sum256([]byte(content))
		hash := hex.EncodeToString(sum[:])
		p := filepath.Join(attDir, hash[:2], hash)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
		return hash
	}
	h1 := writeBlob("synthetic attachment one")
	h2 := writeBlob("synthetic attachment two")
	thumb := writeBlob("synthetic thumbnail bytes")
	_, err = db.Exec(`INSERT INTO attachments
		(content_hash, storage_path, thumbnail_hash, thumbnail_path, size) VALUES
		(?, ?, ?, ?, 24), (?, ?, NULL, NULL, 24)`,
		h1, h1[:2]+"/"+h1, thumb, thumb[:2]+"/"+thumb,
		h2, h2[:2]+"/"+h2)
	require.NoError(t, err)
	return dbPath, attDir
}

const compatRepoDir = "testdata/compat/repo"

// TestGenerateCompatFixture writes the committed fixture repository. It is
// env-gated: it ran once against pre-extraction code and its output is
// committed; regenerating it with post-extraction code would defeat the
// old-writer→new-reader guarantee.
func TestGenerateCompatFixture(t *testing.T) {
	if os.Getenv("MSGVAULT_GENERATE_COMPAT_FIXTURE") != "1" {
		t.Skip("set MSGVAULT_GENERATE_COMPAT_FIXTURE=1 to regenerate the committed fixture")
	}
	require.NoError(t, os.RemoveAll(compatRepoDir))
	archive := t.TempDir()
	dbPath, attDir := seedCompatArchive(t, archive)

	r, err := backup.Init(compatRepoDir)
	require.NoError(t, err)
	opts := backup.CreateOptions{
		DBPath:     dbPath,
		ContentDir: attDir,
		DataDir:    archive,
	}
	app := backupapp.New("compat-fixture")
	_, err = backup.Create(context.Background(), r, app, opts)
	require.NoError(t, err)

	// Second snapshot with a data change, so the fixture exercises the
	// incremental path: page deltas, parent chain, inherited lists.
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO messages (sent_at) VALUES ('2024-12-01T00:00:00Z')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = backup.Create(context.Background(), r, app, opts)
	require.NoError(t, err)
}

// copyFixtureRepo copies the committed fixture repository into a temp dir.
// Restore takes a shared repository lock, which creates a file under locks/;
// opening the fixture in place would write into the source tree and fail on
// a read-only checkout.
func copyFixtureRepo(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.CopyFS(dst, os.DirFS(compatRepoDir)))
	return dst
}

// TestRestoreCompatFixture proves a repository written by the pre-extraction
// code restores correctly. After the engine generalization this is the
// old-writer→new-reader compatibility direction.
func TestRestoreCompatFixture(t *testing.T) {
	r, err := backup.Open(copyFixtureRepo(t))
	require.NoError(t, err)
	snaps, err := r.ListSnapshots()
	require.NoError(t, err)
	require.Len(t, snaps, 2)
	// Pin the exact snapshot IDs of the committed fixture. If the env-gated
	// generator is ever rerun (producing a fixture written by post-refactor
	// code), these IDs change and this test fails — that is the point: the
	// fixture must remain the pre-extraction artifact.
	require.Equal(t,
		"20260706T135616Z-0f5afbf4852769c6ad683bdab6082649",
		snaps[0].SnapshotID)
	require.Equal(t,
		"20260706T135617Z-70996f725e26d0bbff1fb47d5e454074",
		snaps[1].SnapshotID)

	target := t.TempDir()
	res, err := backup.Restore(context.Background(), r, backupapp.New("test"), backup.RestoreOptions{
		TargetDir: filepath.Join(target, "restored"),
	})
	require.NoError(t, err)
	assert.Equal(t, snaps[1].SnapshotID, res.SnapshotID)
	assert.Equal(t, int64(3), res.AttachmentBlobs)

	// Mirror the engine's sqliteURIDSN shape: absolute, slash-separated,
	// slash-rooted — a raw Windows drive-letter path would otherwise be
	// misparsed as a URI authority.
	dbURIPath := res.DBPath
	if abs, err := filepath.Abs(dbURIPath); err == nil {
		dbURIPath = abs
	}
	dbURIPath = filepath.ToSlash(dbURIPath)
	if !strings.HasPrefix(dbURIPath, "/") {
		dbURIPath = "/" + dbURIPath
	}
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     dbURIPath,
		RawQuery: "immutable=1&mode=ro",
	}).String()
	db, err := sql.Open("sqlite3", dsn)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	var messages int64
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&messages))
	assert.Equal(t, int64(3), messages)
}
