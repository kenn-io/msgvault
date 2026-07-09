package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRecordAndGetPackedBlobs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	rec := store.PackRecord{
		PackID:      "01hzy3v7q8r9s0t1u2v3w4x5y6",
		EntryCount:  2,
		StoredBytes: 4096,
		CreatedAt:   time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	entries := []store.PackIndexEntry{
		{BlobHash: "aa11223344556677889900aabbccddeeff00112233445566778899aabbccddee",
			PackID: rec.PackID, Offset: 6, StoredLen: 2048, RawLen: 4000, Flags: 1, CRC32C: 4022250974},
		{BlobHash: "bb11223344556677889900aabbccddeeff00112233445566778899aabbccddee",
			PackID: rec.PackID, Offset: 2054, StoredLen: 2048, RawLen: 2048, Flags: 0, CRC32C: 1},
	}
	require.NoError(st.RecordPackedBlobs(rec, entries))

	got, err := st.GetAttachmentPackEntry(entries[0].BlobHash)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(entries[0], *got)

	// CRC32C above int32 max must round-trip on both backends (BIGINT column).
	assert.Equal(uint32(4022250974), got.CRC32C)

	missing, err := st.GetAttachmentPackEntry(
		"cc11223344556677889900aabbccddeeff00112233445566778899aabbccddee")
	require.NoError(err)
	assert.Nil(missing)

	// Idempotent re-record (crash-reconciliation re-runs adoption).
	require.NoError(st.RecordPackedBlobs(rec, entries))
}
