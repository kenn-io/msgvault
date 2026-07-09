package packer_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/blobstore"
	"go.kenn.io/msgvault/internal/packer"
	"go.kenn.io/msgvault/internal/store"
)

// packIndexRowCount returns the total number of attachment_pack_index rows.
func (f *fixture) packIndexRowCount() int64 {
	f.t.Helper()
	var n int64
	err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM attachment_pack_index`).Scan(&n)
	require.NoError(f.t, err, "count pack index rows")
	return n
}

// packFiles returns the paths of all *.mvpack files under the packs dir.
func (f *fixture) packFiles() []string {
	f.t.Helper()
	var files []string
	err := filepath.WalkDir(filepath.Join(f.dir, "packs"),
		func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(d.Name(), blobstore.PackExt) {
				files = append(files, path)
			}
			return nil
		})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	require.NoError(f.t, err, "walk packs dir")
	return files
}

func TestUnpackRoundTrip(t *testing.T) {
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

	run := f.run(packer.Options{TargetSize: 1024})
	require.Equal(3, run.PacksSealed)
	require.Empty(f.looseFiles(), "all loose files packed away")

	stats, err := packer.Unpack(context.Background(), f.store, f.dir)
	require.NoError(err, "packer.Unpack")

	assert.Equal(3, stats.PacksUnpacked)
	assert.Equal(5, stats.BlobsRestored)
	assert.Equal(int64(5*600), stats.BytesRestored)

	for i, h := range hashes {
		data, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(canonical(h))))
		require.NoErrorf(err, "canonical loose file for %s", h)
		assert.Equalf(contents[i], data, "blob %s restored byte-identical", h)
		entry, err := f.store.GetAttachmentPackEntry(h)
		require.NoErrorf(err, "GetAttachmentPackEntry(%s)", h)
		assert.Nilf(entry, "blob %s no longer indexed", h)
	}

	recs, err := f.store.ListPackRecords()
	require.NoError(err)
	assert.Empty(recs, "attachment_packs empty")
	assert.Zero(f.packIndexRowCount(), "attachment_pack_index empty")
	assert.Empty(f.packFiles(), "no .mvpack files remain")
	assert.Equal(contents[0], f.readBack(hashes[0]),
		"production read path serves the restored loose file")
}

func TestUnpackRestoresEmptyBlob(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	buildOrphanPack(t, f.dir, []byte{})
	adopted := f.run(packer.Options{})
	require.Equal(1, adopted.PacksAdopted, "orphan pack with empty blob adopted")

	stats, err := packer.Unpack(context.Background(), f.store, f.dir)
	require.NoError(err, "packer.Unpack")

	assert.Equal(1, stats.PacksUnpacked)
	assert.Equal(1, stats.BlobsRestored)
	assert.Zero(stats.BytesRestored)
	emptyHash := hashOf(nil)
	data, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(canonical(emptyHash))))
	require.NoError(err, "canonical loose file for empty blob")
	assert.Empty(data)
	assert.Zero(f.packIndexRowCount())
}

func TestUnpackDropsMissingPackWithNoLiveRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	rec := store.PackRecord{
		PackID:    "01hzy3v7q8r9s0t1a2b3c4d5e6",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(f.store.RecordPackedBlobs(rec, nil), "record empty pack")

	stats, err := packer.Unpack(context.Background(), f.store, f.dir)
	require.NoError(err, "missing pack file with no live rows is not an error")

	assert.Equal(packer.UnpackStats{}, stats, "dropped record is not counted as unpacked")
	has, err := f.store.HasPackRecord(rec.PackID)
	require.NoError(err)
	assert.False(has, "stale record dropped")
}

func TestUnpackSkipsZeroLiveCorruptPackAfterOrphanRescue(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	content := randomContent(t, 600)
	hash := f.addBlob(content, canonical)
	packed := f.run(packer.Options{})
	require.Equal(1, packed.PacksSealed)

	stale, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(stale)
	oldPackPath := filepath.Join(f.dir, "packs", stale.PackID[:2], stale.PackID+blobstore.PackExt)
	pf, err := os.OpenFile(oldPackPath, os.O_RDWR, 0)
	require.NoError(err)
	buf := make([]byte, 1)
	_, err = pf.ReadAt(buf, stale.Offset)
	require.NoError(err)
	buf[0] ^= 0xff
	_, err = pf.WriteAt(buf, stale.Offset)
	require.NoError(err)
	require.NoError(pf.Close())

	orphanID := buildOrphanPack(t, f.dir, content)
	rescued := f.run(packer.Options{})
	require.Equal(1, rescued.PacksAdopted)
	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(entry)
	require.Equal(orphanID, entry.PackID)
	oldLive, err := f.store.CountPackIndexEntries(stale.PackID)
	require.NoError(err)
	require.Zero(oldLive, "corrupt source pack is now entirely dead")

	stats, err := packer.Unpack(context.Background(), f.store, f.dir)
	require.NoError(err, "zero-live corrupt pack must be dropped without opening it")

	assert.Equal(1, stats.PacksUnpacked, "only the live replacement pack restores blobs")
	assert.Equal(1, stats.BlobsRestored)
	assert.Equal(content, f.readBack(hash))
	recs, err := f.store.ListPackRecords()
	require.NoError(err)
	assert.Empty(recs)
	assert.Empty(f.packFiles())
}

func TestUnpackRestoresOnlyLivePackEntries(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	deadContent := randomContent(t, 600)
	deadHash := f.addBlob(deadContent, canonical)
	liveContent := randomContent(t, 600)
	liveHash := f.addBlob(liveContent, canonical)
	packed := f.run(packer.Options{})
	require.Equal(1, packed.PacksSealed)

	deadEntry, err := f.store.GetAttachmentPackEntry(deadHash)
	require.NoError(err)
	require.NotNil(deadEntry)
	packPath := filepath.Join(f.dir, "packs", deadEntry.PackID[:2], deadEntry.PackID+blobstore.PackExt)
	pf, err := os.OpenFile(packPath, os.O_RDWR, 0)
	require.NoError(err)
	buf := make([]byte, 1)
	_, err = pf.ReadAt(buf, deadEntry.Offset)
	require.NoError(err)
	buf[0] ^= 0xff
	_, err = pf.WriteAt(buf, deadEntry.Offset)
	require.NoError(err)
	require.NoError(pf.Close())

	_, err = f.store.DB().Exec(f.store.Rebind(
		`DELETE FROM attachment_pack_index WHERE blob_hash = ?`), deadHash)
	require.NoError(err, "mark corrupt footer entry dead")

	stats, err := packer.Unpack(context.Background(), f.store, f.dir)
	require.NoError(err, "dead corrupt footer entry must not block live restore")

	assert.Equal(1, stats.PacksUnpacked)
	assert.Equal(1, stats.BlobsRestored)
	assert.Equal(int64(len(liveContent)), stats.BytesRestored)
	live, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(canonical(liveHash))))
	require.NoError(err)
	assert.Equal(liveContent, live)
	_, err = os.Stat(filepath.Join(f.dir, filepath.FromSlash(canonical(deadHash))))
	assert.ErrorIs(err, fs.ErrNotExist, "dead blob must not be resurrected as loose content")
}

func TestUnpackFailsOnMalformedPackID(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)

	rec := store.PackRecord{
		PackID:    "not-a-ulid",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	// Seed corrupt metadata directly: RecordPackedBlobs rejects malformed IDs
	// at the write boundary, while Unpack must still fail safely on a damaged or
	// externally restored database that already contains one.
	_, err := f.store.DB().Exec(f.store.Rebind(`
		INSERT INTO attachment_packs (pack_id, entry_count, stored_bytes, created_at)
		VALUES (?, 0, 0, ?)`), rec.PackID, rec.CreatedAt.Format(time.RFC3339))
	require.NoError(err, "seed pack with bad id")

	_, err = packer.Unpack(context.Background(), f.store, f.dir)
	require.Error(err, "malformed pack id must not be sliced into a path")
	require.Contains(err.Error(), rec.PackID)
}

func TestUnpackFailsOnMissingPackWithLiveRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	f.addBlob(randomContent(t, 600), canonical)
	f.run(packer.Options{})
	recs, err := f.store.ListPackRecords()
	require.NoError(err)
	require.Len(recs, 1)
	id := recs[0].PackID
	require.NoError(os.Remove(filepath.Join(f.dir, "packs", id[:2], id+blobstore.PackExt)),
		"delete pack file out from under the record")

	_, err = packer.Unpack(context.Background(), f.store, f.dir)
	require.Error(err, "live blobs are unreachable")
	assert.Contains(err.Error(), id, "error names the pack")

	has, err := f.store.HasPackRecord(id)
	require.NoError(err)
	assert.True(has, "record retained")
	n, err := f.store.CountPackIndexEntries(id)
	require.NoError(err)
	assert.Equal(int64(1), n, "index rows retained")
}

// TestUnpackHonorsContextCancellation pins that a cancelled ctx aborts before
// ANY state change even when packs with restorable blobs exist: no DB rows
// dropped, no pack files deleted, no loose files written. The per-pack flow
// re-checks ctx between blob restores and again before the record delete, so
// a mid-run cancellation likewise never drops rows for a pack file that still
// exists.
func TestUnpackHonorsContextCancellation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	f.addBlob(randomContent(t, 100), canonical)
	f.addBlob(randomContent(t, 100), canonical)
	run := f.run(packer.Options{TargetSize: 100}) // two packs, one blob each
	require.Equal(2, run.PacksSealed)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := packer.Unpack(ctx, f.store, f.dir)
	require.ErrorIs(err, context.Canceled)

	recs, err := f.store.ListPackRecords()
	require.NoError(err)
	assert.Len(recs, 2, "pack records retained after cancellation")
	assert.Equal(int64(2), f.packIndexRowCount(), "index rows retained after cancellation")
	assert.Len(f.packFiles(), 2, "pack files retained after cancellation")
	assert.Empty(f.looseFiles(), "no loose files restored after cancellation")
}
