package backupapp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
)

// seedDB creates the minimal msgvault-shaped schema (same shape as the
// internal/backupapp/testdata/compat fixture) with 2 messages, 2 attachments,
// 1 thumbnail, one attachment recorded at a non-canonical namespaced path.
func seedDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "msgvault.db")
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
		`INSERT INTO attachments
			(content_hash, storage_path, thumbnail_hash, thumbnail_path, size) VALUES
			('aabb01', 'aa/aabb01', 'ccdd02', 'cc/ccdd02', 10),
			('eeff03', 'imports/eeff03', NULL, NULL, 20)`,
	} {
		_, err := db.Exec(stmt)
		require.NoError(t, err, "seed: %s", stmt)
	}
	return dbPath
}

func TestFrozenViewContentInfoAndStats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	app := backupapp.New("test")
	session, err := backup.OpenFrozenSession(
		context.Background(), seedDB(t), backup.NoopFreezeCoordinator{})
	require.NoError(err)
	defer func() { require.NoError(session.Close()) }()
	view := app.FrozenView(session)

	info, err := view.ContentInfo(context.Background())
	require.NoError(err)
	assert.Len(info.Refs, 3) // 2 content hashes + 1 thumbnail
	assert.Equal(int64(2), info.Rows)
	assert.True(info.NonCanonicalPaths) // 'imports/eeff03'

	raw, err := view.Stats(context.Background())
	require.NoError(err)
	stats, err := backupapp.ParseStats(raw)
	require.NoError(err)
	assert.Equal(int64(2), stats.Messages)
	assert.Equal(int64(2), stats.AttachmentRows)
	assert.Equal(int64(3), stats.AttachmentBlobs)
	assert.Equal("2024-01-01T00:00:00Z", stats.DateRange[0])

	// Stats marshaling must be stable: ParseStats→Marshal reproduces raw.
	again, err := json.Marshal(stats)
	require.NoError(err)
	assert.Equal(string(raw), string(again))
}

func TestCheckManifest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	app := backupapp.New("test")
	statsJSON, err := json.Marshal(backupapp.Stats{AttachmentBlobs: 3})
	require.NoError(err)

	matching := &backup.Manifest{
		Stats:       statsJSON,
		Attachments: backup.ManifestAttachments{Blobs: 3},
	}
	assert.Empty(app.CheckManifest(matching))

	mismatched := &backup.Manifest{
		Stats:       statsJSON,
		Attachments: backup.ManifestAttachments{Blobs: 5},
	}
	problems := app.CheckManifest(mismatched)
	require.Len(problems, 1)
	assert.Contains(problems[0], "3")
	assert.Contains(problems[0], "5")

	unreadable := &backup.Manifest{
		Stats:       json.RawMessage(`not json`),
		Attachments: backup.ManifestAttachments{Blobs: 1},
	}
	problems = app.CheckManifest(unreadable)
	require.Len(problems, 1)
	assert.Contains(problems[0], "manifest stats unreadable")
}

func TestAppConstants(t *testing.T) {
	assert := assert.New(t)

	app := backupapp.New("1.2.3")
	assert.Equal("msgvault.db", app.DBFileName())
	assert.Equal("attachments", app.ContentDirName())
	assert.Equal(".mvpack", app.PackFileExtension())
	assert.Equal("1.2.3", app.Version())
	assert.Equal(
		[]string{"vectors.db", "analytics/", "logs/", "imports/", "tmp/", "locks"},
		app.ExcludedPaths())
}
