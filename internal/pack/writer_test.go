package pack

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriterLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	require.NoError(os.MkdirAll(staging, 0o700))

	w, err := NewWriter(staging, WriterOptions{})
	require.NoError(err)
	assert.True(IsValidPackID(w.ID()))

	compressible := bytes.Repeat([]byte("hello msgvault "), 1000)
	e1, err := w.Append(compressible)
	require.NoError(err)
	assert.Equal(ComputeBlobID(compressible), e1.ID)
	assert.Equal(uint64(headerSize), e1.Offset)
	assert.Equal(uint64(len(compressible)), e1.RawLen)
	assert.Equal(BlobCompressed, e1.Flags)
	assert.Less(e1.StoredLen, e1.RawLen)

	small := []byte{0xFF, 0x00, 0xAB}
	e2, err := w.Append(small)
	require.NoError(err)
	assert.Equal(e1.Offset+e1.StoredLen, e2.Offset)
	assert.Equal(BlobFlags(0), e2.Flags)
	assert.Equal(uint64(3), e2.StoredLen)

	assert.Equal(int64(e2.Offset+e2.StoredLen), w.StoredSize())
	assert.False(w.Full(), "default target is 32 MiB")

	final := filepath.Join(dir, "packs", w.ID()[:2], w.ID()+".mvpack")
	entries, err := w.Seal(final)
	require.NoError(err)
	assert.Equal([]Entry{e1, e2}, entries)

	_, err = os.Stat(final)
	//nolint:testifylint // independent non-blocking check
	assert.NoError(err, "pack exists at final path")
	leftovers, err := os.ReadDir(staging)
	require.NoError(err)
	assert.Empty(leftovers, "staging file removed by rename")

	_, err = w.Append([]byte("late"))
	//nolint:testifylint // independent non-blocking check
	assert.ErrorIs(err, ErrSealed)
	_, err = w.Seal(final)
	assert.ErrorIs(err, ErrSealed)
}

// TestWriterAppendEncodedMatchesAppend pins AppendEncoded's contract: a
// frame produced by EncodeFrame yields the exact entry Append would have
// produced for the same raw bytes, and the sealed pack reads back normally.
func TestWriterAppendEncodedMatchesAppend(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	require.NoError(os.MkdirAll(staging, 0o700))

	compressible := bytes.Repeat([]byte("hello msgvault "), 1000)
	incompressible := []byte{0xFF, 0x00, 0xAB, 0x17, 0x99}

	wa, err := NewWriter(staging, WriterOptions{})
	require.NoError(err)
	a1, err := wa.Append(compressible)
	require.NoError(err)
	a2, err := wa.Append(incompressible)
	require.NoError(err)
	require.NoError(wa.Abort())

	wb, err := NewWriter(staging, WriterOptions{})
	require.NoError(err)
	for i, raw := range [][]byte{compressible, incompressible} {
		frame, compressed := EncodeFrame(raw, 0)
		e, appendErr := wb.AppendEncoded(ComputeBlobID(raw), frame, uint64(len(raw)), compressed)
		require.NoError(appendErr)
		want := []Entry{a1, a2}[i]
		assert.Equal(want, e, "entry %d must match Append's output exactly", i)
	}

	final := filepath.Join(dir, wb.ID()+".mvpack")
	_, err = wb.Seal(final)
	require.NoError(err)
	r, err := OpenReader(final, nil)
	require.NoError(err)
	defer func() { _ = r.Close() }()
	for i, raw := range [][]byte{compressible, incompressible} {
		got, readErr := r.ReadBlob(r.Entries()[i])
		require.NoError(readErr)
		assert.Equal(raw, got)
	}
}

func TestWriterAppendEncodedRejectsBadInput(t *testing.T) {
	require := require.New(t)
	staging := t.TempDir()
	w, err := NewWriter(staging, WriterOptions{})
	require.NoError(err)
	defer func() { _ = w.Abort() }()

	raw := []byte{0xFF, 0x00, 0xAB}
	// An uncompressed frame whose length disagrees with rawLen is a caller
	// bug that would corrupt the pack; it must be rejected up front.
	_, err = w.AppendEncoded(ComputeBlobID(raw), raw, uint64(len(raw))+1, false)
	require.ErrorContains(err, "claims raw length")

	// Raw lengths beyond the decoder maximum are rejected like Append does.
	_, err = w.AppendEncoded(ComputeBlobID(raw), raw, maxRawLen+1, true)
	require.ErrorContains(err, "exceeds max")
}

func TestWriterAbort(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	staging := t.TempDir()
	w, err := NewWriter(staging, WriterOptions{})
	require.NoError(err)
	_, err = w.Append([]byte("data"))
	require.NoError(err)
	require.NoError(w.Abort())

	leftovers, err := os.ReadDir(staging)
	require.NoError(err)
	assert.Empty(leftovers, "abort removes the staging file")

	_, err = w.Append([]byte("late"))
	assert.ErrorIs(err, ErrSealed)
}

