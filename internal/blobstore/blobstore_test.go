package blobstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/store"
)

// mapIndex is a PackIndex over a plain map. Pack entries imply a live
// reference unless explicitly overridden; loose references are listed in
// referenced.
type mapIndex struct {
	m            map[string]*store.PackIndexEntry
	referenced   map[string]bool
	unreferenced map[string]bool
}

func (i *mapIndex) ResolveAttachmentBlob(h string) (store.AttachmentBlobLocation, error) {
	entry := i.m[h]
	referenced := !i.unreferenced[h] && (entry != nil || i.referenced[h])
	return store.AttachmentBlobLocation{Referenced: referenced, Pack: entry}, nil
}

// buildPack seals content blobs into a pack under attachmentsDir/packs/ and
// returns index entries keyed by blob hash.
func buildPack(t *testing.T, attachmentsDir string, blobs ...[]byte) map[string]*store.PackIndexEntry {
	t.Helper()
	staging := t.TempDir()
	w, err := pack.NewWriter(staging, pack.WriterOptions{})
	require.NoError(t, err)

	for _, b := range blobs {
		_, err := w.Append(b)
		require.NoError(t, err)
	}
	id := w.ID()
	final := filepath.Join(attachmentsDir, "packs", id[:2], id+PackExt)
	require.NoError(t, os.MkdirAll(filepath.Dir(final), 0o700))
	entries, err := w.Seal(final)
	require.NoError(t, err)

	out := make(map[string]*store.PackIndexEntry, len(entries))
	for _, e := range entries {
		out[e.ID.String()] = &store.PackIndexEntry{
			BlobHash:  e.ID.String(),
			PackID:    id,
			Offset:    int64(e.Offset),
			StoredLen: int64(e.StoredLen),
			RawLen:    int64(e.RawLen),
			Flags:     uint8(e.Flags),
			CRC32C:    e.CRC32C,
		}
	}
	return out
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func readAll(t *testing.T, s *Store, hash string) []byte {
	t.Helper()
	r, size, err := s.Open(hash)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), size)
	return data
}

func readBounded(t *testing.T, s *Store, hash string, maxBytes int64) []byte {
	t.Helper()
	data, size, err := s.ReadBounded(hash, maxBytes)
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), size)
	return data
}

// buildSyntheticEntryHeavyPack writes a valid plain v1 pack whose footer has
// more entries than maintenance reads permit. Zero-length duplicate entries
// keep the fixture small while still exercising kit's real footer parser.
func buildSyntheticEntryHeavyPack(t *testing.T, attachmentsDir string) (string, string, *store.PackIndexEntry) {
	t.Helper()
	packID := pack.NewPackID()
	hash := hashOf(nil)
	blobID, err := pack.ParseBlobID(hash)
	require.NoError(t, err)

	count := MaxMaintenancePackEntries + 1
	footer := make([]byte, 4+count*61)
	binary.LittleEndian.PutUint32(footer[:4], uint32(count))
	for i := range count {
		off := 4 + i*61
		copy(footer[off:off+32], blobID[:])
		binary.LittleEndian.PutUint64(footer[off+32:], 6)
		// The remaining entry fields are zero: empty stored/raw lengths,
		// plain flags, and the CRC32C of an empty byte slice.
	}
	footerLen := make([]byte, 4)
	binary.LittleEndian.PutUint32(footerLen, uint32(len(footer)))
	sum := sha256.Sum256(append(append([]byte(nil), footer...), footerLen...))
	contents := make([]byte, 0, 6+len(footer)+40)
	contents = append(contents, []byte("MVPK\x01\x00")...)
	contents = append(contents, footer...)
	contents = append(contents, footerLen...)
	contents = append(contents, sum[:]...)
	contents = append(contents, []byte("KPVM")...)

	path := filepath.Join(attachmentsDir, "packs", packID[:2], packID+PackExt)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	return path, hash, &store.PackIndexEntry{BlobHash: hash, PackID: packID, Offset: 6}
}

func rewritePlainPackFooter(t *testing.T, path string, mutate func([]byte)) {
	t.Helper()
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(contents), 46)
	trailer := contents[len(contents)-40:]
	footerLen := int(binary.LittleEndian.Uint32(trailer[:4]))
	footerStart := len(contents) - 40 - footerLen
	require.GreaterOrEqual(t, footerStart, 6)
	footer := contents[footerStart : len(contents)-40]
	mutate(footer)
	sum := sha256.Sum256(contents[footerStart : len(contents)-36])
	copy(trailer[4:36], sum[:])
	require.NoError(t, os.WriteFile(path, contents, 0o600))
}

func TestReadBoundedConstants(t *testing.T) {
	assert := assert.New(t)
	assert.Equal(64<<20, MaxMaintenanceBlobBytes)
	assert.Equal(100_000, MaxMaintenancePackEntries)
	assert.Equal(8<<20, MaxMaintenanceFooterBytes)
	assert.Equal(128<<20, MaxMaintenancePackBytes)
}

func TestReadBoundedAcceptsExactLimit(t *testing.T) {
	for _, storage := range []string{"packed", "loose"} {
		t.Run(storage, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			dir := t.TempDir()
			content := []byte("exact bounded attachment bytes")
			hash := hashOf(content)
			idx := map[string]*store.PackIndexEntry{}
			referenced := map[string]bool{}
			if storage == "packed" {
				idx = buildPack(t, dir, content)
			} else {
				referenced[hash] = true
				require.NoError(os.MkdirAll(filepath.Join(dir, hash[:2]), 0o700))
				require.NoError(os.WriteFile(filepath.Join(dir, hash[:2], hash), content, 0o600))
			}
			s := New(&mapIndex{m: idx, referenced: referenced}, dir)
			defer func() { require.NoError(s.Close()) }()

			got := readBounded(t, s, hash, int64(len(content)))
			assert.Equal(content, got)
			if storage == "loose" {
				assert.Equal(len(got), cap(got), "loose reads allocate exactly the stat-reported size")
			}
			_, _, err := s.ReadBounded(hash, int64(len(content)-1))
			assert.ErrorIs(err, ErrBlobTooLarge)
		})
	}
}

func TestReadBoundedLooseGrowthProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the deterministic zero-size device fixture is Unix-only")
	}
	dir := t.TempDir()
	hash := hashOf([]byte("device-backed loose attachment"))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, hash[:2]), 0o700))
	require.NoError(t, os.Symlink("/dev/zero", filepath.Join(dir, hash[:2], hash)))
	s := New(&mapIndex{m: map[string]*store.PackIndexEntry{}, referenced: map[string]bool{hash: true}}, dir)
	defer func() { require.NoError(t, s.Close()) }()

	_, _, err := s.ReadBounded(hash, MaxMaintenanceBlobBytes)
	assert.ErrorIs(t, err, ErrBlobTooLarge,
		"a separate byte probe must detect content beyond the stat-sized allocation")
}

func TestReadBoundedPreflightsPackLimits(t *testing.T) {
	t.Run("container", func(t *testing.T) {
		dir := t.TempDir()
		content := []byte("oversized sparse container")
		idx := buildPack(t, dir, content)
		hash := hashOf(content)
		entry := idx[hash]
		path := filepath.Join(dir, "packs", entry.PackID[:2], entry.PackID+PackExt)
		require.NoError(t, os.Truncate(path, MaxMaintenancePackBytes+1))
		s := New(&mapIndex{m: idx}, dir)
		defer func() { require.NoError(t, s.Close()) }()

		_, _, err := s.ReadBounded(hash, MaxMaintenanceBlobBytes)
		assert.ErrorIs(t, err, ErrBlobTooLarge,
			"the stat limit must win before OpenReader sees the now-invalid trailer")
	})

	t.Run("footer", func(t *testing.T) {
		require := require.New(t)
		dir := t.TempDir()
		content := []byte("oversized claimed footer")
		idx := buildPack(t, dir, content)
		hash := hashOf(content)
		entry := idx[hash]
		path := filepath.Join(dir, "packs", entry.PackID[:2], entry.PackID+PackExt)
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		require.NoError(err)
		st, err := f.Stat()
		require.NoError(err)
		var encoded [4]byte
		binary.LittleEndian.PutUint32(encoded[:], MaxMaintenanceFooterBytes+1)
		_, err = f.WriteAt(encoded[:], st.Size()-40)
		require.NoError(err)
		require.NoError(f.Close())
		s := New(&mapIndex{m: idx}, dir)
		defer func() { require.NoError(s.Close()) }()

		_, _, err = s.ReadBounded(hash, MaxMaintenanceBlobBytes)
		assert.ErrorIs(t, err, ErrBlobTooLarge,
			"the footer bound must win before OpenReader verifies the stale checksum")
	})

	t.Run("entry count", func(t *testing.T) {
		dir := t.TempDir()
		_, hash, entry := buildSyntheticEntryHeavyPack(t, dir)
		s := New(&mapIndex{m: map[string]*store.PackIndexEntry{hash: entry}}, dir)
		defer func() { require.NoError(t, s.Close()) }()

		_, _, err := s.ReadBounded(hash, MaxMaintenanceBlobBytes)
		assert.ErrorIs(t, err, ErrBlobTooLarge)
	})
}

func TestOpenMaintenancePackProvidesBoundedOpaqueReads(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	content := []byte("maintenance reader content")
	idx := buildPack(t, dir, content)
	hash := hashOf(content)
	entry := idx[hash]
	path := filepath.Join(dir, "packs", entry.PackID[:2], entry.PackID+PackExt)

	r, err := OpenMaintenancePack(path)
	require.NoError(err)
	entries := r.Entries()
	require.Len(entries, 1)
	assert.Equal(hash, entries[0].ID.String())

	got, err := r.ReadBlob(hash, int64(len(content)))
	require.NoError(err)
	assert.Equal(content, got)
	_, err = r.ReadBlob(hash, int64(len(content)-1))
	require.ErrorIs(err, ErrBlobTooLarge)
	require.NoError(r.Close())

	_, err = r.ReadBlob(hash, int64(len(content)))
	assert.Error(err, "reads after Close must fail through the retained descriptor")
}

func TestOpenMaintenancePackPreflightsContainerFooterAndCount(t *testing.T) {
	t.Run("container", func(t *testing.T) {
		dir := t.TempDir()
		content := []byte("oversized sparse maintenance container")
		idx := buildPack(t, dir, content)
		entry := idx[hashOf(content)]
		path := filepath.Join(dir, "packs", entry.PackID[:2], entry.PackID+PackExt)
		require.NoError(t, os.Truncate(path, MaxMaintenancePackBytes+1))

		_, err := OpenMaintenancePack(path)
		assert.ErrorIs(t, err, ErrBlobTooLarge)
	})

	t.Run("footer", func(t *testing.T) {
		require := require.New(t)
		dir := t.TempDir()
		content := []byte("oversized claimed maintenance footer")
		idx := buildPack(t, dir, content)
		entry := idx[hashOf(content)]
		path := filepath.Join(dir, "packs", entry.PackID[:2], entry.PackID+PackExt)
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		require.NoError(err)
		st, err := f.Stat()
		require.NoError(err)
		var encoded [4]byte
		binary.LittleEndian.PutUint32(encoded[:], MaxMaintenanceFooterBytes+1)
		_, err = f.WriteAt(encoded[:], st.Size()-40)
		require.NoError(err)
		require.NoError(f.Close())

		_, err = OpenMaintenancePack(path)
		assert.ErrorIs(t, err, ErrBlobTooLarge)
	})

	t.Run("entry count", func(t *testing.T) {
		dir := t.TempDir()
		path, _, _ := buildSyntheticEntryHeavyPack(t, dir)

		_, err := OpenMaintenancePack(path)
		assert.ErrorIs(t, err, ErrBlobTooLarge)
	})
}

func TestOpenMaintenancePackReportsTypedContainerLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	content := []byte("typed oversized maintenance container")
	idx := buildPack(t, dir, content)
	entry := idx[hashOf(content)]
	path := filepath.Join(dir, "packs", entry.PackID[:2], entry.PackID+PackExt)
	actual := int64(MaxMaintenancePackBytes + 1)
	require.NoError(os.Truncate(path, actual))

	_, err := OpenMaintenancePack(path)
	require.ErrorIs(err, ErrBlobTooLarge)
	var limitErr *LimitError
	require.ErrorAs(err, &limitErr)
	assert.Equal(LimitPackContainerBytes, limitErr.Dimension)
	assert.Equal(uint64(actual), limitErr.Actual)
	assert.Equal(uint64(MaxMaintenancePackBytes), limitErr.Limit)
}

