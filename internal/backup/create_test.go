package backup

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/pack"
)

// seedBackupFixture builds a WAL sqlite DB (frozenTestSchema from
// frozen_test.go) plus a matching loose attachments dir, and returns
// (dbPath, attachmentsDir, dataDir, writer).
func seedBackupFixture(t *testing.T) (string, string, string, *sql.DB) {
	t.Helper()
	dataDir := t.TempDir()
	attachmentsDir := filepath.Join(dataDir, "attachments")
	contentA := []byte("first attachment content")
	contentB := []byte("second attachment content, longer")
	refA := writeLooseAttachment(t, attachmentsDir, contentA)
	refB := writeLooseAttachment(t, attachmentsDir, contentB)

	dbPath := filepath.Join(dataDir, "msgvault.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(frozenTestSchema)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO messages (sent_at) VALUES ('2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO attachments (content_hash, storage_path, size, thumbnail_hash, thumbnail_path)
		 VALUES (?, ?, ?, '', ''), (?, ?, ?, '', '')`,
		refA.Hash, refA.Hash[:2]+"/"+refA.Hash, refA.Size,
		refB.Hash, refB.Hash[:2]+"/"+refB.Hash, refB.Size)
	require.NoError(t, err)
	return dbPath, attachmentsDir, dataDir, db
}

func createOpts(dbPath, attachmentsDir, dataDir string, cacheDir string) CreateOptions {
	return CreateOptions{
		DBPath:          dbPath,
		AttachmentsDir:  attachmentsDir,
		DataDir:         dataDir,
		CacheDir:        cacheDir,
		MsgvaultVersion: "test",
	}
}

// TestCreateManifestReaderVersionTracksStoragePaths pins the compatibility
// gate for namespaced attachment paths: a snapshot restorable by placing
// every blob at the canonical "<aa>/<hash>" path stays at reader version 1,
// while one whose database records any other path requires the path-aware
// reader — a version-1 restore would report success with the database
// pointing at files that do not exist.
func TestCreateManifestReaderVersionTracksStoragePaths(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, writer := seedBackupFixture(t)
	cacheDir := t.TempDir()

	m1, err := Create(ctx, r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	assert.Equal(FormatVersion, m1.FormatVersion)
	assert.Equal(MinReaderVersion, m1.MinReaderVersion,
		"an all-canonical snapshot must stay readable by version-1 readers")

	// The same content hash gains a second row at a namespaced path. The
	// per-hash MIN(storage_path) capture selection stays canonical, so the
	// gate must inspect every row, not the captured refs.
	ref := writeLooseAttachment(t, attachmentsDir, []byte("hash recorded at two paths"))
	_, err = writer.ExecContext(ctx,
		`INSERT INTO attachments (content_hash, storage_path, size, thumbnail_hash, thumbnail_path)
		 VALUES (?, ?, ?, '', ''), (?, ?, ?, '', '')`,
		ref.Hash, ref.Hash[:2]+"/"+ref.Hash, ref.Size,
		ref.Hash, "ns-source/"+ref.Hash[:2]+"/"+ref.Hash, ref.Size)
	require.NoError(err)

	m2, err := Create(ctx, r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	assert.Equal(dbPathManifestVersion, m2.FormatVersion)
	assert.Equal(dbPathManifestVersion, m2.MinReaderVersion,
		"a namespaced-path snapshot must be refused by version-1 readers")

	// The current reader restores it, of course.
	_, err = Restore(ctx, r, RestoreOptions{TargetDir: filepath.Join(t.TempDir(), "restore")})
	require.NoError(err)
}

// TestStorePageBlobsJobsSerialMatchesParallel pins the --jobs contract for
// the pack stage: any worker count yields the same page-map delta (blob
// order, run set) and the same readable blobs as a strictly serial run,
// including dedup of identical blob content appearing in two plans.
func TestStorePageBlobsJobsSerialMatchesParallel(t *testing.T) {
	require := require.New(t)
	pageSize := uint32(4096)
	pageCount := uint64(3000)

	path := filepath.Join(t.TempDir(), "pages")
	content := make([]byte, pageCount*uint64(pageSize))
	for p := range pageCount {
		fill := byte(p % 251)
		if p < 256 || (p >= 500 && p < 756) {
			fill = 0 // two identical zero regions: same blob content twice
		}
		start := p * uint64(pageSize)
		for i := range uint64(pageSize) {
			content[start+i] = fill
		}
	}
	require.NoError(os.WriteFile(path, content, 0o600))
	f, err := os.Open(path)
	require.NoError(err)
	defer func() { _ = f.Close() }()

	// Hand-crafted dirty set: two dedicated-blob regions with identical
	// content, plus enough scattered small ranges to split into multiple
	// grouped plans.
	scan := &ScanResult{PageSize: pageSize, PageCount: pageCount}
	scan.Dirty = append(scan.Dirty, PageRange{Start: 0, Count: 256}, PageRange{Start: 500, Count: 256})
	for i := range uint64(800) {
		scan.Dirty = append(scan.Dirty, PageRange{Start: 1000 + i*2, Count: 2})
	}

	run := func(jobs int) (*PageMap, *Repo, []IndexEntry) {
		r := initTestRepo(t)
		appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
		delta, err := storePageBlobs(context.Background(), f, scan, appender, jobs, newProgressEmitter(nil))
		require.NoError(err)
		_, entries, err := appender.Finish()
		require.NoError(err)
		return delta, r, entries
	}
	serial, _, serialEntries := run(1)
	parallel, parallelRepo, parallelEntries := run(8)

	require.Equal(serial, parallel, "the delta must not depend on jobs")
	require.Len(parallelEntries, len(serialEntries))
	require.Less(len(parallel.Blobs), len(parallel.Runs),
		"the duplicate zero regions must share one blob")

	known := map[pack.BlobID]IndexEntry{}
	for _, e := range parallelEntries {
		known[e.Blob] = e
	}
	for _, id := range parallel.Blobs {
		blob, err := parallelRepo.ReadBlob(known, id, nil)
		require.NoError(err)
		require.Equal(id, pack.ComputeBlobID(blob))
	}
}

func TestCreateInitialSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)

	m, err := Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	assert.NotEmpty(m.SnapshotID)
	assert.Empty(m.ParentID)
	assert.Equal("sqlite", m.DB.Engine)
	assert.Zero(m.DB.MapChainDepth, "initial snapshot is a keyframe")
	assert.Positive(m.DB.PageCount)
	assert.Equal([]string{"loose"}, m.Attachments.Layout)
	assert.Equal(int64(2), m.Attachments.Blobs)
	assert.Len(m.Attachments.Lists, 1)
	assert.Empty(m.Attachments.Recipes)
	assert.Equal(int64(2), m.Stats.AttachmentRows)
	assert.NotEmpty(m.NewPacks)
	assert.NotEmpty(m.NewIndex)
	assert.Positive(m.BytesAdded)
	assert.Equal(manifestExcluded, m.Excluded)

	list, err := r.ListSnapshots()
	require.NoError(err)
	require.Len(list, 1)
	assert.Equal(m.SnapshotID, list[0].SnapshotID)

	// Repo config records the page size after the first backup.
	reopened, err := Open(r.Root())
	require.NoError(err)
	assert.Equal(int(m.DB.PageSize), reopened.Config().PageSize)

	// Staging is clean after a successful run.
	entries, err := os.ReadDir(r.Path("staging"))
	require.NoError(err)
	assert.Empty(entries)
}

func TestCreateIncrementalSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, db := seedBackupFixture(t)
	cacheDir := t.TempDir()

	m1, err := Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	// Mutate: new message and a new attachment.
	contentC := []byte("third attachment added later")
	refC := writeLooseAttachment(t, attachmentsDir, contentC)
	_, err = db.Exec(`INSERT INTO messages (sent_at) VALUES ('2026-02-01T00:00:00Z')`)
	require.NoError(err)
	_, err = db.Exec(
		`INSERT INTO attachments (content_hash, storage_path, size, thumbnail_hash, thumbnail_path)
		 VALUES (?, ?, ?, '', '')`,
		refC.Hash, refC.Hash[:2]+"/"+refC.Hash, refC.Size)
	require.NoError(err)

	m2, err := Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	assert.Equal(m1.SnapshotID, m2.ParentID)
	assert.Equal(1, m2.DB.MapChainDepth)
	assert.Equal(int64(3), m2.Attachments.Blobs)
	assert.Len(m2.Attachments.Lists, 2, "parent lists carried over plus one new segment")
	assert.Equal(m1.Attachments.Lists[0], m2.Attachments.Lists[0])
	assert.Less(m2.BytesAdded, m1.BytesAdded, "incremental adds a fraction of the initial bytes")

	// The cache was refreshed to the new snapshot.
	snapID, cached, err := LoadHashMapCache(cacheDir, r.Config().RepoID)
	require.NoError(err)
	assert.Equal(m2.SnapshotID, snapID)
	require.NotNil(cached)
	assert.Equal(m2.DB.PageCount, cached.PageCount)
}

// TestCreateSameSecondChainOrder covers the bug where two Create calls
// landing in the same wall-clock second produce snapshot IDs whose
// timestamp prefixes tie, leaving lexicographic order (what
// ListSnapshots/LatestSnapshot rely on) to fall back to the uncorrelated
// content-hash suffix. Create must bump CreatedAt so IDs stay strictly
// increasing regardless of wall-clock timing.
func TestCreateSameSecondChainOrder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, db := seedBackupFixture(t)
	cacheDir := t.TempDir()

	m1, err := Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	_, err = db.Exec(`INSERT INTO messages (sent_at) VALUES ('2026-02-01T00:00:00Z')`)
	require.NoError(err)
	m2, err := Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	assert.Greater(m2.SnapshotID, m1.SnapshotID, "second snapshot's ID must sort after the first")
	assert.Equal(m1.SnapshotID, m2.ParentID)

	latest, err := r.LatestSnapshot()
	require.NoError(err)
	assert.Equal(m2.SnapshotID, latest.SnapshotID, "LatestSnapshot must return the true latest, not the older tie")

	_, err = db.Exec(`INSERT INTO messages (sent_at) VALUES ('2026-03-01T00:00:00Z')`)
	require.NoError(err)
	m3, err := Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	assert.Greater(m3.SnapshotID, m2.SnapshotID, "chain must keep extending in strictly increasing order")
	assert.Equal(m2.SnapshotID, m3.ParentID, "third snapshot's parent must be the second, not orphaned")
}

// seedThumbnailFixture builds a fixture with one attachment that also has a
// distinct thumbnail blob, so the manifest counters exercise the thumbnail
// population (content hash UNION thumbnail hash).
func seedThumbnailFixture(t *testing.T) (string, string, string) {
	t.Helper()
	dataDir := t.TempDir()
	attachmentsDir := filepath.Join(dataDir, "attachments")
	refA := writeLooseAttachment(t, attachmentsDir, []byte("attachment with a thumbnail"))
	refT := writeLooseAttachment(t, attachmentsDir, []byte("thumbnail bytes for A"))

	dbPath := filepath.Join(dataDir, "msgvault.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(frozenTestSchema)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO messages (sent_at) VALUES ('2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO attachments (content_hash, storage_path, size, thumbnail_hash, thumbnail_path)
		 VALUES (?, ?, ?, ?, ?)`,
		refA.Hash, refA.Hash[:2]+"/"+refA.Hash, refA.Size,
		refT.Hash, refT.Hash[:2]+"/"+refT.Hash)
	require.NoError(t, err)
	return dbPath, attachmentsDir, dataDir
}

