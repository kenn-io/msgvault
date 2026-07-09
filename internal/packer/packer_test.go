package packer_test

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/blobstore"
	"go.kenn.io/msgvault/internal/packer"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// fixture seeds one message in a test store and provides helpers to create
// attachment rows with matching loose blob files under a temp attachments dir.
type fixture struct {
	t     *testing.T
	store *store.Store
	dir   string
	msgID int64
	seq   int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "thread-pack", "Pack Thread")
	require.NoError(err, "EnsureConversation")
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "pack-msg",
		MessageType:     "email",
		SizeEstimate:    100,
	})
	require.NoError(err, "UpsertMessage")
	return &fixture{t: t, store: st, dir: t.TempDir(), msgID: msgID}
}

// randomContent returns n incompressible bytes so pack sizes track raw sizes.
func randomContent(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := crand.Read(b)
	require.NoError(t, err, "rand.Read")
	return b
}

func hashOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func canonical(hash string) string { return hash[:2] + "/" + hash }

// addRow inserts an attachment row recording hash at the given relative path.
func (f *fixture) addRow(hash, relPath string, size int) {
	f.t.Helper()
	f.seq++
	err := f.store.UpsertAttachment(f.msgID, fmt.Sprintf("file-%d.bin", f.seq),
		"application/octet-stream", relPath, hash, size)
	require.NoErrorf(f.t, err, "UpsertAttachment(%s)", hash)
}

// writeLoose writes content at the slash-separated relPath under the
// attachments dir.
func (f *fixture) writeLoose(relPath string, content []byte) {
	f.t.Helper()
	full := filepath.Join(f.dir, filepath.FromSlash(relPath))
	require.NoError(f.t, os.MkdirAll(filepath.Dir(full), 0o700), "mkdir loose dir")
	require.NoError(f.t, os.WriteFile(full, content, 0o600), "write loose file")
}

// addBlob records an attachment row and writes the matching loose file at the
// path derived by pathOf; it returns the content hash.
func (f *fixture) addBlob(content []byte, pathOf func(hash string) string) string {
	f.t.Helper()
	h := hashOf(content)
	p := pathOf(h)
	f.addRow(h, p, len(content))
	f.writeLoose(p, content)
	return h
}