func TestReadBoundedChecksCachedReaderEntryLimit(t *testing.T) {
	dir := t.TempDir()
	path, hash, entry := buildSyntheticEntryHeavyPack(t, dir)
	f, err := os.Open(path)
	require.NoError(t, err)
	entries := make(map[pack.BlobID]pack.Entry, MaxMaintenancePackEntries+1)
	for i := range MaxMaintenancePackEntries + 1 {
		var id pack.BlobID
		binary.LittleEndian.PutUint32(id[:4], uint32(i))
		entries[id] = pack.Entry{ID: id}
	}
	r := &boundedPackReader{file: f, entries: entries}
	s := New(&mapIndex{m: map[string]*store.PackIndexEntry{hash: entry}}, dir)
	s.boundedReaders[entry.PackID] = r
	s.order = append(s.order, entry.PackID)
	defer func() { require.NoError(t, s.Close()) }()

	_, _, err = s.ReadBounded(hash, MaxMaintenanceBlobBytes)
	assert.ErrorIs(t, err, ErrBlobTooLarge)
}

func TestBoundedPackVersionIsPinnedToStableV1(t *testing.T) {
	assert.Equal(t, plainPackVersion, byte(1))
}

func TestBoundedPackReaderKeepsPreflightedDescriptor(t *testing.T) {
	require := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("replacing an open file is not supported on Windows")
	}
	dir := t.TempDir()
	original := []byte("bytes from the preflighted descriptor")
	idx := buildPack(t, dir, original)
	hash := hashOf(original)
	entry := idx[hash]
	path := filepath.Join(dir, "packs", entry.PackID[:2], entry.PackID+PackExt)
	r, err := openBoundedPack(path)
	require.NoError(err)
	defer func() { require.NoError(r.Close()) }()

	replacementDir := t.TempDir()
	replacement := []byte("replacement path bytes must not be observed")
	replacementIdx := buildPack(t, replacementDir, replacement)
	replacementEntry := replacementIdx[hashOf(replacement)]
	replacementPath := filepath.Join(replacementDir, "packs", replacementEntry.PackID[:2], replacementEntry.PackID+PackExt)
	require.NoError(os.Rename(replacementPath, path))

	blobID, err := pack.ParseBlobID(hash)
	require.NoError(err)
	footerEntry, ok := r.entries[blobID]
	require.True(ok)
	got, err := r.readBlob(footerEntry, int64(len(original)))
	require.NoError(err)
	assert.Equal(t, original, got)
}

func TestBoundedReaderLifecycleClosesHeldDescriptor(t *testing.T) {
	for _, lifecycle := range []string{"close", "retire"} {
		t.Run(lifecycle, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			dir := t.TempDir()
			content := []byte("bounded descriptor lifecycle")
			idx := buildPack(t, dir, content)
			hash := hashOf(content)
			entry := idx[hash]
			s := New(&mapIndex{m: idx}, dir)
			assert.Equal(content, readBounded(t, s, hash, int64(len(content))))

			s.mu.Lock()
			cached := s.boundedReaders[entry.PackID]
			s.mu.Unlock()
			require.NotNil(cached)
			if lifecycle == "close" {
				require.NoError(s.Close())
			} else {
				require.NoError(s.RetirePack(entry.PackID))
				require.NoError(s.Close())
			}
			_, err := cached.file.ReadAt(make([]byte, 1), 0)
			assert.Error(err, "cache lifecycle must close the held pack descriptor")
		})
	}
}

func TestReadBoundedRejectsAuthoritativeFooterLengths(t *testing.T) {
	t.Run("raw length", func(t *testing.T) {
		dir := t.TempDir()
		content := bytes.Repeat([]byte("compressible"), 1024)
		idx := buildPack(t, dir, content)
		hash := hashOf(content)
		entry := idx[hash]
		require.Less(t, entry.StoredLen, entry.RawLen)
		s := New(&mapIndex{m: idx}, dir)
		defer func() { require.NoError(t, s.Close()) }()

		_, _, err := s.ReadBounded(hash, entry.StoredLen)
		assert.ErrorIs(t, err, ErrBlobTooLarge)
	})

	t.Run("stored length", func(t *testing.T) {
		dir := t.TempDir()
		content := []byte("footer stored length is authoritative")
		idx := buildPack(t, dir, content)
		hash := hashOf(content)
		entry := idx[hash]
		path := filepath.Join(dir, "packs", entry.PackID[:2], entry.PackID+PackExt)
		rewritePlainPackFooter(t, path, func(footer []byte) {
			// Claim a smaller raw length while retaining the genuine stored
			// span. ReadBlob would reject this frame, but the bounded read must
			// reject its stored allocation first.
			binary.LittleEndian.PutUint64(footer[4+48:], uint64(len(content)-1))
		})
		entry.RawLen = int64(len(content) - 1)
		s := New(&mapIndex{m: idx}, dir)
		defer func() { require.NoError(t, s.Close()) }()

		_, _, err := s.ReadBounded(hash, entry.RawLen)
		assert.ErrorIs(t, err, ErrBlobTooLarge)
	})
}

func TestReadBoundedCapsCompressedOutputAtFooterRawLength(t *testing.T) {
	dir := t.TempDir()
	content := bytes.Repeat([]byte("zstd expansion must stop at the footer bound"), 1<<18)
	idx := buildPack(t, dir, content)
	hash := hashOf(content)
	entry := idx[hash]
	require.Equal(t, uint8(pack.BlobCompressed), entry.Flags&uint8(pack.BlobCompressed))

	const forgedRawLen = 64
	path := filepath.Join(dir, "packs", entry.PackID[:2], entry.PackID+PackExt)
	rewritePlainPackFooter(t, path, func(footer []byte) {
		binary.LittleEndian.PutUint64(footer[4+48:], forgedRawLen)
	})
	entry.RawLen = forgedRawLen
	maxBytes := max(entry.StoredLen, int64(forgedRawLen))
	s := New(&mapIndex{m: idx}, dir)
	defer func() { require.NoError(t, s.Close()) }()

	_, _, err := s.ReadBounded(hash, maxBytes)
	require.ErrorIs(t, err, pack.ErrCorrupt)
	assert.ErrorContains(t, err, "exceeds declared raw length",
		"the bounded decoder must stop at its guarded destination capacity")
}