// listUnion decodes a manifest's attachment lists into their ref union.
func listUnion(t *testing.T, r *Repo, m *Manifest) []ContentRef {
	t.Helper()
	known, err := r.LoadBlobIndex()
	require.NoError(t, err)
	refs, _, err := LoadListRefs(r, known, m.Attachments.Lists, nil)
	require.NoError(t, err)
	return refs
}

// TestCreateThumbnailManifestCountersAgree pins Critical 2: a snapshot whose
// attachments carry thumbnails must produce manifest counters that agree, so
// full Verify reports no problems. Before the fix, Stats.AttachmentBlobs
// counted only content hashes (excluding thumbnails) while Attachments.Blobs
// counted the captured refs (including thumbnails), and Verify enforced
// equality between them.
func TestCreateThumbnailManifestCountersAgree(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir := seedThumbnailFixture(t)

	m, err := Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	assert.Equal(int64(2), m.Attachments.Blobs, "one content blob plus one thumbnail blob")
	assert.Equal(m.Attachments.Blobs, m.Stats.AttachmentBlobs, "stats population must match attachments")
	assert.Len(listUnion(t, r, m), 2, "list union must match the manifest counter")

	res, err := Verify(context.Background(), r, VerifyOptions{})
	require.NoError(err)
	assert.Empty(res.Problems)
}