// setThumbnail sets thumbnail columns on the newest row with contentHash (no
// public API writes thumbnails yet, so those are set with direct SQL).
func (f *fixture) setThumbnail(contentHash, thumbHash, thumbPath string) {
	f.t.Helper()
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachments SET thumbnail_hash = ?, thumbnail_path = ?
		WHERE id = (SELECT MAX(id) FROM attachments WHERE content_hash = ?)`),
		thumbHash, thumbPath, contentHash)
	require.NoErrorf(f.t, err, "setThumbnail(%s)", thumbHash)
}

func (f *fixture) run(opts packer.Options) packer.Stats {
	f.t.Helper()
	stats, err := packer.Run(context.Background(), f.store, f.dir, opts)
	require.NoError(f.t, err, "packer.Run")
	return stats
}

// readBack reads a blob through the production read path.
func (f *fixture) readBack(hash string) []byte {
	f.t.Helper()
	require := require.New(f.t)
	bs := blobstore.New(f.store, f.dir)
	defer bs.Close() //nolint:errcheck // test cleanup
	r, _, err := bs.Open(hash)
	require.NoErrorf(err, "blobstore.Open(%s)", hash)
	defer r.Close() //nolint:errcheck // test cleanup
	data, err := io.ReadAll(r)
	require.NoError(err, "read blob")
	return data
}

// looseFiles returns the relative paths of all regular files under the
// attachments dir, excluding the packs subtree.
func (f *fixture) looseFiles() []string {
	f.t.Helper()
	packsDir := filepath.Join(f.dir, "packs")
	var files []string
	err := filepath.WalkDir(f.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == packsDir {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(f.dir, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(f.t, err, "walk attachments dir")
	return files
}

func (f *fixture) storagePaths(contentHash string) []string {
	f.t.Helper()
	require := require.New(f.t)
	rows, err := f.store.DB().Query(f.store.Rebind(`
		SELECT storage_path FROM attachments WHERE content_hash = ? ORDER BY id`), contentHash)
	require.NoError(err, "query storage_path")
	defer rows.Close() //nolint:errcheck // read-only cursor
	var paths []string
	for rows.Next() {
		var p string
		require.NoError(rows.Scan(&p), "scan storage_path")
		paths = append(paths, p)
	}
	require.NoError(rows.Err(), "rows.Err")
	return paths
}

func (f *fixture) thumbnailPath(thumbHash string) string {
	f.t.Helper()
	var p string
	err := f.store.DB().QueryRow(f.store.Rebind(`
		SELECT thumbnail_path FROM attachments WHERE thumbnail_hash = ?`), thumbHash).Scan(&p)
	require.NoErrorf(f.t, err, "query thumbnail_path for %s", thumbHash)
	return p
}

// buildOrphanPack seals a pack containing the given blobs directly into
// packs/<id[:2]>/ with no DB record, simulating a crash between seal and
// commit. It returns the pack ID.
func buildOrphanPack(t *testing.T, attachmentsDir string, blobs ...[]byte) string {
	t.Helper()
	require := require.New(t)
	packsDir := filepath.Join(attachmentsDir, "packs")
	require.NoError(os.MkdirAll(packsDir, 0o700), "mkdir packs dir")
	w, err := pack.NewWriter(packsDir, pack.WriterOptions{ZstdLevel: pack.DefaultZstdLevel})
	require.NoError(err, "pack.NewWriter")
	for _, b := range blobs {
		_, err := w.Append(b)
		require.NoError(err, "pack append")
	}
	id := w.ID()
	_, err = w.Seal(filepath.Join(packsDir, id[:2], id+blobstore.PackExt))
	require.NoError(err, "pack seal")
	return id
}

func TestRunPacksLooseBlobs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	var contents [][]byte
	var hashes []string
	for range 4 {
		c := randomContent(t, 600)
		contents = append(contents, c)
		hashes = append(hashes, f.addBlob(c, canonical))
	}
	ncContent := randomContent(t, 600)
	ncHash := f.addBlob(ncContent, func(h string) string {
		return "synctech-sms/" + h[:2] + "/" + h
	})
	contents = append(contents, ncContent)
	hashes = append(hashes, ncHash)

	thumbContent := randomContent(t, 200)
	thumbHash := hashOf(thumbContent)
	f.writeLoose("thumbs/"+thumbHash, thumbContent)
	f.setThumbnail(hashes[0], thumbHash, "thumbs/"+thumbHash)
	contents = append(contents, thumbContent)
	hashes = append(hashes, thumbHash)

	stats := f.run(packer.Options{TargetSize: 1024})

	assert.Equal(3, stats.PacksSealed, "1024-byte target packs two 600-byte blobs per pack")
	assert.Equal(6, stats.BlobsPacked)
	assert.Equal(int64(5*600+200), stats.BytesPacked)
	assert.Zero(stats.PacksAdopted)
	assert.Zero(stats.PacksRemoved)
	assert.Zero(stats.BlobsMissing)
	assert.Zero(stats.BlobsCorrupt)

	for i, h := range hashes {
		entry, err := f.store.GetAttachmentPackEntry(h)
		require.NoErrorf(err, "GetAttachmentPackEntry(%s)", h)
		require.NotNilf(entry, "blob %s must be indexed", h)
		assert.Equalf(contents[i], f.readBack(h), "blob %s reads back byte-identical", h)
	}

	recs, err := f.store.ListPackRecords()
	require.NoError(err)
	require.Len(recs, 3)
	for _, r := range recs {
		_, err := os.Stat(filepath.Join(f.dir, "packs", r.PackID[:2], r.PackID+blobstore.PackExt))
		require.NoErrorf(err, "pack file for %s exists under packs/<id[:2]>/", r.PackID)
	}

	assert.Empty(f.looseFiles(), "canonical and noncanonical loose files are gone")
	assert.Equal([]string{canonical(ncHash)}, f.storagePaths(ncHash),
		"noncanonical storage_path is canonicalized")
	assert.Equal(canonical(thumbHash), f.thumbnailPath(thumbHash),
		"thumbnail_path is canonicalized")
}

func TestRunSecondRunIsNoOp(t *testing.T) {
	assert := assert.New(t)
	f := newFixture(t)
	f.addBlob(randomContent(t, 600), canonical)
	f.addBlob(randomContent(t, 600), canonical)

	first := f.run(packer.Options{})
	assert.Equal(1, first.PacksSealed)
	assert.Equal(2, first.BlobsPacked)

	second := f.run(packer.Options{})
	assert.Equal(packer.Stats{}, second, "second run is a no-op")
}

func TestRunAdoptsOrphanPack(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	c1 := randomContent(t, 600)
	c2 := randomContent(t, 600)
	h1 := f.addBlob(c1, canonical)
	h2 := f.addBlob(c2, canonical)
	id := buildOrphanPack(t, f.dir, c1, c2)

	stats := f.run(packer.Options{})

	assert.Equal(1, stats.PacksAdopted)
	assert.Zero(stats.PacksSealed, "adopted blobs are not re-packed")
	assert.Zero(stats.BlobsPacked)
	assert.Equal(2, stats.LooseSwept, "loose files of adopted blobs are swept")

	has, err := f.store.HasPackRecord(id)
	require.NoError(err)
	assert.True(has, "adoption records the pack")
	for h, want := range map[string][]byte{h1: c1, h2: c2} {
		entry, err := f.store.GetAttachmentPackEntry(h)
		require.NoErrorf(err, "GetAttachmentPackEntry(%s)", h)
		require.NotNilf(entry, "blob %s indexed by adoption", h)
		assert.Equal(id, entry.PackID)
		assert.Equal(want, f.readBack(h))
	}
	assert.Empty(f.looseFiles())
}

func TestRunRemovesRedundantOrphanPack(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	c := randomContent(t, 600)
	h := f.addBlob(c, canonical)
	first := f.run(packer.Options{})
	require.Equal(1, first.PacksSealed)

	entryBefore, err := f.store.GetAttachmentPackEntry(h)
	require.NoError(err)
	require.NotNil(entryBefore)

	orphanID := buildOrphanPack(t, f.dir, c)
	second := f.run(packer.Options{})

	assert.Equal(1, second.PacksRemoved)
	assert.Zero(second.PacksAdopted)
	_, err = os.Stat(filepath.Join(f.dir, "packs", orphanID[:2], orphanID+blobstore.PackExt))
	require.ErrorIs(err, os.ErrNotExist, "redundant orphan pack file is deleted")
	has, err := f.store.HasPackRecord(orphanID)
	require.NoError(err)
	assert.False(has, "no record for the removed orphan")

	entryAfter, err := f.store.GetAttachmentPackEntry(h)
	require.NoError(err)
	require.NotNil(entryAfter)
	assert.Equal(entryBefore.PackID, entryAfter.PackID, "index still points at the original pack")
	assert.Equal(c, f.readBack(h))
}

func TestRunSweepsIndexedLooseLeftovers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	c := randomContent(t, 600)
	h := f.addBlob(c, canonical)
	// Simulate "indexed, loose not deleted": record the pack + index rows as
	// if a previous run crashed after commit but before source deletion.
	rec := store.PackRecord{
		PackID: "01hzy3v7q8r9s0t1u2v3w4x5y6", EntryCount: 1,
		StoredBytes: 600, CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(f.store.RecordPackedBlobs(rec, []store.PackIndexEntry{
		{BlobHash: h, PackID: rec.PackID, Offset: 6, StoredLen: 600, RawLen: 600},
	}))

	stats := f.run(packer.Options{})

	assert.Equal(1, stats.LooseSwept)
	assert.Zero(stats.PacksSealed)
	assert.Zero(stats.BlobsPacked)
	assert.Empty(f.looseFiles(), "lingering loose file removed by the sweep")
}

func TestRunRemovesStaleStagingFiles(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	packsDir := filepath.Join(f.dir, "packs")
	require.NoError(os.MkdirAll(packsDir, 0o700))
	staging := filepath.Join(packsDir, "01hzy3v7q8r9s0t1u2v3w4x5y6.staging")
	require.NoError(os.WriteFile(staging, []byte("dead mid-seal abort"), 0o600))

	stats := f.run(packer.Options{})

	assert.Equal(packer.Stats{}, stats)
	_, err := os.Stat(staging)
	assert.ErrorIs(err, os.ErrNotExist, "stale staging file removed at start")
}

func TestRunCountsMissingSourceFiles(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	missing := randomContent(t, 100)
	missingHash := hashOf(missing)
	f.addRow(missingHash, canonical(missingHash), len(missing))
	good := randomContent(t, 100)
	goodHash := f.addBlob(good, canonical)

	stats := f.run(packer.Options{})

	assert.Equal(1, stats.BlobsMissing)
	assert.Equal(1, stats.BlobsPacked, "run continues past the missing blob")
	entry, err := f.store.GetAttachmentPackEntry(missingHash)
	require.NoError(err)
	assert.Nil(entry, "missing blob stays unindexed for backfill")
	entry, err = f.store.GetAttachmentPackEntry(goodHash)
	require.NoError(err)
	assert.NotNil(entry)
}

func TestRunSkipsCorruptSourceFiles(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	corrupt := randomContent(t, 100)
	corruptHash := hashOf(corrupt)
	f.addRow(corruptHash, canonical(corruptHash), len(corrupt))
	f.writeLoose(canonical(corruptHash), []byte("bytes that do not match the hash"))
	good := randomContent(t, 100)
	goodHash := f.addBlob(good, canonical)

	stats := f.run(packer.Options{})

	assert.Equal(1, stats.BlobsCorrupt)
	assert.Equal(1, stats.BlobsPacked, "run continues past the corrupt blob")
	entry, err := f.store.GetAttachmentPackEntry(corruptHash)
	require.NoError(err)
	assert.Nil(entry, "corrupt blob is not indexed")
	_, err = os.Stat(filepath.Join(f.dir, filepath.FromSlash(canonical(corruptHash))))
	require.NoError(err, "corrupt file is left in place")
	entry, err = f.store.GetAttachmentPackEntry(goodHash)
	require.NoError(err)
	assert.NotNil(entry)
}

func TestRunHonorsContextCancellation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	c := randomContent(t, 100)
	h := f.addBlob(c, canonical)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := packer.Run(ctx, f.store, f.dir, packer.Options{})
	require.ErrorIs(err, context.Canceled)

	entry, err := f.store.GetAttachmentPackEntry(h)
	require.NoError(err)
	assert.Nil(entry, "nothing indexed after cancellation")
}
