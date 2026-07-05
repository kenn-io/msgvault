package pack

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildTestPack writes a pack with the given blobs and returns its final path
// and entries. crypter may be nil for a plain pack.
func buildTestPack(t *testing.T, blobs [][]byte,
	crypter *Crypter) (string, []Entry) {
	t.Helper()
	dir := t.TempDir()
	w, err := NewWriter(dir, WriterOptions{Crypter: crypter})
	require.NoError(t, err)
	for _, b := range blobs {
		_, err := w.Append(b)
		require.NoError(t, err)
	}
	final := filepath.Join(dir, w.ID()+".mvpack")
	entries, err := w.Seal(final)
	require.NoError(t, err)
	return final, entries
}

func testBlobs(t *testing.T) [][]byte {
	t.Helper()
	random := make([]byte, 32*1024)
	_, err := rand.Read(random)
	require.NoError(t, err)
	return [][]byte{
		bytes.Repeat([]byte("compressible text "), 2000),
		random,
		{},
		[]byte("small"),
	}
}

func TestReaderRoundTripPlain(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	blobs := testBlobs(t)
	path, wrote := buildTestPack(t, blobs, nil)

	r, err := OpenReader(path, nil)
	require.NoError(err)
	defer func() { _ = r.Close() }()

	assert.Equal(filepath.Base(path), r.ID()+".mvpack")
	require.Equal(wrote, r.Entries())
	for i, e := range r.Entries() {
		got, err := r.ReadBlob(e)
		require.NoError(err)
		assert.Equal(blobs[i], got, "blob %d", i)
		require.NoError(r.VerifyStored(e))
	}
}

func TestReaderHeaderValidation(t *testing.T) {
	path, _ := buildTestPack(t, testBlobs(t), nil)
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	writeVariant := func(mutate func([]byte)) string {
		v := append([]byte(nil), data...)
		mutate(v)
		p := filepath.Join(t.TempDir(), NewPackID()+".mvpack")
		require.NoError(t, os.WriteFile(p, v, 0o600))
		return p
	}

	_, err = OpenReader(writeVariant(func(b []byte) { b[0] = 'X' }), nil)
	require.ErrorIs(t, err, ErrBadMagic)

	_, err = OpenReader(writeVariant(func(b []byte) { b[4] = 99 }), nil)
	require.ErrorIs(t, err, ErrUnsupportedVersion)
}

func TestReaderBlobCorruption(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path, entries := buildTestPack(t, testBlobs(t), nil)
	data, err := os.ReadFile(path)
	require.NoError(err)

	// Flip one byte inside the first blob's stored bytes. The footer is
	// untouched, so Entries parses; the CRC catches the damage.
	data[entries[0].Offset] ^= 0x01
	corrupted := filepath.Join(t.TempDir(), filepath.Base(path))
	require.NoError(os.WriteFile(corrupted, data, 0o600))

	r, err := OpenReader(corrupted, nil)
	require.NoError(err)
	defer func() { _ = r.Close() }()
	_, err = r.ReadBlob(r.Entries()[0])
	require.ErrorIs(err, ErrCorrupt)
	require.ErrorIs(r.VerifyStored(r.Entries()[0]), ErrCorrupt)

	got, err := r.ReadBlob(r.Entries()[3])
	require.NoError(err, "other blobs remain readable")
	assert.Equal([]byte("small"), got)
}

func TestReaderRoundTripLargePack(t *testing.T) {
	// Build a several-MB pack (much larger than the footer itself) and
	// confirm opening and reading it back is still correct.
	require := require.New(t)
	assert := assert.New(t)

	var blobs [][]byte
	for range 8 {
		b := make([]byte, 512*1024)
		_, err := rand.Read(b)
		require.NoError(err)
		blobs = append(blobs, b)
	}
	path, wrote := buildTestPack(t, blobs, nil)

	info, err := os.Stat(path)
	require.NoError(err)
	require.Greater(info.Size(), int64(4<<20), "fixture must be several MB")

	r, err := OpenReader(path, nil)
	require.NoError(err)
	defer func() { _ = r.Close() }()

	require.Equal(wrote, r.Entries())
	for i, e := range r.Entries() {
		got, err := r.ReadBlob(e)
		require.NoError(err)
		assert.Equal(blobs[i], got, "blob %d", i)
	}
}

