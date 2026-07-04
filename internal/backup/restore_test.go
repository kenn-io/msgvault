package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fileSHA256(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return sha256.Sum256(data)
}

// snapshotDirHashes maps every regular file under root (relative path) to
// its content hash, for whole-tree equality comparisons.
func snapshotDirHashes(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		require.NoError(t, err)
		out[rel] = fileSHA256(t, path)
		return nil
	})
	require.NoError(t, err)
	return out
}

// TestRestoreReproducesArchiveByteForByte is the restore proof's proof: a
// restored snapshot's database is byte-identical to the live database file
// as it existed at capture time, attachments and extras land byte-identical
// in the live layout, and this holds for a parent snapshot restored from an
// incremental chain, not just the latest.
func TestRestoreReproducesArchiveByteForByte(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, writer := seedBackupFixture(t)
	cacheDir := t.TempDir()

	// An extras file rides along (the deletions dir is always captured).
	deletionsPath := filepath.Join(dataDir, "deletions", "manifest-1.json")
	require.NoError(os.MkdirAll(filepath.Dir(deletionsPath), 0o700))
	require.NoError(os.WriteFile(deletionsPath, []byte(`{"id":"manifest-1"}`), 0o640))
	// WriteFile's mode is umask-filtered; pin the mode so the
	// capture-and-restore round trip has a distinctive value to preserve.
	require.NoError(os.Chmod(deletionsPath, 0o640))

	m1, err := Create(ctx, r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	// Create checkpoint-truncated the WAL inside the freeze and nothing has
	// written since, so the on-disk file is exactly the captured state.
	dbAtSnap1, err := os.ReadFile(dbPath)
	require.NoError(err)

	// Mutate the archive and take an incremental child snapshot. One added
	// attachment lives in the loose layout, one under an importer-style
	// namespaced storage path — restore must reproduce both placements. The
	// namespace deliberately starts with "http": only http:// and https://
	// URLs are excluded from capture, never local paths sharing the prefix.
	_, err = writer.ExecContext(ctx, `INSERT INTO messages (sent_at) VALUES ('2026-03-01T00:00:00Z')`)
	require.NoError(err)
	newRef := writeLooseAttachment(t, attachmentsDir, []byte("attachment added after snapshot 1"))
	nsRef := writeNamespacedAttachment(t, attachmentsDir, "http-cache", []byte("namespaced attachment"))
	_, err = writer.ExecContext(ctx,
		`INSERT INTO attachments (content_hash, storage_path, size, thumbnail_hash, thumbnail_path)
		 VALUES (?, ?, ?, '', ''), (?, ?, ?, '', '')`,
		newRef.Hash, newRef.Hash[:2]+"/"+newRef.Hash, newRef.Size,
		nsRef.Hash, nsRef.StoragePath, nsRef.Size)
	require.NoError(err)
	m2, err := Create(ctx, r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	require.Equal(m1.SnapshotID, m2.ParentID)
	dbAtSnap2, err := os.ReadFile(dbPath)
	require.NoError(err)

	// Restore the latest snapshot and compare byte-for-byte.
	target2 := filepath.Join(t.TempDir(), "restore-2")
	var events []ProgressEvent
	res2, err := Restore(ctx, r, RestoreOptions{
		TargetDir: target2,
		Progress:  func(ev ProgressEvent) { events = append(events, ev) },
	})
	require.NoError(err)
	assert.Equal(m2.SnapshotID, res2.SnapshotID)
	restored2, err := os.ReadFile(res2.DBPath)
	require.NoError(err)
	require.True(bytes.Equal(dbAtSnap2, restored2),
		"restored database must be byte-identical to the live file at capture time")
	assert.Equal(m2.Attachments.Blobs, res2.AttachmentBlobs)
	assert.Equal(m2.Attachments.BlobBytes, res2.AttachmentBytes)

	// Attachments land in the loose layout with matching bytes.
	assert.Equal(snapshotDirHashes(t, attachmentsDir), snapshotDirHashes(t, filepath.Join(target2, "attachments")))

	// Extras land at their captured relative path with mode preserved.
	restoredDeletions := filepath.Join(target2, "deletions", "manifest-1.json")
	assert.Equal(fileSHA256(t, deletionsPath), fileSHA256(t, restoredDeletions))
	if runtime.GOOS != "windows" {
		// Windows has no POSIX permission bits — Stat reports 0666 for any
		// writable file — so the exact-mode round trip is POSIX-only.
		info, err := os.Stat(restoredDeletions)
		require.NoError(err)
		assert.Equal(os.FileMode(0o640), info.Mode().Perm())
	}

	// Restoring the PARENT from the incremental chain reproduces the older
	// state, not the current one.
	target1 := filepath.Join(t.TempDir(), "restore-1")
	res1, err := Restore(ctx, r, RestoreOptions{SnapshotID: m1.SnapshotID, TargetDir: target1})
	require.NoError(err)
	restored1, err := os.ReadFile(res1.DBPath)
	require.NoError(err)
	require.True(bytes.Equal(dbAtSnap1, restored1),
		"restoring the parent snapshot must reproduce the pre-mutation database")
	_, err = os.Stat(filepath.Join(target1, "attachments", newRef.Hash[:2], newRef.Hash))
	require.ErrorIs(err, os.ErrNotExist,
		"an attachment added after snapshot 1 must not appear in snapshot 1's restore")

	// Progress: the db, attachments, and proof stages all completed.
	final := map[ProgressStage]ProgressEvent{}
	for _, ev := range events {
		if ev.Final {
			final[ev.Stage] = ev
		}
	}
	for _, stage := range []ProgressStage{ProgressStageRestoreDB, ProgressStageAttachments, ProgressStageExtras, ProgressStageProof} {
		require.Contains(final, stage)
		assert.Equal(final[stage].Done, final[stage].Total, "stage %s must finish complete", stage)
	}
}

func TestRestoreRefusesNonEmptyTargetWithoutOverwrite(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	target := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(target, "existing.txt"), []byte("x"), 0o600))

	_, err = Restore(ctx, r, RestoreOptions{TargetDir: target})
	require.ErrorContains(err, "not empty")

	_, err = Restore(ctx, r, RestoreOptions{TargetDir: target, Overwrite: true})
	require.NoError(err)
}