// TestCreateAttachmentDeletionKeepsVerifyClean pins Critical 1: after a local
// attachment deletion, a fresh snapshot must not inherit a parent list union
// that is a strict superset of the current ref set, or Verify would
// permanently fail otherwise healthy snapshots. Snapshot 2 must carry a fresh
// full list of exactly the surviving refs while snapshot 1 stays verifiable.
func TestCreateAttachmentDeletionKeepsVerifyClean(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, db := seedBackupFixture(t)
	cacheDir := t.TempDir()

	m1, err := Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	require.Equal(int64(2), m1.Attachments.Blobs)

	// Delete attachment B (highest id row) and its loose file.
	var hashB string
	require.NoError(db.QueryRow(`SELECT content_hash FROM attachments ORDER BY id DESC LIMIT 1`).Scan(&hashB))
	_, err = db.Exec(`DELETE FROM attachments WHERE content_hash = ?`, hashB)
	require.NoError(err)
	require.NoError(os.Remove(filepath.Join(attachmentsDir, hashB[:2], hashB)))

	m2, err := Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	var hashA string
	require.NoError(db.QueryRow(`SELECT content_hash FROM attachments`).Scan(&hashA))
	require.NotEqual(hashB, hashA)

	assert.Equal(int64(1), m2.Attachments.Blobs, "only the surviving attachment is captured")
	union := listUnion(t, r, m2)
	require.Len(union, 1, "snapshot 2 list union must equal exactly the surviving ref")
	assert.Equal(hashA, union[0].Hash)

	res, err := Verify(context.Background(), r, VerifyOptions{All: true})
	require.NoError(err)
	assert.Empty(res.Problems, "both snapshots verify cleanly after a deletion")
}

