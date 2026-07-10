package packer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/blobstore"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

type closeFailUnpackReader struct {
	unpackPackReader

	err error
}

func (r *closeFailUnpackReader) Close() error {
	return errors.Join(r.unpackPackReader.Close(), r.err)
}

func TestUnpackCloseFailureRetainsPackAuthority(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	content := []byte("close must precede metadata deletion")
	hash, _ := f.addBlob(content, maintenanceCanonical)
	packed, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	require.Equal(1, packed.PacksSealed)
	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(entry)
	packPath := filepath.Join(f.dir, "packs", entry.PackID[:2], entry.PackID+blobstore.PackExt)

	closeErr := errors.New("injected retained reader close failure")
	originalOpen := openUnpackPack
	openUnpackPack = func(path string) (unpackPackReader, error) {
		reader, err := blobstore.OpenMaintenancePack(path)
		if err != nil {
			return nil, err
		}
		return &closeFailUnpackReader{unpackPackReader: reader, err: closeErr}, nil
	}
	t.Cleanup(func() { openUnpackPack = originalOpen })

	stats, err := Unpack(context.Background(), f.store, f.dir)
	require.Error(err)
	require.ErrorIs(err, closeErr)
	assert.Contains(err.Error(), entry.PackID)
	assert.Equal(UnpackStats{}, stats)
	assert.FileExists(packPath)
	has, err := f.store.HasPackRecord(entry.PackID)
	require.NoError(err)
	assert.True(has)
	indexed, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	assert.NotNil(indexed)
	loosePath := filepath.Join(f.dir, filepath.FromSlash(maintenanceCanonical(hash)))
	loose, err := os.ReadFile(loosePath)
	require.NoError(err, "verified loose copy may remain after close failure")
	assert.Equal(content, loose)
}

func TestUnpackCancellationDuringPlanningWritesNothing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	firstHash, _ := f.addBlob([]byte("first planning blob"), maintenanceCanonical)
	secondHash, _ := f.addBlob([]byte("second planning blob"), maintenanceCanonical)
	packed, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	require.Equal(1, packed.PacksSealed)
	firstEntry, err := f.store.GetAttachmentPackEntry(firstHash)
	require.NoError(err)
	require.NotNil(firstEntry)
	packPath := filepath.Join(f.dir, "packs", firstEntry.PackID[:2], firstEntry.PackID+blobstore.PackExt)
	ctx := &cancelAfterErrContext{Context: context.Background(), cancelAfter: 4}

	stats, err := Unpack(ctx, f.store, f.dir)
	require.ErrorIs(err, context.Canceled)
	assert.Equal(UnpackStats{}, stats)
	assert.NoFileExists(filepath.Join(f.dir, filepath.FromSlash(maintenanceCanonical(firstHash))))
	assert.NoFileExists(filepath.Join(f.dir, filepath.FromSlash(maintenanceCanonical(secondHash))))
	assert.FileExists(packPath)
	count, err := f.store.CountPackIndexEntries(firstEntry.PackID)
	require.NoError(err)
	assert.Equal(int64(2), count)
}

func TestUnpackDurableFileFailureRetainsPackAuthority(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	content := []byte("file sync authority boundary")
	hash, _ := f.addBlob(content, maintenanceCanonical)
	packed, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	require.Equal(1, packed.PacksSealed)
	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(entry)
	packPath := filepath.Join(f.dir, "packs", entry.PackID[:2], entry.PackID+blobstore.PackExt)
	syncErr := errors.New("injected durable attachment file sync failure")
	originalStore := storeRestoredAttachment
	storeRestoredAttachment = func(string, *mime.Attachment) (string, error) {
		return "", syncErr
	}
	t.Cleanup(func() { storeRestoredAttachment = originalStore })

	stats, err := Unpack(context.Background(), f.store, f.dir)
	require.ErrorIs(err, syncErr)
	assert.Equal(UnpackStats{}, stats)
	assert.FileExists(packPath)
	indexed, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	assert.NotNil(indexed)

	storeRestoredAttachment = originalStore
	stats, err = Unpack(context.Background(), f.store, f.dir)
	require.NoError(err, "retry after durable file failure")
	assert.Equal(1, stats.PacksUnpacked)
	assert.NoFileExists(packPath)
}

