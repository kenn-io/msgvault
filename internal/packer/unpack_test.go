package packer_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/blobstore"
	"go.kenn.io/msgvault/internal/packer"
	"go.kenn.io/msgvault/internal/store"
)

const syntheticUnpackPackID = "01hzy3v7q8r9s0t1a2b3c4d5e6"

func recordSyntheticUnpackPack(
	t *testing.T,
	f *fixture,
	entries []pack.Entry,
	writeData map[string][]byte,
) string {
	t.Helper()
	require := require.New(t)
	path := filepath.Join(f.dir, "packs", syntheticUnpackPackID[:2],
		syntheticUnpackPackID+blobstore.PackExt)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	require.NoError(err)
	require.NoError(func() error {
		_, err := file.WriteAt([]byte{'M', 'V', 'P', 'K', 1, 0}, 0)
		return err
	}())

	footerStart := uint64(6)
	for _, entry := range entries {
		footerStart = max(footerStart, entry.Offset+entry.StoredLen)
		if data := writeData[entry.ID.String()]; data != nil {
			_, err = file.WriteAt(data, int64(entry.Offset))
			require.NoError(err)
		}
	}
	footer := make([]byte, 4+61*len(entries))
	binary.LittleEndian.PutUint32(footer, uint32(len(entries)))
	for i, entry := range entries {
		offset := 4 + i*61
		copy(footer[offset:offset+32], entry.ID[:])
		binary.LittleEndian.PutUint64(footer[offset+32:], entry.Offset)
		binary.LittleEndian.PutUint64(footer[offset+40:], entry.StoredLen)
		binary.LittleEndian.PutUint64(footer[offset+48:], entry.RawLen)
		footer[offset+56] = byte(entry.Flags)
		binary.LittleEndian.PutUint32(footer[offset+57:], entry.CRC32C)
	}
	footerLen := uint32(len(footer))
	var footerLenBytes [4]byte
	binary.LittleEndian.PutUint32(footerLenBytes[:], footerLen)
	digest := sha256.New()
	_, err = digest.Write(footer)
	require.NoError(err)
	_, err = digest.Write(footerLenBytes[:])
	require.NoError(err)
	trailer := make([]byte, 40)
	copy(trailer[:4], footerLenBytes[:])
	copy(trailer[4:36], digest.Sum(nil))
	copy(trailer[36:], "KPVM")
	_, err = file.WriteAt(footer, int64(footerStart))
	require.NoError(err)
	_, err = file.WriteAt(trailer, int64(footerStart)+int64(len(footer)))
	require.NoError(err)
	require.NoError(file.Close())

	indexed := make([]store.PackIndexEntry, 0, len(entries))
	var storedBytes int64
	for _, entry := range entries {
		indexed = append(indexed, store.PackIndexEntry{
			BlobHash: entry.ID.String(), PackID: syntheticUnpackPackID,
			Offset: int64(entry.Offset), StoredLen: int64(entry.StoredLen),
			RawLen: int64(entry.RawLen), Flags: uint8(entry.Flags), CRC32C: entry.CRC32C,
		})
		storedBytes += int64(entry.StoredLen)
	}
	require.NoError(f.store.RecordPackedBlobs(store.PackRecord{
		PackID: syntheticUnpackPackID, EntryCount: int64(len(entries)),
		StoredBytes: storedBytes, CreatedAt: time.Now().UTC(),
	}, indexed))
	return path
}

func syntheticUnpackEntry(t *testing.T, content []byte, offset, rawLen, storedLen uint64) pack.Entry {
	t.Helper()
	id, err := pack.ParseBlobID(hashOf(content))
	require.NoError(t, err)
	return pack.Entry{
		ID: id, Offset: offset, RawLen: rawLen, StoredLen: storedLen,
		CRC32C: crc32.Checksum(content, crc32.MakeTable(crc32.Castagnoli)),
	}
}

func assertUnpackPackPreserved(t *testing.T, f *fixture, path, packID string, wantEntries int64) {
	t.Helper()
	assert := assert.New(t)
	assert.FileExists(path)
	has, err := f.store.HasPackRecord(packID)
	require.NoError(t, err)
	assert.True(has)
	count, err := f.store.CountPackIndexEntries(packID)
	require.NoError(t, err)
	assert.Equal(wantEntries, count)
}

