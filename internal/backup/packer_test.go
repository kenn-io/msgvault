package backup

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/pack"
)

func TestPackAppenderAddSealsAndIndexes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	a := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
	blobA := bytes.Repeat([]byte("attachment-bytes-"), 100)
	blobB := []byte("tiny")

	idA, added, err := a.Add(blobA)
	require.NoError(err)
	assert.True(added)
	assert.Equal(pack.ComputeBlobID(blobA), idA)

	_, added, err = a.Add(blobB)
	require.NoError(err)
	assert.True(added)

	// Same content again: dedup, no new entry.
	_, added, err = a.Add(blobA)
	require.NoError(err)
	assert.False(added)

	packs, entries, err := a.Finish()
	require.NoError(err)
	require.Len(packs, 1)
	require.Len(entries, 2)

	// The sealed pack exists at its sharded path and blobs read back.
	packPath := r.Path("packs", packs[0][:2], packs[0]+".mvpack")
	_, statErr := os.Stat(packPath)
	require.NoError(statErr)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	got, err := r.ReadBlob(known, idA, nil)
	require.NoError(err)
	assert.Equal(blobA, got)
}

func TestPackAppenderDedupsAgainstKnown(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	blob := []byte("already stored")
	known := map[pack.BlobID]IndexEntry{
		pack.ComputeBlobID(blob): {Blob: pack.ComputeBlobID(blob), PackID: pack.NewPackID()},
	}
	a := NewPackAppender(r, known, pack.DefaultZstdLevel, nil)
	_, added, err := a.Add(blob)
	require.NoError(err)
	require.False(added)

	packs, entries, err := a.Finish()
	require.NoError(err)
	require.Empty(packs)
	require.Empty(entries)
}

func TestPackAppenderRotatesAtTargetSize(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	a := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
	a.targetSize = 1 << 16 // shrink for the test

	// Incompressible blobs: use cryptographically random bytes.
	for range 8 {
		blob := make([]byte, 1<<14)
		_, err := rand.Read(blob)
		require.NoError(err)
		_, _, err = a.Add(blob)
		require.NoError(err)
	}
	packs, entries, err := a.Finish()
	require.NoError(err)
	require.Greater(len(packs), 1, "expected pack rotation")
	require.Len(entries, 8)
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.PackID] = true
	}
	require.Len(seen, len(packs))
}

func TestPackAppenderPoisonsOnSealFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	a := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
	blob := []byte("test data")

	// Add one blob to the open pack.
	_, _, err := a.Add(blob)
	require.NoError(err)

	// Pre-create the pack's final shard directory. Init only creates the
	// top-level packs/ dir, not packs/<xx>/ shards, so without this the
	// always-failing SyncDir stub below would fire inside
	// mkdirAllSynced's pre-rename shard-directory creation instead of the
	// post-rename directory sync in pack.Writer.Seal, which is the path
	// this test means to exercise.
	packID := a.w.ID()
	shardDir := r.Path(packsDirName, packID[:2])
	require.NoError(os.MkdirAll(shardDir, 0o700))

	// Inject SyncDir failure during Finish. The pack has blobs so it will
	// call sealOpen, which will call Seal. Seal renames the file then calls
	// SyncDir on the directory. We inject a failure there.
	origSyncDir := pack.SyncDir
	pack.SyncDir = func(d string) error {
		return errors.New("simulated sync failure")
	}
	t.Cleanup(func() { pack.SyncDir = origSyncDir })

	// Finish should fail and poison the appender.
	_, _, err = a.Finish()
	require.Error(err)
	assert.Contains(err.Error(), "simulated sync failure")

	// The rename to the final path must have already succeeded: this proves
	// the injected failure fired on the post-rename directory sync, not a
	// pre-rename mkdir failure (which would have left no pack file behind).
	_, statErr := os.Stat(r.Path(packsDirName, packID[:2], packID+".mvpack"))
	require.NoError(statErr, "pack must be renamed to its final path despite the post-rename sync failure")

	// Restore SyncDir so we can test the poisoned state.
	pack.SyncDir = origSyncDir

	// Subsequent Add returns the sticky error.
	_, _, err = a.Add([]byte("more data"))
	require.Error(err)
	assert.Contains(err.Error(), "simulated sync failure")

	// Subsequent Finish also returns the sticky error.
	_, _, err = a.Finish()
	require.Error(err)
	assert.Contains(err.Error(), "simulated sync failure")

	// Abort is safe and does not panic.
	a.Abort()
}