func TestReadBoundedRejectsDuplicateFooterBlobIDs(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	content := []byte("duplicate footer blob id")
	staging := t.TempDir()
	w, err := pack.NewWriter(staging, pack.WriterOptions{})
	require.NoError(err)
	first, err := w.Append(content)
	require.NoError(err)
	_, err = w.Append(content)
	require.NoError(err)
	packID := w.ID()
	path := filepath.Join(dir, "packs", packID[:2], packID+PackExt)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	_, err = w.Seal(path)
	require.NoError(err)

	hash := hashOf(content)
	entry := &store.PackIndexEntry{
		BlobHash: hash, PackID: packID, Offset: int64(first.Offset),
		StoredLen: int64(first.StoredLen), RawLen: int64(first.RawLen),
		Flags: uint8(first.Flags), CRC32C: first.CRC32C,
	}
	s := New(&mapIndex{m: map[string]*store.PackIndexEntry{hash: entry}}, dir)
	defer func() { require.NoError(s.Close()) }()

	_, _, err = s.ReadBounded(hash, int64(len(content)))
	require.ErrorIs(err, pack.ErrCorrupt)
	assert.ErrorContains(t, err, "duplicate blob id")
}

func TestReadBoundedRejectsUnsupportedFooterFlags(t *testing.T) {
	for name, flags := range map[string]uint8{
		"encrypted": uint8(pack.BlobEncrypted),
		"unknown":   1 << 7,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			content := []byte("unsupported bounded footer flags")
			idx := buildPack(t, dir, content)
			hash := hashOf(content)
			entry := idx[hash]
			path := filepath.Join(dir, "packs", entry.PackID[:2], entry.PackID+PackExt)
			rewritePlainPackFooter(t, path, func(footer []byte) {
				footer[4+56] = flags
			})
			entry.Flags = flags
			s := New(&mapIndex{m: idx}, dir)
			defer func() { require.NoError(t, s.Close()) }()

			_, _, err := s.ReadBounded(hash, int64(len(content)))
			require.ErrorIs(t, err, pack.ErrCorrupt)
			assert.ErrorContains(t, err, "unsupported blob flags")
		})
	}
}

func TestReadBoundedRejectsPackWithAnyRawLengthAboveKitV1Maximum(t *testing.T) {
	dir := t.TempDir()
	wanted := []byte("valid target in structurally invalid pack")
	other := []byte("entry with impossible raw length")
	idx := buildPack(t, dir, wanted, other)
	wantedHash := hashOf(wanted)
	entry := idx[wantedHash]
	path := filepath.Join(dir, "packs", entry.PackID[:2], entry.PackID+PackExt)
	rewritePlainPackFooter(t, path, func(footer []byte) {
		const secondEntry = 4 + 61
		binary.LittleEndian.PutUint64(footer[secondEntry+48:], uint64(pack.MaxRawLen)+1)
	})
	s := New(&mapIndex{m: idx}, dir)
	defer func() { require.NoError(t, s.Close()) }()

	_, _, err := s.ReadBounded(wantedHash, int64(len(wanted)))
	require.ErrorIs(t, err, pack.ErrCorrupt,
		"every footer entry is structurally validated before any target can be served")
	assert.NotErrorIs(t, err, ErrBlobTooLarge,
		"the pack is corrupt; this is not a target-specific byte-limit rejection")
}

func TestReadBoundedVerifiesLocalPackIntegrity(t *testing.T) {
	t.Run("footer checksum", func(t *testing.T) {
		dir := t.TempDir()
		content := []byte("bounded footer checksum")
		idx := buildPack(t, dir, content)
		hash := hashOf(content)
		entry := idx[hash]
		path := filepath.Join(dir, "packs", entry.PackID[:2], entry.PackID+PackExt)
		contents, err := os.ReadFile(path)
		require.NoError(t, err)
		footerLen := int(binary.LittleEndian.Uint32(contents[len(contents)-40:]))
		contents[len(contents)-40-footerLen+4] ^= 0x01
		require.NoError(t, os.WriteFile(path, contents, 0o600))
		s := New(&mapIndex{m: idx}, dir)
		defer func() { require.NoError(t, s.Close()) }()

		_, _, err = s.ReadBounded(hash, int64(len(content)))
		assert.ErrorIs(t, err, pack.ErrChecksum)
	})

	t.Run("stored crc32c", func(t *testing.T) {
		require := require.New(t)
		dir := t.TempDir()
		content := []byte("bounded stored crc32c")
		idx := buildPack(t, dir, content)
		hash := hashOf(content)
		entry := idx[hash]
		path := filepath.Join(dir, "packs", entry.PackID[:2], entry.PackID+PackExt)
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		require.NoError(err)
		stored := make([]byte, entry.StoredLen)
		_, err = f.ReadAt(stored, entry.Offset)
		require.NoError(err)
		stored[0] ^= 0x01
		_, err = f.WriteAt(stored, entry.Offset)
		require.NoError(err)
		require.NoError(f.Close())
		s := New(&mapIndex{m: idx}, dir)
		defer func() { require.NoError(s.Close()) }()

		_, _, err = s.ReadBounded(hash, int64(len(content)))
		require.ErrorIs(err, pack.ErrCorrupt)
		assert.ErrorContains(t, err, "crc mismatch")
	})

	t.Run("blob sha256", func(t *testing.T) {
		require := require.New(t)
		dir := t.TempDir()
		content := []byte("bounded blob sha256")
		idx := buildPack(t, dir, content)
		hash := hashOf(content)
		entry := idx[hash]
		path := filepath.Join(dir, "packs", entry.PackID[:2], entry.PackID+PackExt)
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		require.NoError(err)
		stored := make([]byte, entry.StoredLen)
		_, err = f.ReadAt(stored, entry.Offset)
		require.NoError(err)
		stored[0] ^= 0x01
		_, err = f.WriteAt(stored, entry.Offset)
		require.NoError(err)
		require.NoError(f.Close())
		newCRC := crc32.Checksum(stored, crc32.MakeTable(crc32.Castagnoli))
		rewritePlainPackFooter(t, path, func(footer []byte) {
			binary.LittleEndian.PutUint32(footer[4+57:], newCRC)
		})
		entry.CRC32C = newCRC
		s := New(&mapIndex{m: idx}, dir)
		defer func() { require.NoError(s.Close()) }()

		_, _, err = s.ReadBounded(hash, int64(len(content)))
		assert.ErrorIs(t, err, pack.ErrBlobMismatch)
	})
}