func TestReaderRejectsForgedHugeRawLen(t *testing.T) {
	// A forger who can rewrite pack bytes can also recompute the plain
	// trailer's SHA-256 over the rewritten footer region, so the footer opens
	// cleanly and only the entry's RawLen is a lie. decodeFrame must reject an
	// absurd RawLen before trusting it to size a preallocation, rather than
	// panicking with "makeslice: cap out of range" or attempting a
	// terabyte-scale allocation.
	require := require.New(t)
	compressible := bytes.Repeat([]byte("forge me some zstd bytes "), 4096)
	path, entries := buildTestPack(t, [][]byte{compressible}, nil)
	require.Equal(BlobCompressed, entries[0].Flags,
		"fixture must compress so decodeFrame reaches the zstd path")

	forged := entries[0]
	forged.RawLen = 1 << 50

	data, err := os.ReadFile(path)
	require.NoError(err)
	footerStart := int(entries[0].Offset + entries[0].StoredLen)
	rebuilt := append([]byte{}, data[:footerStart]...)
	rebuilt = append(rebuilt, appendPlainTrailer(encodeFooterRegion([]Entry{forged}))...)
	forgedPath := filepath.Join(t.TempDir(), filepath.Base(path))
	require.NoError(os.WriteFile(forgedPath, rebuilt, 0o600))

	r, err := OpenReader(forgedPath, nil)
	require.NoError(err, "footer is well-formed; only the entry's RawLen is forged")
	defer func() { _ = r.Close() }()

	_, err = r.ReadBlob(r.Entries()[0])
	require.ErrorIs(err, ErrCorrupt)
}

func TestReaderRejectsEncryptedFlagInPlainPack(t *testing.T) {
	// An entry flagged BlobEncrypted inside a pack whose trailer is plain is
	// structurally corrupt: the pack-level flag and the entry-level flag
	// disagree about whether the blob was sealed.
	require := require.New(t)
	blobs := [][]byte{[]byte("first"), []byte("second")}
	path, entries := buildTestPack(t, blobs, nil)

	flagged := append([]Entry{}, entries...)
	flagged[0].Flags |= BlobEncrypted

	data, err := os.ReadFile(path)
	require.NoError(err)
	footerStart := int(entries[len(entries)-1].Offset + entries[len(entries)-1].StoredLen)
	forged := append([]byte{}, data[:footerStart]...)
	forged = append(forged, appendPlainTrailer(encodeFooterRegion(flagged))...)
	forgedPath := filepath.Join(t.TempDir(), filepath.Base(path))
	require.NoError(os.WriteFile(forgedPath, forged, 0o600))

	r, err := OpenReader(forgedPath, nil)
	require.NoError(err)
	defer func() { _ = r.Close() }()
	_, err = r.ReadBlob(r.Entries()[0])
	require.ErrorIs(err, ErrCorrupt)
}

func TestReaderBlobHashMismatch(t *testing.T) {
	require := require.New(t)
	// A stored frame whose bytes are internally consistent (CRC recomputed to
	// match) but whose content does not hash to the entry's BlobID must fail
	// with ErrBlobMismatch. Build it by lying to the footer: swap two entries'
	// IDs after writing.
	blobs := [][]byte{[]byte("first"), []byte("second")}
	path, entries := buildTestPack(t, blobs, nil)

	swapped := []Entry{entries[0], entries[1]}
	swapped[0].ID, swapped[1].ID = swapped[1].ID, swapped[0].ID
	data, err := os.ReadFile(path)
	require.NoError(err)
	footerStart := int(entries[1].Offset + entries[1].StoredLen)
	forged := append([]byte{}, data[:footerStart]...)
	forged = append(forged,
		appendPlainTrailer(encodeFooterRegion(swapped))...)
	forgedPath := filepath.Join(t.TempDir(), filepath.Base(path))
	require.NoError(os.WriteFile(forgedPath, forged, 0o600))

	r, err := OpenReader(forgedPath, nil)
	require.NoError(err)
	defer func() { _ = r.Close() }()
	_, err = r.ReadBlob(r.Entries()[0])
	require.ErrorIs(err, ErrBlobMismatch)
}