func TestNextCreatedAt(t *testing.T) {
	t.Run("no parent returns now unchanged", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		now := time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)
		got, err := nextCreatedAt(now, nil)
		require.NoError(err)
		assert.Equal(now, got)
	})

	t.Run("parent strictly before now returns now unchanged", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		now := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
		parent := &Manifest{CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)}
		got, err := nextCreatedAt(now, parent)
		require.NoError(err)
		assert.Equal(now, got)
	})

	t.Run("same second as parent bumps by one second", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		parentTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		now := parentTime.Add(500 * time.Millisecond)
		parent := &Manifest{CreatedAt: parentTime.Format(time.RFC3339)}
		got, err := nextCreatedAt(now, parent)
		require.NoError(err)
		assert.Equal(parentTime.Add(time.Second), got)
	})

	t.Run("now before parent (clock skew) still bumps forward", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		parentTime := time.Date(2026, 1, 1, 0, 0, 5, 0, time.UTC)
		now := parentTime.Add(-2 * time.Second)
		parent := &Manifest{CreatedAt: parentTime.Format(time.RFC3339)}
		got, err := nextCreatedAt(now, parent)
		require.NoError(err)
		assert.Equal(parentTime.Add(time.Second), got)
	})

	t.Run("malformed parent created_at is an error", func(t *testing.T) {
		require := require.New(t)
		parent := &Manifest{SnapshotID: "bad-parent", CreatedAt: "not-a-timestamp"}
		_, err := nextCreatedAt(time.Now(), parent)
		require.Error(err)
	})
}

func TestCreateNoChanges(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	cacheDir := t.TempDir()

	_, err := Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	m2, err := Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	// Only the tiny map/hash delta objects are new; no content re-uploaded.
	assert.Equal(int64(2), m2.Attachments.Blobs)
	assert.Len(m2.Attachments.Lists, 1, "no new list segment when nothing new")
}