func TestReadBoundedRejectsForgedIndexMetadata(t *testing.T) {
	dir := t.TempDir()
	content := []byte("footer metadata must override the database index")
	idx := buildPack(t, dir, content)
	hash := hashOf(content)
	good := *idx[hash]

	tests := map[string]func(*store.PackIndexEntry){
		"blob hash":  func(e *store.PackIndexEntry) { e.BlobHash = hashOf([]byte("other blob")) },
		"offset":     func(e *store.PackIndexEntry) { e.Offset++ },
		"stored len": func(e *store.PackIndexEntry) { e.StoredLen-- },
		"raw len":    func(e *store.PackIndexEntry) { e.RawLen++ },
		"flags":      func(e *store.PackIndexEntry) { e.Flags ^= uint8(pack.BlobCompressed) },
		"crc32c":     func(e *store.PackIndexEntry) { e.CRC32C++ },
	}
	for name, forge := range tests {
		t.Run(name, func(t *testing.T) {
			forged := good
			forge(&forged)
			s := New(&mapIndex{m: map[string]*store.PackIndexEntry{hash: &forged}}, dir)
			defer func() { require.NoError(t, s.Close()) }()

			_, _, err := s.ReadBounded(hash, MaxMaintenanceBlobBytes)
			require.Error(t, err)
			require.ErrorContains(t, err, "pack index metadata")
			assert.NotErrorIs(t, err, fs.ErrNotExist)
		})
	}
}

func TestReadBoundedPreservesLooseFallbackAndRetries(t *testing.T) {
	t.Run("loose fallback", func(t *testing.T) {
		dir := t.TempDir()
		content := []byte("bounded loose fallback")
		hash := hashOf(content)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, hash[:2]), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, hash[:2], hash), content, 0o600))
		s := New(&mapIndex{m: map[string]*store.PackIndexEntry{}, referenced: map[string]bool{hash: true}}, dir)
		defer func() { require.NoError(t, s.Close()) }()
		assert.Equal(t, content, readBounded(t, s, hash, int64(len(content))))
	})

	t.Run("packer index race", func(t *testing.T) {
		dir := t.TempDir()
		content := []byte("bounded packed between lookups")
		idx := buildPack(t, dir, content)
		hash := hashOf(content)
		s := New(&flipIndex{entry: idx[hash]}, dir)
		defer func() { require.NoError(t, s.Close()) }()
		assert.Equal(t, content, readBounded(t, s, hash, int64(len(content))))
	})

	t.Run("repacker missing file race", func(t *testing.T) {
		dir := t.TempDir()
		content := []byte("bounded survives missing old pack")
		idx := buildPack(t, dir, content)
		hash := hashOf(content)
		stale := *idx[hash]
		stale.PackID = pack.NewPackID()
		s := New(&staleIndex{stale: &stale, live: idx[hash]}, dir)
		defer func() { require.NoError(t, s.Close()) }()
		assert.Equal(t, content, readBounded(t, s, hash, int64(len(content))))
	})

	t.Run("repacker missing footer entry race", func(t *testing.T) {
		dir := t.TempDir()
		wanted := []byte("bounded replacement footer entry")
		other := []byte("stale pack contains a different blob")
		staleIndexEntries := buildPack(t, dir, other)
		liveIndex := buildPack(t, dir, wanted)
		hash := hashOf(wanted)
		stale := *staleIndexEntries[hashOf(other)]
		stale.BlobHash = hash
		s := New(&staleIndex{stale: &stale, live: liveIndex[hash]}, dir)
		defer func() { require.NoError(t, s.Close()) }()
		assert.Equal(t, wanted, readBounded(t, s, hash, int64(len(wanted))))
	})
}

func TestOpenPacked(t *testing.T) {
	dir := t.TempDir()
	content := []byte("packed blob content")
	idx := buildPack(t, dir, content)
	s := New(&mapIndex{m: idx}, dir)
	defer func() { _ = s.Close() }()

	assert.Equal(t, content, readAll(t, s, hashOf(content)))
}

func TestOpenLooseFallback(t *testing.T) {
	dir := t.TempDir()
	content := []byte("loose blob content")
	h := hashOf(content)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, h[:2]), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, h[:2], h), content, 0o600))

	s := New(&mapIndex{
		m: map[string]*store.PackIndexEntry{}, referenced: map[string]bool{h: true},
	}, dir)
	defer func() { _ = s.Close() }()
	assert.Equal(t, content, readAll(t, s, h))
}

