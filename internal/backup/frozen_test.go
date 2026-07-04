package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const frozenTestSchema = `
CREATE TABLE messages (id INTEGER PRIMARY KEY, sent_at TEXT);
CREATE TABLE conversations (id INTEGER PRIMARY KEY);
CREATE TABLE sources (id INTEGER PRIMARY KEY);
CREATE TABLE account_identities (id INTEGER PRIMARY KEY);
CREATE TABLE labels (id INTEGER PRIMARY KEY);
CREATE TABLE attachments (
  id INTEGER PRIMARY KEY,
  content_hash TEXT,
  storage_path TEXT,
  size INTEGER,
  thumbnail_hash TEXT,
  thumbnail_path TEXT
);
`

func newFrozenTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(frozenTestSchema)
	require.NoError(t, err)
	seed := `
INSERT INTO messages (sent_at) VALUES ('2026-01-01T00:00:00Z'), ('2026-02-01T00:00:00Z');
INSERT INTO conversations DEFAULT VALUES;
INSERT INTO sources DEFAULT VALUES;
INSERT INTO account_identities DEFAULT VALUES;
INSERT INTO labels DEFAULT VALUES;
INSERT INTO labels DEFAULT VALUES;
INSERT INTO attachments (content_hash, storage_path, size, thumbnail_hash, thumbnail_path) VALUES
  ('aa11', 'aa/aa11', 100, 'tt77', 'tt/tt77'),
  ('aa11', 'aa/aa11', 100, '', ''),
  ('bb22', 'bb/bb22', 50, '', ''),
  ('cc33', 'https://example.com/x', 5, '', ''),
  ('dd44', 'dd/dd44', NULL, '', ''),
  ('ee55', 'http-cache/ee/ee55', 25, '', ''),
  ('', '', 0, '', '');
`
	_, err = db.Exec(seed)
	require.NoError(t, err)
	return path, db
}

func TestFrozenSessionPinsAndCounts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	path, writer := newFrozenTestDB(t)
	s, err := OpenFrozenSession(ctx, path, NoopFreezeCoordinator{})
	require.NoError(err)
	defer func() { require.NoError(s.Close()) }()

	assert.Positive(s.PageSize)
	assert.Positive(s.PageCount)

	// WAL was checkpoint-truncated at open.
	walInfo, statErr := os.Stat(path + "-wal")
	if statErr == nil {
		assert.Zero(walInfo.Size(), "WAL should be truncated")
	}

	// A concurrent writer proceeds (into the WAL) while the session is open.
	_, err = writer.ExecContext(ctx, `INSERT INTO messages (sent_at) VALUES ('2026-03-01T00:00:00Z')`)
	require.NoError(err)

	// The pinned snapshot does not see the new row.
	stats, err := s.Stats(ctx)
	require.NoError(err)
	assert.Equal(int64(2), stats.Messages)
	assert.Equal(int64(1), stats.Conversations)
	assert.Equal(int64(1), stats.Sources)
	assert.Equal(int64(1), stats.Accounts)
	assert.Equal(int64(2), stats.Labels)
	assert.Equal(int64(7), stats.AttachmentRows)
	assert.Equal(int64(5), stats.AttachmentBlobs,
		"aa11, bb22, NULL-size dd44, http-cache-namespaced ee55, and thumbnail tt77; URL-backed and placeholder excluded")
	assert.Equal("2026-01-01T00:00:00Z", stats.DateRange[0])
	assert.Equal("2026-02-01T00:00:00Z", stats.DateRange[1])

	refs, err := s.AttachmentRefs(ctx)
	require.NoError(err)
	require.Len(refs, 5)
	assert.Equal(ContentRef{Hash: "aa11", Size: 100, StoragePath: "aa/aa11"}, refs[0])
	assert.Equal(ContentRef{Hash: "bb22", Size: 50, StoragePath: "bb/bb22"}, refs[1])
	assert.Equal(ContentRef{Hash: "dd44", Size: -1, StoragePath: "dd/dd44"}, refs[2],
		"a NULL size column must not fail the scan; capture resolves the real size from the file")
	assert.Equal(ContentRef{Hash: "ee55", Size: 25, StoragePath: "http-cache/ee/ee55"}, refs[3],
		"a local path is free to START with http; only http:// and https:// URLs are excluded")
	assert.Equal(ContentRef{Hash: "tt77", Size: -1, StoragePath: "tt/tt77"}, refs[4])
}

func TestFrozenSessionCoordinatorErrors(t *testing.T) {
	require := require.New(t)
	path, _ := newFrozenTestDB(t)
	fc := &recordingCoordinator{beginErr: assert.AnError}
	_, err := OpenFrozenSession(context.Background(), path, fc)
	require.ErrorIs(err, assert.AnError)

	fc = &recordingCoordinator{}
	s, err := OpenFrozenSession(context.Background(), path, fc)
	require.NoError(err)
	require.NoError(s.Close())
	require.True(fc.began)
	require.True(fc.ended, "gate must be released after the freeze window")
}

// TestFrozenSessionJoinsPinErrorAndEndError pins the fix ensuring
// OpenFrozenSession still calls fc.End even when openPinnedSession fails, and
// surfaces End's error alongside the pin failure instead of discarding it.
// dbPath is a directory (not a sqlite file), which fails openPinnedSession's
// first query deterministically and quickly.
func TestFrozenSessionJoinsPinErrorAndEndError(t *testing.T) {
	require := require.New(t)
	dbPath := t.TempDir()
	fc := &recordingCoordinator{endErr: assert.AnError}

	_, err := OpenFrozenSession(context.Background(), dbPath, fc)
	require.Error(err)
	require.True(fc.began)
	require.True(fc.ended, "End must still be called after a pin failure")
	require.ErrorIs(err, assert.AnError, "End's error must surface, not be dropped")
	require.ErrorContains(err, "unable to open database file",
		"the pin failure must still surface alongside End's error")
}

type recordingCoordinator struct {
	beginErr error
	endErr   error
	began    bool
	ended    bool
}

func (c *recordingCoordinator) Begin(context.Context) error {
	c.began = true
	return c.beginErr
}

func (c *recordingCoordinator) End(context.Context) error {
	c.ended = true
	return c.endErr
}