// TestRestoreOverwriteRemovesStaleDBSidecars pins the --overwrite hazard: a
// stale -wal/-shm pair left next to the restored database would be replayed
// over the proven bytes on its first normal SQLite open, so overwrite must
// remove them even though it merges the rest of the tree.
func TestRestoreOverwriteRemovesStaleDBSidecars(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	target := t.TempDir()
	for _, name := range []string{"msgvault.db", "msgvault.db-wal", "msgvault.db-shm"} {
		require.NoError(os.WriteFile(filepath.Join(target, name), []byte("stale "+name), 0o600))
	}
	unrelated := filepath.Join(target, "keep-me.txt")
	require.NoError(os.WriteFile(unrelated, []byte("survives the merge"), 0o600))

	res, err := Restore(ctx, r, RestoreOptions{TargetDir: target, Overwrite: true})
	require.NoError(err)
	for _, name := range []string{"msgvault.db-wal", "msgvault.db-shm"} {
		_, err := os.Stat(filepath.Join(target, name))
		require.ErrorIs(err, os.ErrNotExist, "stale sidecar %s must not survive an overwrite restore", name)
	}
	require.Equal(fileSHA256(t, dbPath), fileSHA256(t, res.DBPath),
		"the stale database must be fully replaced, not merged")
	_, err = os.Stat(unrelated)
	require.NoError(err, "overwrite merges: unrelated files stay in place")
}