func TestUnpackDurableParentFailureRetainsPackAuthority(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	content := []byte("parent sync authority boundary")
	hash, _ := f.addBlob(content, maintenanceCanonical)
	packed, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	require.Equal(1, packed.PacksSealed)
	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(entry)
	packPath := filepath.Join(f.dir, "packs", entry.PackID[:2], entry.PackID+blobstore.PackExt)
	resolvedDir, err := filepath.EvalSymlinks(f.dir)
	require.NoError(err)
	hashDir := filepath.Join(resolvedDir, hash[:2])
	syncErr := errors.New("injected durable attachment parent sync failure")
	originalSyncDir := pack.SyncDir
	pack.SyncDir = func(path string) error {
		if filepath.Clean(path) == filepath.Clean(hashDir) {
			return syncErr
		}
		return originalSyncDir(path)
	}
	t.Cleanup(func() { pack.SyncDir = originalSyncDir })

	stats, err := Unpack(context.Background(), f.store, f.dir)
	require.ErrorIs(err, syncErr)
	assert.Equal(UnpackStats{}, stats)
	assert.FileExists(packPath)
	indexed, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	assert.NotNil(indexed)
	assert.FileExists(filepath.Join(hashDir, hash), "durable loose residue is harmless while pack stays authoritative")

	pack.SyncDir = originalSyncDir
	stats, err = Unpack(context.Background(), f.store, f.dir)
	require.NoError(err, "retry after durable parent failure")
	assert.Equal(1, stats.PacksUnpacked)
	assert.NoFileExists(packPath)
}

func TestUnpackRechecksContextBeforeDroppingZeroLivePack(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	packID := "01hzy3v7q8r9s0t1a2b3c4d5e6"
	require.NoError(f.store.RecordPackedBlobs(store.PackRecord{
		PackID: packID, CreatedAt: time.Now().UTC(),
	}, nil))
	packsDir := filepath.Join(f.dir, "packs")
	path := filepath.Join(packsDir, packID[:2], packID+blobstore.PackExt)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(os.WriteFile(path, []byte("dead pack need not parse"), 0o600))
	ctx := &cancelAfterErrContext{Context: context.Background(), cancelAfter: 2}
	var stats UnpackStats

	err := unpackOne(ctx, f.store, f.dir, packsDir, packID, &stats)
	require.ErrorIs(err, context.Canceled)
	assert.Equal(UnpackStats{}, stats)
	assert.FileExists(path)
	has, err := f.store.HasPackRecord(packID)
	require.NoError(err)
	assert.True(has)
}

func TestUnpackRetriesAttachmentsBaseSyncAfterDirectoryResidue(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	content := []byte("base directory durability retry")
	hash, _ := f.addBlob(content, maintenanceCanonical)
	packed, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	require.Equal(1, packed.PacksSealed)
	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(entry)
	packPath := filepath.Join(f.dir, "packs", entry.PackID[:2], entry.PackID+blobstore.PackExt)
	resolvedDir, err := filepath.EvalSymlinks(f.dir)
	require.NoError(err)
	hashDir := filepath.Join(resolvedDir, hash[:2])
	_ = os.Remove(hashDir)
	syncErr := errors.New("injected attachments base sync failure")
	originalSyncDir := pack.SyncDir
	var baseSyncs int
	pack.SyncDir = func(path string) error {
		if filepath.Clean(path) == filepath.Clean(resolvedDir) {
			baseSyncs++
			if baseSyncs == 1 {
				return syncErr
			}
		}
		return originalSyncDir(path)
	}
	t.Cleanup(func() { pack.SyncDir = originalSyncDir })

	stats, err := Unpack(context.Background(), f.store, f.dir)
	require.ErrorIs(err, syncErr)
	assert.Equal(UnpackStats{}, stats)
	assert.Equal(1, baseSyncs)
	assert.DirExists(hashDir, "failed base sync may leave a directory residue")
	assert.FileExists(packPath)
	indexed, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	assert.NotNil(indexed)

	stats, err = Unpack(context.Background(), f.store, f.dir)
	require.NoError(err, "retry must resync the base despite the existing hash directory residue")
	assert.Equal(2, baseSyncs, "base durability must be retried unconditionally")
	assert.Equal(1, stats.PacksUnpacked)
	assert.NoFileExists(packPath)
}