func TestWriterSealDirSyncFailureKeepsPackPublished(t *testing.T) {
	// If os.Rename succeeds but the following directory sync fails, Seal must
	// still mark the Writer done: the pack is already published at
	// finalPath, so a following Abort must be a no-op rather than deleting
	// the published file out from under the caller.
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	require.NoError(os.MkdirAll(staging, 0o700))

	w, err := NewWriter(staging, WriterOptions{})
	require.NoError(err)
	_, err = w.Append([]byte("data"))
	require.NoError(err)

	// Pre-create the destination directory so Seal's mkdirAllSynced call is
	// a no-op and does not itself invoke syncDir; the failure this test
	// injects must land only on the single post-rename sync of the leaf
	// directory, matching the doc comment above.
	final := filepath.Join(dir, "packs", w.ID()[:2], w.ID()+".mvpack")
	require.NoError(os.MkdirAll(filepath.Dir(final), 0o700))

	origSyncDir := SyncDir
	SyncDir = func(d string) error {
		if err := origSyncDir(d); err != nil {
			return err
		}
		return errors.New("simulated directory sync failure")
	}
	t.Cleanup(func() { SyncDir = origSyncDir })

	_, err = w.Seal(final)
	require.Error(err)
	assert.Contains(err.Error(), final)

	_, statErr := os.Stat(final)
	assert.NoError(statErr, "pack still exists at final path")

	assert.NoError(w.Abort(), "abort after failed sync is a no-op")
	_, statErr = os.Stat(final)
	//nolint:testifylint // independent non-blocking check
	assert.NoError(statErr, "abort must not remove the published pack")

	_, err = w.Append([]byte("late"))
	assert.ErrorIs(err, ErrSealed)
}

func TestWriterAppendPoisonsOnWriteFailure(t *testing.T) {
	// A write failure partway through Append (for example ENOSPC) can leave
	// the fd position past w.off if the write was partial, so every following
	// Append or Seal must fail rather than record or publish offsets that no
	// longer match the file. Sabotage the fd directly (closing it) since the
	// package has no other seam for injecting a write failure.
	assert := assert.New(t)
	require := require.New(t)
	staging := t.TempDir()
	w, err := NewWriter(staging, WriterOptions{})
	require.NoError(err)
	require.NoError(w.f.Close(), "sabotage: further writes to the fd must fail")

	_, firstErr := w.Append([]byte("data"))
	require.Error(firstErr)

	_, err = w.Append([]byte("more"))
	require.ErrorIs(err, firstErr, "a poisoned writer repeats its error on Append")

	final := filepath.Join(t.TempDir(), w.ID()+".mvpack")
	_, err = w.Seal(final)
	require.ErrorIs(err, firstErr, "a poisoned writer repeats its error on Seal")

	require.NoError(w.Abort(), "abort still cleans up a poisoned writer")
	leftovers, err := os.ReadDir(staging)
	require.NoError(err)
	assert.Empty(leftovers, "abort removes the staging file even after poisoning")
}

func TestMkdirAllSyncedCreatesMultipleLevels(t *testing.T) {
	// mkdirAllSynced must create every missing intermediate directory, not
	// just the leaf, mirroring os.MkdirAll's tree-creation behavior while
	// additionally fsyncing each parent that gains a new child.
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	deep := filepath.Join(root, "packs", "ab", "cd")

	require.NoError(mkdirAllSynced(deep))

	for _, level := range []string{
		filepath.Join(root, "packs"),
		filepath.Join(root, "packs", "ab"),
		deep,
	} {
		info, err := os.Stat(level)
		require.NoError(err)
		assert.True(info.IsDir(), "%s must be a directory", level)
	}

	// Calling it again on an already-fully-existing path must be a no-op,
	// not an error, matching os.MkdirAll's idempotence.
	assert.NoError(mkdirAllSynced(deep))
}

func TestWriterSealFreshMultiLevelDirectory(t *testing.T) {
	// Seal's final path is often under a packs root and a shard directory
	// that don't exist yet on a fresh backup destination; both levels must
	// be created (and synced) for the pack to round-trip.
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	require.NoError(os.MkdirAll(staging, 0o700))

	w, err := NewWriter(staging, WriterOptions{})
	require.NoError(err)
	_, err = w.Append([]byte("data"))
	require.NoError(err)

	final := filepath.Join(dir, "packs", "ab", "cd", w.ID()+".mvpack")
	entries, err := w.Seal(final)
	require.NoError(err)
	assert.Len(entries, 1)

	_, statErr := os.Stat(final)
	assert.NoError(statErr, "pack exists at fresh multi-level final path")
}

func TestWriterDuplicateContent(t *testing.T) {
	// Dedup is the caller's job: appending identical content twice yields two
	// entries with the same BlobID at different offsets.
	assert := assert.New(t)
	require := require.New(t)
	staging := t.TempDir()
	w, err := NewWriter(staging, WriterOptions{})
	require.NoError(err)
	e1, err := w.Append([]byte("same"))
	require.NoError(err)
	e2, err := w.Append([]byte("same"))
	require.NoError(err)
	assert.Equal(e1.ID, e2.ID)
	assert.NotEqual(e1.Offset, e2.Offset)
	require.NoError(w.Abort())
}