func recordExistingUnpackPack(t *testing.T, f *fixture, packID string) string {
	t.Helper()
	path := filepath.Join(f.dir, "packs", packID[:2], packID+blobstore.PackExt)
	r, err := blobstore.OpenMaintenancePack(path)
	require.NoError(t, err)
	footer := r.Entries()
	require.NoError(t, r.Close())
	entries := make([]store.PackIndexEntry, 0, len(footer))
	var storedBytes int64
	for _, entry := range footer {
		entries = append(entries, store.PackIndexEntry{
			BlobHash: entry.ID.String(), PackID: packID,
			Offset: int64(entry.Offset), StoredLen: int64(entry.StoredLen),
			RawLen: int64(entry.RawLen), Flags: uint8(entry.Flags), CRC32C: entry.CRC32C,
		})
		storedBytes += int64(entry.StoredLen)
	}
	require.NoError(t, f.store.RecordPackedBlobs(store.PackRecord{
		PackID: packID, EntryCount: int64(len(entries)), StoredBytes: storedBytes,
		CreatedAt: time.Now().UTC(),
	}, entries))
	return path
}

func rewriteUnpackPreflightFixture(t *testing.T, path string, dimension blobstore.LimitDimension) {
	t.Helper()
	require := require.New(t)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_TRUNC, 0o600)
	require.NoError(err)
	defer func() { require.NoError(file.Close()) }()
	require.NoError(func() error {
		_, err := file.WriteAt([]byte{'M', 'V', 'P', 'K', 1, 0}, 0)
		return err
	}())
	switch dimension {
	case blobstore.LimitPackContainerBytes:
		require.NoError(file.Truncate(blobstore.MaxMaintenancePackBytes + 1))
	case blobstore.LimitPackFooterBytes:
		trailer := make([]byte, 40)
		binary.LittleEndian.PutUint32(trailer[:4], blobstore.MaxMaintenanceFooterBytes+1)
		copy(trailer[36:], "KPVM")
		_, err = file.WriteAt(trailer, 6)
		require.NoError(err)
	case blobstore.LimitPackEntryCount:
		count := uint32(blobstore.MaxMaintenancePackEntries + 1)
		footerLen := uint32(4) + count*61
		footerStart := int64(6)
		var countBytes [4]byte
		binary.LittleEndian.PutUint32(countBytes[:], count)
		_, err = file.WriteAt(countBytes[:], footerStart)
		require.NoError(err)
		trailer := make([]byte, 40)
		binary.LittleEndian.PutUint32(trailer[:4], footerLen)
		copy(trailer[36:], "KPVM")
		_, err = file.WriteAt(trailer, footerStart+int64(footerLen))
		require.NoError(err)
	default:
		require.FailNow("unsupported preflight fixture dimension", string(dimension))
	}
}

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

