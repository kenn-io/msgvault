package blobstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