func TestOpenNotFound(t *testing.T) {
	s := New(&mapIndex{m: map[string]*store.PackIndexEntry{}}, t.TempDir())
	defer func() { _ = s.Close() }()
	_, _, err := s.Open(hashOf([]byte("nowhere")))
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

func TestOpenRejectsUnreferenced(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	content := []byte("logically deleted attachment")
	h := hashOf(content)
	idx := buildPack(t, dir, content)
	require.NoError(os.MkdirAll(filepath.Join(dir, h[:2]), 0o700))
	require.NoError(os.WriteFile(filepath.Join(dir, h[:2], h), content, 0o600))

	s := New(&mapIndex{m: idx, unreferenced: map[string]bool{h: true}}, dir)
	defer func() { require.NoError(s.Close()) }()
	_, _, err := s.Open(h)
	require.ErrorIs(err, fs.ErrNotExist,
		"neither a stale pack mapping nor loose crash copy may bypass logical deletion")
	assert.Empty(s.readers, "production resolution rejects the hash before opening its pack")
}

func TestOpenRejectsCorruptPack(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	content := []byte("integrity checked content")
	idx := buildPack(t, dir, content)
	h := hashOf(content)

	// Flip one byte of the stored blob on disk.
	e := idx[h]
	p := filepath.Join(dir, "packs", e.PackID[:2], e.PackID+PackExt)
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	require.NoError(err)
	buf := []byte{0}
	_, err = f.ReadAt(buf, e.Offset)
	require.NoError(err)
	buf[0] ^= 0xFF
	_, err = f.WriteAt(buf, e.Offset)
	require.NoError(err)
	require.NoError(f.Close())

	s := New(&mapIndex{m: idx}, dir)
	defer func() { _ = s.Close() }()
	_, _, err = s.Open(h)
	require.Error(err)
}

func TestOpenRejectsInvalidPackID(t *testing.T) {
	h := hashOf([]byte("x"))
	idx := map[string]*store.PackIndexEntry{
		h: {BlobHash: h, PackID: "../../../etc/passwd"},
	}
	s := New(&mapIndex{m: idx}, t.TempDir())
	defer func() { _ = s.Close() }()
	_, _, err := s.Open(h)
	require.Error(t, err)
	assert.NotErrorIs(t, err, fs.ErrNotExist)
}

func TestOpenRejectsForgedPackMetadataBeforeReadBlob(t *testing.T) {
	dir := t.TempDir()
	content := []byte("ordinary reads trust the footer, not database coordinates")
	idx := buildPack(t, dir, content)
	hash := hashOf(content)
	good := *idx[hash]
	const hugeDBValue int64 = 1<<63 - 1

	tests := map[string]func(*store.PackIndexEntry){
		"blob hash":  func(e *store.PackIndexEntry) { e.BlobHash = hashOf([]byte("other ordinary blob")) },
		"offset":     func(e *store.PackIndexEntry) { e.Offset = hugeDBValue },
		"stored len": func(e *store.PackIndexEntry) { e.StoredLen = hugeDBValue },
		"raw len":    func(e *store.PackIndexEntry) { e.RawLen = hugeDBValue },
		"flags":      func(e *store.PackIndexEntry) { e.Flags ^= uint8(pack.BlobCompressed) },
		"crc32c":     func(e *store.PackIndexEntry) { e.CRC32C++ },
	}
	for name, forge := range tests {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			forged := good
			forge(&forged)
			s := New(&mapIndex{m: map[string]*store.PackIndexEntry{hash: &forged}}, dir)
			defer func() { require.NoError(s.Close()) }()

			var openErr error
			assert.NotPanics(func() {
				r, _, err := s.Open(hash)
				openErr = err
				if r != nil {
					_ = r.Close()
				}
			}, "forged positive metadata must be rejected before kit allocates")
			require.Error(openErr)
			require.ErrorContains(openErr, "pack index metadata mismatch")
			assert.NotErrorIs(openErr, fs.ErrNotExist)
		})
	}
}

func TestOpenRejectsDuplicateFooterBlobIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	content := []byte("ordinary duplicate footer blob id")
	w, err := pack.NewWriter(t.TempDir(), pack.WriterOptions{})
	require.NoError(err)
	first, err := w.Append(content)
	require.NoError(err)
	_, err = w.Append(content)
	require.NoError(err)
	packID := w.ID()
	path := filepath.Join(dir, "packs", packID[:2], packID+PackExt)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	_, err = w.Seal(path)
	require.NoError(err)

	hash := hashOf(content)
	entry := &store.PackIndexEntry{
		BlobHash: hash, PackID: packID, Offset: int64(first.Offset),
		StoredLen: int64(first.StoredLen), RawLen: int64(first.RawLen),
		Flags: uint8(first.Flags), CRC32C: first.CRC32C,
	}
	s := New(&mapIndex{m: map[string]*store.PackIndexEntry{hash: entry}}, dir)
	defer func() { require.NoError(s.Close()) }()

	_, _, err = s.Open(hash)
	require.ErrorIs(err, pack.ErrCorrupt)
	assert.ErrorContains(err, "duplicate blob id")
}

// flipIndex returns nothing on the first lookup, then the real entry —
// simulating the packer committing between a reader's index miss and its
// loose-file open (the loose file never existed here).
type flipIndex struct {
	first bool
	entry *store.PackIndexEntry
}

func (i *flipIndex) ResolveAttachmentBlob(string) (store.AttachmentBlobLocation, error) {
	if !i.first {
		i.first = true
		return store.AttachmentBlobLocation{Referenced: true}, nil
	}
	return store.AttachmentBlobLocation{Referenced: true, Pack: i.entry}, nil
}

func TestOpenRetriesIndexAfterLooseMiss(t *testing.T) {
	dir := t.TempDir()
	content := []byte("packed between lookups")
	idx := buildPack(t, dir, content)
	h := hashOf(content)

	s := New(&flipIndex{entry: idx[h]}, dir)
	defer func() { _ = s.Close() }()
	assert.Equal(t, content, readAll(t, s, h))
}

// staleIndex returns a dangling pack entry first, then the live one —
// simulating a repack swapping rows and deleting the old pack mid-read.
type staleIndex struct {
	served bool
	stale  *store.PackIndexEntry
	live   *store.PackIndexEntry
}

func (i *staleIndex) ResolveAttachmentBlob(string) (store.AttachmentBlobLocation, error) {
	if !i.served {
		i.served = true
		return store.AttachmentBlobLocation{Referenced: true, Pack: i.stale}, nil
	}
	return store.AttachmentBlobLocation{Referenced: true, Pack: i.live}, nil
}

func TestOpenRetriesIndexAfterPackMiss(t *testing.T) {
	dir := t.TempDir()
	content := []byte("survives repack race")
	idx := buildPack(t, dir, content)
	h := hashOf(content)

	stale := *idx[h]
	// A syntactically valid ULID that names no pack file on disk (real
	// generated pack IDs are random, so collision with idx[h].PackID is not
	// a realistic concern here).
	stale.PackID = pack.NewPackID()

	s := New(&staleIndex{stale: &stale, live: idx[h]}, dir)
	defer func() { _ = s.Close() }()
	assert.Equal(t, content, readAll(t, s, h))
}