func TestMixedNonlocalAliasRemainsReadableAfterPackAuthorityRemoval(t *testing.T) {
	for _, tc := range []struct {
		name            string
		removeAuthority func(*testing.T, *fixture, string, []byte)
	}{
		{
			name: "unpack",
			removeAuthority: func(t *testing.T, f *fixture, _ string, _ []byte) {
				t.Helper()
				_, err := packer.Unpack(context.Background(), f.store, f.dir)
				require.NoError(t, err)
			},
		},
		{
			name: "restored metadata clear",
			removeAuthority: func(t *testing.T, f *fixture, hash string, content []byte) {
				t.Helper()
				f.writeLoose(canonical(hash), content)
				require.NoError(t, f.store.ClearAttachmentPackMetadata())
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newFixture(t)
			content := randomContent(t, 600)
			hash := hashOf(content)
			uppercase := strings.ToUpper(hash)
			f.addRow(hash, "HTTPS://cdn.example.com/attachment", len(content))
			f.addRow(uppercase, "legacy/"+uppercase, len(content))
			f.writeLoose("legacy/"+uppercase, content)

			packed := f.run(packer.Options{})
			require.Equal(1, packed.BlobsPacked)
			tc.removeAuthority(t, f, hash, content)

			var localHash string
			require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
				SELECT content_hash FROM attachments
				WHERE message_id = ?
				  AND storage_path IS NOT NULL AND storage_path != ''
				  AND LOWER(storage_path) NOT LIKE 'http://%'
				  AND LOWER(storage_path) NOT LIKE 'https://%'`), f.msgID).Scan(&localHash))
			assert.Equal(hash, localHash, "local DB hash must address the canonical loose path")
			assert.Equal(content, f.readBack(localHash))
		})
	}
}

func TestUnpackRestoresEmptyBlob(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	emptyHash := hashOf(nil)
	f.addRow(emptyHash, canonical(emptyHash), 0)
	buildOrphanPack(t, f.dir, []byte{})
	adopted := f.run(packer.Options{})
	require.Equal(1, adopted.PacksAdopted, "orphan pack with empty blob adopted")

	stats, err := packer.Unpack(context.Background(), f.store, f.dir)
	require.NoError(err, "packer.Unpack")

	assert.Equal(1, stats.PacksUnpacked)
	assert.Equal(1, stats.BlobsRestored)
	assert.Zero(stats.BytesRestored)
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

func TestUnpackPlansEveryLiveEntryBeforeWriting(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	firstContent := []byte("eligible first blob")
	first := syntheticUnpackEntry(t, firstContent, 6,
		uint64(len(firstContent)), uint64(len(firstContent)))
	oversizedContent := []byte{0x7f}
	oversized := syntheticUnpackEntry(t, oversizedContent,
		first.Offset+first.StoredLen,
		uint64(blobstore.MaxMaintenanceBlobBytes+1), 1)
	f.addRow(first.ID.String(), canonical(first.ID.String()), len(firstContent))
	f.addRow(oversized.ID.String(), canonical(oversized.ID.String()), len(oversizedContent))
	packPath := recordSyntheticUnpackPack(t, f, []pack.Entry{first, oversized}, map[string][]byte{
		first.ID.String():     firstContent,
		oversized.ID.String(): oversizedContent,
	})

	stats, err := packer.Unpack(context.Background(), f.store, f.dir)
	require.Error(err)
	var limitErr *blobstore.LimitError
	require.ErrorAs(err, &limitErr)
	assert.Equal(blobstore.LimitBlobRawBytes, limitErr.Dimension)
	assert.Equal(uint64(blobstore.MaxMaintenanceBlobBytes+1), limitErr.Actual)
	assert.Equal(uint64(blobstore.MaxMaintenanceBlobBytes), limitErr.Limit)
	assert.Contains(err.Error(), oversized.ID.String())
	assert.Contains(err.Error(), syntheticUnpackPackID)
	assert.Equal(packer.UnpackStats{}, stats)
	assert.NoFileExists(filepath.Join(f.dir, filepath.FromSlash(canonical(first.ID.String()))),
		"planning a later oversized entry must precede the first loose write")
	assert.NoFileExists(filepath.Join(f.dir, filepath.FromSlash(canonical(oversized.ID.String()))))
	assertUnpackPackPreserved(t, f, packPath, syntheticUnpackPackID, 2)
}

func TestUnpackRejectsAuthoritativeOversizedStoredLengthBeforeWriting(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	content := []byte{0x42}
	entry := syntheticUnpackEntry(t, content, 6, 1,
		uint64(blobstore.MaxMaintenanceBlobBytes+1))
	f.addRow(entry.ID.String(), canonical(entry.ID.String()), len(content))
	packPath := recordSyntheticUnpackPack(t, f, []pack.Entry{entry}, map[string][]byte{
		entry.ID.String(): content,
	})

	stats, err := packer.Unpack(context.Background(), f.store, f.dir)
	require.Error(err)
	var limitErr *blobstore.LimitError
	require.ErrorAs(err, &limitErr)
	assert.Equal(blobstore.LimitBlobStoredBytes, limitErr.Dimension)
	assert.Equal(uint64(blobstore.MaxMaintenanceBlobBytes+1), limitErr.Actual)
	assert.Equal(packer.UnpackStats{}, stats)
	assert.NoFileExists(filepath.Join(f.dir, filepath.FromSlash(canonical(entry.ID.String()))))
	assertUnpackPackPreserved(t, f, packPath, syntheticUnpackPackID, 1)
}

func TestUnpackDropsZeroLiveOversizedPackWithoutOpeningIt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)
	rec := store.PackRecord{
		PackID:    syntheticUnpackPackID,
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(f.store.RecordPackedBlobs(rec, nil))
	path := filepath.Join(f.dir, "packs", rec.PackID[:2], rec.PackID+blobstore.PackExt)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	file, err := os.Create(path)
	require.NoError(err)
	require.NoError(file.Truncate(blobstore.MaxMaintenancePackBytes + 1))
	require.NoError(file.Close())

	stats, err := packer.Unpack(context.Background(), f.store, f.dir)
	require.NoError(err, "zero-live packs must not enter bounded preflight")
	assert.Equal(packer.UnpackStats{}, stats)
	assert.NoFileExists(path)
	has, err := f.store.HasPackRecord(rec.PackID)
	require.NoError(err)
	assert.False(has)
}

func TestUnpackPreflightFailuresPreserveLivePackAuthority(t *testing.T) {
	for _, dimension := range []blobstore.LimitDimension{
		blobstore.LimitPackContainerBytes,
		blobstore.LimitPackFooterBytes,
		blobstore.LimitPackEntryCount,
	} {
		t.Run(string(dimension), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newFixture(t)
			content := []byte("live preflight blob")
			entry := syntheticUnpackEntry(t, content, 6,
				uint64(len(content)), uint64(len(content)))
			f.addRow(entry.ID.String(), canonical(entry.ID.String()), len(content))
			path := recordSyntheticUnpackPack(t, f, []pack.Entry{entry}, map[string][]byte{
				entry.ID.String(): content,
			})
			rewriteUnpackPreflightFixture(t, path, dimension)

			stats, err := packer.Unpack(context.Background(), f.store, f.dir)
			require.Error(err)
			var limitErr *blobstore.LimitError
			require.ErrorAs(err, &limitErr)
			assert.Equal(dimension, limitErr.Dimension)
			assert.Equal(packer.UnpackStats{}, stats)
			assert.NoFileExists(filepath.Join(f.dir, filepath.FromSlash(canonical(entry.ID.String()))))
			assertUnpackPackPreserved(t, f, path, syntheticUnpackPackID, 1)
		})
	}
}

func TestUnpackRejectsFooterDatabaseMismatchBeforeWriting(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)
	content := randomContent(t, 600)
	hash := f.addBlob(content, canonical)
	packed := f.run(packer.Options{})
	require.Equal(1, packed.PacksSealed)
	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(entry)
	path := filepath.Join(f.dir, "packs", entry.PackID[:2], entry.PackID+blobstore.PackExt)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_pack_index SET raw_len = raw_len + 1 WHERE blob_hash = ?`), hash)
	require.NoError(err)

	stats, err := packer.Unpack(context.Background(), f.store, f.dir)
	require.Error(err)
	require.ErrorIs(err, pack.ErrCorrupt)
	assert.Contains(err.Error(), hash)
	assert.Contains(err.Error(), entry.PackID)
	assert.Equal(packer.UnpackStats{}, stats)
	assert.NoFileExists(filepath.Join(f.dir, filepath.FromSlash(canonical(hash))))
	assertUnpackPackPreserved(t, f, path, entry.PackID, 1)
}

func TestUnpackClassifiesMissingFooterEntryAsCorrupt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)
	footerContent := []byte("footer-only identity")
	footerEntry := syntheticUnpackEntry(t, footerContent, 6,
		uint64(len(footerContent)), uint64(len(footerContent)))
	path := recordSyntheticUnpackPack(t, f, []pack.Entry{footerEntry}, map[string][]byte{
		footerEntry.ID.String(): footerContent,
	})
	liveContent := []byte("live database identity")
	liveHash := hashOf(liveContent)
	f.addRow(liveHash, canonical(liveHash), len(liveContent))
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_pack_index SET blob_hash = ? WHERE blob_hash = ?`),
		liveHash, footerEntry.ID.String())
	require.NoError(err)

	stats, err := packer.Unpack(context.Background(), f.store, f.dir)
	require.Error(err)
	require.ErrorIs(err, pack.ErrCorrupt)
	assert.Contains(err.Error(), liveHash)
	assert.Contains(err.Error(), syntheticUnpackPackID)
	assert.Equal(packer.UnpackStats{}, stats)
	assert.NoFileExists(filepath.Join(f.dir, filepath.FromSlash(canonical(liveHash))))
	assertUnpackPackPreserved(t, f, path, syntheticUnpackPackID, 1)
}

func TestUnpackAcceptsBlobAtExactMaintenanceLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("exact 64 MiB boundary test")
	}
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)
	content := make([]byte, blobstore.MaxMaintenanceBlobBytes)
	hash := hashOf(content)
	f.addRow(hash, canonical(hash), len(content))
	packID := buildOrphanPack(t, f.dir, content)
	packPath := recordExistingUnpackPack(t, f, packID)

	stats, err := packer.Unpack(context.Background(), f.store, f.dir)
	require.NoError(err)
	assert.Equal(1, stats.PacksUnpacked)
	assert.Equal(1, stats.BlobsRestored)
	assert.Equal(int64(blobstore.MaxMaintenanceBlobBytes), stats.BytesRestored)
	assert.NoFileExists(packPath)
	loosePath := filepath.Join(f.dir, filepath.FromSlash(canonical(hash)))
	file, err := os.Open(loosePath)
	require.NoError(err)
	digest := sha256.New()
	bytesRead, err := io.Copy(digest, file)
	require.NoError(err)
	require.NoError(file.Close())
	assert.Equal(int64(blobstore.MaxMaintenanceBlobBytes), bytesRead)
	assert.Equal(hash, hex.EncodeToString(digest.Sum(nil)))
}

func TestUnpackCorruptLaterBlobRetainsPackAuthority(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)
	contents := map[string][]byte{}
	for range 2 {
		content := randomContent(t, 600)
		hash := f.addBlob(content, canonical)
		contents[hash] = content
	}
	packed := f.run(packer.Options{})
	require.Equal(1, packed.PacksSealed)
	recs, err := f.store.ListPackRecords()
	require.NoError(err)
	require.Len(recs, 1)
	entries, err := f.store.ListAttachmentPackEntries(recs[0].PackID)
	require.NoError(err)
	require.Len(entries, 2)
	path := filepath.Join(f.dir, "packs", recs[0].PackID[:2], recs[0].PackID+blobstore.PackExt)
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(err)
	var damaged [1]byte
	_, err = file.ReadAt(damaged[:], entries[1].Offset)
	require.NoError(err)
	damaged[0] ^= 0xff
	_, err = file.WriteAt(damaged[:], entries[1].Offset)
	require.NoError(err)
	require.NoError(file.Close())

	stats, err := packer.Unpack(context.Background(), f.store, f.dir)
	require.Error(err)
	require.ErrorIs(err, pack.ErrCorrupt)
	assert.Equal(packer.UnpackStats{}, stats)
	firstPath := filepath.Join(f.dir, filepath.FromSlash(canonical(entries[0].BlobHash)))
	firstData, readErr := os.ReadFile(firstPath)
	require.NoError(readErr, "a prior verified loose copy may remain harmlessly")
	assert.Equal(contents[entries[0].BlobHash], firstData)
	assert.NoFileExists(filepath.Join(f.dir, filepath.FromSlash(canonical(entries[1].BlobHash))))
	assertUnpackPackPreserved(t, f, path, recs[0].PackID, 2)
}

func TestUnpackPrunesStaleMappings(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	content := randomContent(t, 600)
	hash := f.addBlob(content, canonical)
	packed := f.run(packer.Options{})
	require.Equal(1, packed.PacksSealed)
	require.NoError(f.store.ReplaceMessageInlineAttachments(f.msgID, nil))
	// UpsertAttachment is not Teams-managed, so remove the final liveness row
	// directly to model a generic cascade while retaining its stale mapping.
	_, err := f.store.DB().Exec(f.store.Rebind(
		`DELETE FROM attachments WHERE message_id = ? AND content_hash = ?`), f.msgID, hash)
	require.NoError(err)

	stats, err := packer.Unpack(context.Background(), f.store, f.dir)
	require.NoError(err)

	assert.Equal(1, stats.MappingsPruned)
	assert.Zero(stats.BlobsRestored, "unreferenced stale mapping must not be restored")
	assert.NoFileExists(filepath.Join(f.dir, filepath.FromSlash(canonical(hash))))
	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	assert.Nil(entry)
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