// TestRestoreTargetPathWithURISyntax pins the proof DSN construction: a
// target path containing '?' or '#' must reach SQLite as a path, not be
// misparsed as URI query/fragment syntax.
func TestRestoreTargetPathWithURISyntax(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	// '?' is illegal in Windows filenames, so exercise it only where the
	// filesystem allows it; '#' and space are legal everywhere.
	dirName := "odd? dir#name"
	if runtime.GOOS == "windows" {
		dirName = "odd dir#name"
	}
	target := filepath.Join(t.TempDir(), dirName)
	res, err := Restore(ctx, r, RestoreOptions{TargetDir: target})
	require.NoError(err)
	require.Equal(fileSHA256(t, dbPath), fileSHA256(t, res.DBPath))
}

// TestRestoredDBDSNDrivePath pins the Windows DSN shape: a drive-letter
// path must be rooted with a slash so SQLite's URI parser reads it as a
// path rather than a URI authority. Windows-only: elsewhere filepath.Abs
// treats a drive-letter path as relative.
func TestRestoredDBDSNDrivePath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive-letter paths are only absolute on Windows")
	}
	assert.Equal(t, "file:///C:/Users/x%20y/msgvault.db?immutable=1&mode=ro",
		restoredDBDSN(`C:\Users\x y\msgvault.db`))
}

// TestRestoredDBDSNRelativePath pins that a relative path resolves against
// the working directory instead of being rooted at "/".
func TestRestoredDBDSNRelativePath(t *testing.T) {
	require := require.New(t)
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	require.NoError(err)

	rel := filepath.Join("out", "msgvault.db")
	dsn := restoredDBDSN(rel)
	require.Equal(restoredDBDSN(filepath.Join(cwd, rel)), dsn,
		"relative and absolute forms of the same path must produce the same DSN")
	require.NotContains(dsn, "file:///out",
		"a relative path must not be rooted at /")
}

// TestRestoreRelativeTarget pins that a relative --target works end to end:
// the proof DSN must resolve the restored database against the working
// directory, not root the relative path at "/".
func TestRestoreRelativeTarget(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	t.Chdir(t.TempDir())
	res, err := Restore(ctx, r, RestoreOptions{TargetDir: "restore-out"})
	require.NoError(err)
	require.Equal(fileSHA256(t, dbPath), fileSHA256(t, res.DBPath))
}