func TestOpenRetriesIndexAfterFooterEntryMiss(t *testing.T) {
	dir := t.TempDir()
	wanted := []byte("ordinary replacement footer entry")
	other := []byte("ordinary stale pack contains another blob")
	staleEntries := buildPack(t, dir, other)
	liveEntries := buildPack(t, dir, wanted)
	hash := hashOf(wanted)
	stale := *staleEntries[hashOf(other)]
	stale.BlobHash = hash

	s := New(&staleIndex{stale: &stale, live: liveEntries[hash]}, dir)
	defer func() { require.NoError(t, s.Close()) }()
	assert.Equal(t, wanted, readAll(t, s, hash))
}

// TestOpenConcurrent runs concurrent Open calls against a mix of packed and
// loose hashes; run with -race to catch data races in the reader cache.
func TestOpenConcurrent(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()

	packedBlobs := [][]byte{
		[]byte("concurrent packed blob one"),
		[]byte("concurrent packed blob two"),
		[]byte("concurrent packed blob three"),
	}
	idx := buildPack(t, dir, packedBlobs...)

	looseBlobs := [][]byte{
		[]byte("concurrent loose blob one"),
		[]byte("concurrent loose blob two"),
	}
	for _, b := range looseBlobs {
		h := hashOf(b)
		if idx[h] != nil {
			require.FailNow("loose hash unexpectedly collides with packed fixture")
		}
		require.NoError(os.MkdirAll(filepath.Join(dir, h[:2]), 0o700))
		require.NoError(os.WriteFile(filepath.Join(dir, h[:2], h), b, 0o600))
	}

	all := make([][]byte, 0, len(packedBlobs)+len(looseBlobs))
	all = append(all, packedBlobs...)
	all = append(all, looseBlobs...)

	referenced := make(map[string]bool, len(looseBlobs))
	for _, b := range looseBlobs {
		referenced[hashOf(b)] = true
	}
	s := New(&mapIndex{m: idx, referenced: referenced}, dir)
	defer func() { _ = s.Close() }()

	// t.Error/t.Errorf are safe to call from non-test goroutines; t.Fatal
	// family is not, so failures are collected here and reported after
	// wg.Wait rather than inside the goroutines.
	const goroutines = 8
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*len(all))
	for range goroutines {
		wg.Go(func() {
			for _, want := range all {
				h := hashOf(want)
				r, size, err := s.Open(h)
				if err != nil {
					errCh <- fmt.Errorf("open %s: %w", h, err)
					continue
				}
				got, err := io.ReadAll(r)
				_ = r.Close()
				if err != nil {
					errCh <- fmt.Errorf("read %s: %w", h, err)
					continue
				}
				if size != int64(len(got)) || !bytes.Equal(got, want) {
					errCh <- fmt.Errorf("content mismatch for %s", h)
				}
			}
		})
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		assert.NoError(t, err)
	}
}

// TestOpenAndReadBoundedConcurrent exercises the ordinary and bounded caches
// for the same packs under the race detector.
func TestOpenAndReadBoundedConcurrent(t *testing.T) {
	assert := assert.New(t)
	dir := t.TempDir()
	blobs := [][]byte{
		[]byte("mixed cache packed blob one"),
		[]byte("mixed cache packed blob two"),
		bytes.Repeat([]byte("mixed compressed cache blob"), 256),
	}
	idx := buildPack(t, dir, blobs...)
	s := New(&mapIndex{m: idx}, dir)
	defer func() { require.NoError(t, s.Close()) }()

	const goroutines = 8
	const iterations = 10
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*iterations*len(blobs))
	for worker := range goroutines {
		wg.Go(func() {
			for iteration := range iterations {
				for _, want := range blobs {
					hash := hashOf(want)
					if (worker+iteration)%2 == 0 {
						r, size, err := s.Open(hash)
						if err != nil {
							errCh <- fmt.Errorf("open %s: %w", hash, err)
							continue
						}
						got, err := io.ReadAll(r)
						_ = r.Close()
						if err != nil {
							errCh <- fmt.Errorf("read ordinary %s: %w", hash, err)
							continue
						}
						if size != int64(len(got)) || !bytes.Equal(got, want) {
							errCh <- fmt.Errorf("ordinary content mismatch for %s", hash)
						}
						continue
					}
					got, size, err := s.ReadBounded(hash, int64(len(want)))
					if err != nil {
						errCh <- fmt.Errorf("read bounded %s: %w", hash, err)
						continue
					}
					if size != int64(len(got)) || !bytes.Equal(got, want) {
						errCh <- fmt.Errorf("bounded content mismatch for %s", hash)
					}
				}
			}
		})
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	s.mu.Lock()
	assert.Len(s.readers, 1)
	assert.Len(s.boundedReaders, 1)
	assert.Len(s.order, 1, "ordinary and bounded readers for one pack share a FIFO slot")
	s.mu.Unlock()
}

// TestOpenEvictsAndReopens builds more packs than maxOpenReaders, opens each
// through one Store to force FIFO eviction, then re-opens the first pack
// (guaranteed evicted) to prove eviction correctly closes and a later Open
// reopens the pack file rather than returning stale/closed state.
func TestOpenEvictsAndReopens(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()

	const numPacks = maxOpenReaders + 4
	blobs := make([][]byte, numPacks)
	idx := make(map[string]*store.PackIndexEntry, numPacks)
	for i := range numPacks {
		b := fmt.Appendf(nil, "eviction blob %d", i)
		blobs[i] = b
		maps.Copy(idx, buildPack(t, dir, b))
	}

	s := New(&mapIndex{m: idx}, dir)
	defer func() { _ = s.Close() }()

	for _, b := range blobs {
		assert.Equal(t, b, readAll(t, s, hashOf(b)))
	}

	s.mu.Lock()
	numOpen := len(s.readers)
	s.mu.Unlock()
	require.LessOrEqual(numOpen, maxOpenReaders)

	// The first pack opened is the first evicted under FIFO; re-opening it
	// must still return correct content.
	assert.Equal(t, blobs[0], readAll(t, s, hashOf(blobs[0])))
}

