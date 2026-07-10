package packer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/blobstore"
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