// TestRestoreProofCatchesManifestStatsMismatch proves the proof fires: a
// self-consistent manifest (valid content-derived ID) whose recorded stats
// disagree with the captured pages must fail the restore, not pass silently.
func TestRestoreProofCatchesManifestStatsMismatch(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	m, err := Create(ctx, r, createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	path := r.Path(snapshotsDirName, m.SnapshotID+manifestExt)
	data, err := os.ReadFile(path)
	require.NoError(err)
	var doctored Manifest
	require.NoError(json.Unmarshal(data, &doctored))
	doctored.Stats.Messages++
	createdAt, err := time.Parse(time.RFC3339, doctored.CreatedAt)
	require.NoError(err)
	forgedID, err := ComputeSnapshotID(createdAt, &doctored)
	require.NoError(err)
	doctored.SnapshotID = forgedID
	out, err := json.MarshalIndent(&doctored, "", "  ")
	require.NoError(err)
	require.NoError(os.Remove(path))
	require.NoError(os.WriteFile(r.Path(snapshotsDirName, forgedID+manifestExt), out, 0o600))

	_, err = Restore(ctx, r, RestoreOptions{TargetDir: filepath.Join(t.TempDir(), "restore")})
	require.ErrorContains(err, "do not match the manifest's recorded stats")
}

func TestRestoreDetectsCorruptPack(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	m, err := Create(ctx, r, createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	packID := m.NewPacks[0]
	path := r.Path("packs", packID[:2], packID+".mvpack")
	data, err := os.ReadFile(path)
	require.NoError(err)
	data[len(data)/3] ^= 0x01
	require.NoError(os.WriteFile(path, data, 0o600))

	_, err = Restore(ctx, r, RestoreOptions{TargetDir: filepath.Join(t.TempDir(), "restore")})
	require.Error(err, "a corrupted pack must fail the restore, never produce an unverified tree")
}

// TestRestoreJobsSerialMatchesParallel pins the --jobs contract for restore:
// serial and parallel runs produce byte-identical trees.
func TestRestoreJobsSerialMatchesParallel(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	targets := map[int]string{1: filepath.Join(t.TempDir(), "serial"), 8: filepath.Join(t.TempDir(), "parallel")}
	trees := map[int]map[string][32]byte{}
	for jobs, target := range targets {
		_, err := Restore(ctx, r, RestoreOptions{TargetDir: target, Jobs: jobs})
		require.NoError(err)
		trees[jobs] = snapshotDirHashes(t, target)
	}
	require.Equal(trees[1], trees[8])
}

// TestWriteRunRejectsOverflowingBlobOffset pins the subtraction-based bounds
// check: BlobOffset comes from a decoded page-map object, and a huge value
// must produce a restore error, not wrap the addition-based comparison and
// panic on the slice.
func TestWriteRunRejectsOverflowingBlobOffset(t *testing.T) {
	require := require.New(t)
	st := &restoreState{progress: newProgressEmitter(nil)}
	f, err := os.Create(filepath.Join(t.TempDir(), "db"))
	require.NoError(err)
	defer func() { _ = f.Close() }()

	raw := make([]byte, 4096)
	hm := &PageHashMap{PageSize: 4096, PageCount: 1}
	for _, offset := range []uint64{math.MaxUint64 - 100, math.MaxUint64, 4097} {
		err := st.writeRun(f, raw, blobID("b"), PageRun{StartPage: 0, PageCount: 1, BlobOffset: offset}, 4096, hm)
		require.ErrorContains(err, "overruns blob", "offset %d", offset)
	}
}

func TestRestoreExtrasEntryRejectsEscapingPaths(t *testing.T) {
	require := require.New(t)
	st := &restoreState{}
	target := t.TempDir()

	for _, path := range []string{"", "/etc/passwd", "../outside", "a/../../outside", ".."} {
		err := st.restoreExtrasEntry(ExtrasEntry{Path: path, Blob: blobID("x").String()}, target)
		require.ErrorContains(err, "escapes the restore target", "path %q", path)
	}
}

// TestRestoreExtrasEntryRejectsArchiveOverlap pins that a tampered extras
// tree cannot overwrite outputs restore already produced and proved: the
// database, its SQLite sidecars, and the attachments tree are off limits,
// case-insensitively (the default macOS filesystem folds case).
func TestRestoreExtrasEntryRejectsArchiveOverlap(t *testing.T) {
	require := require.New(t)
	st := &restoreState{}
	target := t.TempDir()

	for _, path := range []string{
		"msgvault.db", "MSGVAULT.DB", "msgvault.db-wal", "msgvault.db-shm",
		"attachments/aa/aa11", "Attachments/aa/aa11", "attachments",
	} {
		err := st.restoreExtrasEntry(ExtrasEntry{Path: path, Blob: blobID("x").String()}, target)
		require.ErrorContains(err, "overlaps restored archive content", "path %q", path)
	}

	// Traversal that lexically resolves onto a reserved path must be caught
	// too: the raw first segment looks safe, but filepath.Join cleans the
	// ".." away before the write.
	for _, path := range []string{
		"safe/../msgvault.db", "safe/../MSGVAULT.DB-wal", "safe/../attachments/aa/aa11",
	} {
		err := st.restoreExtrasEntry(ExtrasEntry{Path: path, Blob: blobID("x").String()}, target)
		require.ErrorContains(err, "overlaps restored archive content", "path %q", path)
	}

	// A path that cleans to the target directory itself is rejected outright.
	err := st.restoreExtrasEntry(ExtrasEntry{Path: "safe/..", Blob: blobID("x").String()}, target)
	require.ErrorContains(err, "escapes the restore target")

	// Legitimate extras still restore fine (proven end-to-end elsewhere);
	// here just confirm the reserved-name check does not reject them.
	err = st.restoreExtrasEntry(ExtrasEntry{Path: "deletions/manifest-1.json", Blob: "not-a-blob"}, target)
	require.NotContains(err.Error(), "overlaps restored archive content")
}