// TestCreatePageSizeChangeForcesFullRecapture covers the case where the
// source DB is rebuilt at a different page size between snapshots (e.g. a
// VACUUM with a new PRAGMA page_size). The parent's page/hash-map chain is
// the old page size and can no longer be merged against; Create must fall
// back to a full re-capture instead of hard-erroring on every future run.
func TestCreatePageSizeChangeForcesFullRecapture(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, db := seedBackupFixture(t)
	cacheDir := t.TempDir()

	m1, err := Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	rebuildAtPageSize(t, db, 8192)

	m2, err := Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	assert.Equal(m1.SnapshotID, m2.ParentID, "lineage still records the true parent snapshot")
	assert.Zero(m2.DB.MapChainDepth, "page-size mismatch forces a keyframe")
	assert.EqualValues(8192, m2.DB.PageSize)
	assert.NotEqual(m1.DB.PageSize, m2.DB.PageSize)

	// The new page map must be a self-contained keyframe: materializing it
	// with no parent chain involved still has to cover every page.
	known, err := r.LoadBlobIndex()
	require.NoError(err)
	fetch := func(id pack.BlobID) ([]byte, error) { return r.ReadBlob(known, id, nil) }
	loaded, err := r.LoadManifest(m2.SnapshotID)
	require.NoError(err)
	chain, err := r.PageMapChain(loaded)
	require.NoError(err)
	assert.Len(chain, 1, "keyframe chain is a single blob")
	full, err := MaterializePageMap(fetch, chain)
	require.NoError(err)
	assert.NoError(full.CheckCoverage())
}

// rebuildAtPageSize rewrites db's on-disk page size via VACUUM, mirroring
// what a `PRAGMA page_size=N; VACUUM;` maintenance pass does to a live
// msgvault database. Page size can only change outside WAL mode, so this
// pins a single connection and toggles the journal mode around the vacuum.
func rebuildAtPageSize(t *testing.T, db *sql.DB, pageSize int) {
	t.Helper()
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()

	_, err = conn.ExecContext(context.Background(), `PRAGMA journal_mode=DELETE`)
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), fmt.Sprintf(`PRAGMA page_size=%d`, pageSize))
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), `VACUUM`)
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), `PRAGMA journal_mode=WAL`)
	require.NoError(t, err)
}

func TestCreateHoldsExclusiveLock(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)

	shared, err := r.AcquireSharedLock("verify", false)
	require.NoError(err)
	defer func() { require.NoError(shared.Release()) }()

	oldTimeout, oldPoll := sharedWaitTimeout, sharedWaitPoll
	sharedWaitTimeout, sharedWaitPoll = 200*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { sharedWaitTimeout, sharedWaitPoll = oldTimeout, oldPoll })

	_, err = Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.ErrorIs(err, ErrRepoLocked)
}

// TestCreatePhasesHonorCancellation pins that the long capture phases
// observe context cancellation instead of running to completion: an
// already-canceled context must surface ctx.Err() from each phase without
// deadlocking its worker pipeline.
func TestCreatePhasesHonorCancellation(t *testing.T) {
	require := require.New(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	pageSize := uint32(4096)
	db := fakeDB(t, pageSize, 64)
	_, err := ScanPages(canceled, bytes.NewReader(db), pageSize, 64, nil, nil)
	require.ErrorIs(err, context.Canceled, "ScanPages")

	path := filepath.Join(t.TempDir(), "pages")
	require.NoError(os.WriteFile(path, db, 0o600))
	f, err := os.Open(path)
	require.NoError(err)
	defer func() { _ = f.Close() }()
	scan := &ScanResult{PageSize: pageSize, PageCount: 64, Dirty: []PageRange{{Start: 0, Count: 64}}}
	r := initTestRepo(t)
	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
	defer appender.Abort()
	_, err = storePageBlobs(canceled, f, scan, appender, 4, newProgressEmitter(nil))
	require.ErrorIs(err, context.Canceled, "storePageBlobs")

	dir := t.TempDir()
	ref := writeLooseAttachment(t, dir, []byte("attachment content"))
	capAppender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
	defer capAppender.Abort()
	_, err = CaptureAttachments(canceled, dir, []ContentRef{ref}, map[string]bool{}, capAppender, CaptureOptions{Jobs: 4})
	require.ErrorIs(err, context.Canceled, "CaptureAttachments")
}