func TestReadBoundedEvictsClosesAndReopens(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	const numPacks = maxOpenReaders + 1
	blobs := make([][]byte, numPacks)
	idx := make(map[string]*store.PackIndexEntry, numPacks)
	for i := range numPacks {
		blob := fmt.Appendf(nil, "bounded eviction blob %d", i)
		blobs[i] = blob
		maps.Copy(idx, buildPack(t, dir, blob))
	}
	s := New(&mapIndex{m: idx}, dir)
	defer func() { require.NoError(s.Close()) }()

	firstHash := hashOf(blobs[0])
	assert.Equal(blobs[0], readBounded(t, s, firstHash, int64(len(blobs[0]))))
	firstPackID := idx[firstHash].PackID
	s.mu.Lock()
	firstReader := s.boundedReaders[firstPackID]
	s.mu.Unlock()
	require.NotNil(firstReader)

	for _, blob := range blobs[1:] {
		assert.Equal(blob, readBounded(t, s, hashOf(blob), int64(len(blob))))
	}
	s.mu.Lock()
	_, stillCached := s.boundedReaders[firstPackID]
	numBounded := len(s.boundedReaders)
	numSlots := len(s.order)
	s.mu.Unlock()
	assert.False(stillCached)
	assert.LessOrEqual(numBounded, maxOpenReaders)
	assert.LessOrEqual(numSlots, maxOpenReaders)
	_, err := firstReader.file.ReadAt(make([]byte, 1), 0)
	require.Error(err, "FIFO eviction closes the bounded descriptor")

	assert.Equal(blobs[0], readBounded(t, s, firstHash, int64(len(blobs[0]))))
	s.mu.Lock()
	reopened := s.boundedReaders[firstPackID]
	s.mu.Unlock()
	require.NotNil(reopened)
	assert.NotSame(firstReader, reopened)
}

func TestRetirePackValidatesAndTreatsMissingAsSuccess(t *testing.T) {
	s := New(&mapIndex{m: map[string]*store.PackIndexEntry{}}, t.TempDir())
	defer func() { require.NoError(t, s.Close()) }()

	require.ErrorContains(t, s.RetirePack("../../../outside"), "invalid pack id")
	require.NoError(t, s.RetirePack(pack.NewPackID()), "an absent canonical pack file is already retired")
}

func TestRetirePackClosesCacheAndRemovesEveryFIFOEntry(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	content := []byte("reader retired before physical deletion")
	idx := buildPack(t, dir, content)
	hash := hashOf(content)
	entry := idx[hash]
	s := New(&mapIndex{m: idx}, dir)
	defer func() { require.NoError(s.Close()) }()

	assert.Equal(content, readAll(t, s, hash))
	s.mu.Lock()
	cached := s.readers[entry.PackID]
	require.NotNil(cached)
	s.order = append(s.order, entry.PackID, entry.PackID)
	s.mu.Unlock()

	require.NoError(s.RetirePack(entry.PackID))
	s.mu.Lock()
	_, cachedStillPresent := s.readers[entry.PackID]
	order := append([]string(nil), s.order...)
	s.mu.Unlock()
	assert.False(cachedStillPresent)
	assert.NotContains(order, entry.PackID)
	_, statErr := os.Stat(filepath.Join(dir, "packs", entry.PackID[:2], entry.PackID+PackExt))
	require.ErrorIs(statErr, fs.ErrNotExist)

	blobID, err := pack.ParseBlobID(hash)
	require.NoError(err)
	_, err = cached.ReadBlob(pack.Entry{
		ID: blobID, Offset: uint64(entry.Offset), StoredLen: uint64(entry.StoredLen),
		RawLen: uint64(entry.RawLen), Flags: pack.BlobFlags(entry.Flags), CRC32C: entry.CRC32C,
	})
	assert.Error(err, "retirement closes the daemon-owned cached reader")
}

func TestRetirePackAllowsStaleIndexRetryToReplacement(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	content := []byte("stale index retries after old reader retirement")
	oldIndex := buildPack(t, dir, content)
	newIndex := buildPack(t, dir, content)
	hash := hashOf(content)
	oldEntry := oldIndex[hash]

	daemon := New(&mapIndex{m: oldIndex}, dir)
	require.Equal(content, readAll(t, daemon, hash))
	require.NoError(daemon.RetirePack(oldEntry.PackID))
	require.NoError(daemon.Close())

	retrying := New(&staleIndex{stale: oldEntry, live: newIndex[hash]}, dir)
	defer func() { require.NoError(retrying.Close()) }()
	assert.Equal(content, readAll(t, retrying, hash))
}

func TestIndependentReaderAcrossRetire(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	first := []byte("first blob opens the independent old-pack reader")
	second := []byte("second blob proves the independent old reader remains usable")
	oldIndex := buildPack(t, dir, first, second)
	newIndex := buildPack(t, dir, first, second)
	oldPackID := oldIndex[hashOf(first)].PackID
	oldPath := filepath.Join(dir, "packs", oldPackID[:2], oldPackID+PackExt)

	backupReader := New(&mapIndex{m: oldIndex}, dir)
	assert.Equal(first, readAll(t, backupReader, hashOf(first)))

	daemon := New(&mapIndex{m: newIndex}, dir)
	defer func() { require.NoError(daemon.Close()) }()
	retireErr := daemon.RetirePack(oldPackID)
	if runtime.GOOS == "windows" {
		require.Error(retireErr, "Windows must retain a pack held by an independent reader")
		_, err := os.Stat(oldPath)
		require.NoError(err)
		require.NoError(backupReader.Close())
		require.NoError(daemon.RetirePack(oldPackID))
		_, err = os.Stat(oldPath)
		require.ErrorIs(err, fs.ErrNotExist)
	} else {
		require.NoError(retireErr)
		_, err := os.Stat(oldPath)
		require.ErrorIs(err, fs.ErrNotExist)
		assert.Equal(second, readAll(t, backupReader, hashOf(second)),
			"Unix open handles remain usable after unlink")
		require.NoError(backupReader.Close())
	}

	assert.Equal(second, readAll(t, daemon, hashOf(second)),
		"new daemon opens follow the replacement mapping")
}
