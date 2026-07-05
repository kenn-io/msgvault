package pack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testEntries() []Entry {
	return []Entry{
		{ID: ComputeBlobID([]byte("a")), Offset: 6, StoredLen: 10, RawLen: 20,
			Flags: BlobCompressed, CRC32C: 0xDEADBEEF},
		{ID: ComputeBlobID([]byte("b")), Offset: 16, StoredLen: 4, RawLen: 4,
			Flags: 0, CRC32C: 1},
	}
}

func TestFooterRegionRoundTrip(t *testing.T) {
	entries := testEntries()
	region := encodeFooterRegion(entries)
	assert.Len(t, region, 4+2*61, "count(u32) plus 61 bytes per entry")

	got, err := parseFooterRegion(region, 20)
	require.NoError(t, err)
	assert.Equal(t, entries, got)
}

func TestFooterRegionEmpty(t *testing.T) {
	region := encodeFooterRegion(nil)
	got, err := parseFooterRegion(region, 6)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestParseFooterRegionRejects(t *testing.T) {
	assert := assert.New(t)
	region := encodeFooterRegion(testEntries())

	_, err := parseFooterRegion(region[:len(region)-1], 20)
	assert.ErrorIs(err, ErrCorrupt, "count/length mismatch") //nolint:testifylint // independent non-blocking check

	_, err = parseFooterRegion(region[:3], 20)
	assert.ErrorIs(err, ErrCorrupt, "shorter than count field") //nolint:testifylint // independent non-blocking check

	_, err = parseFooterRegion(region, 15)
	assert.ErrorIs(err, ErrCorrupt, "entry extends past footer start") //nolint:testifylint // independent non-blocking check

	bad := encodeFooterRegion([]Entry{{Offset: 2, StoredLen: 1, RawLen: 1}})
	_, err = parseFooterRegion(bad, 20)
	assert.ErrorIs(err, ErrCorrupt, "entry offset inside header")
}

// TestParseFooterRegionRejectsHugeStoredLen pins the fix bounding StoredLen
// against maxStoredLen during footer validation, before readStored ever gets
// a chance to preallocate a buffer of that size. footerStart is set far
// beyond the doctored entry's [offset, offset+StoredLen) span so the huge
// StoredLen is caught by the dedicated bound check rather than incidentally
// by the existing footerStart span check.
func TestParseFooterRegionRejectsHugeStoredLen(t *testing.T) {
	huge := encodeFooterRegion([]Entry{
		{ID: ComputeBlobID([]byte("x")), Offset: headerSize, StoredLen: 1 << 40, RawLen: 1},
	})
	_, err := parseFooterRegion(huge, 1<<50)
	assert.ErrorIs(t, err, ErrCorrupt, "StoredLen beyond maxStoredLen must be rejected")
}

func TestPlainTrailerRoundTrip(t *testing.T) {
	region := encodeFooterRegion(testEntries())
	file := append([]byte("MVPK\x01\x00padpadpadpadpadpad"), appendPlainTrailer(region)...)

	got, err := extractPlainFooterRegion(file)
	require.NoError(t, err)
	assert.Equal(t, region, got)
}

func TestPlainTrailerRejects(t *testing.T) {
	assert := assert.New(t)
	region := encodeFooterRegion(testEntries())
	good := append([]byte("MVPK\x01\x00"), appendPlainTrailer(region)...)

	corrupt := append([]byte(nil), good...)
	corrupt[10] ^= 0x01 // flip a byte inside the footer region
	_, err := extractPlainFooterRegion(corrupt)
	assert.ErrorIs(err, ErrChecksum) //nolint:testifylint // independent non-blocking check

	noMagic := append([]byte(nil), good...)
	noMagic[len(noMagic)-1] ^= 0x01
	_, err = extractPlainFooterRegion(noMagic)
	assert.ErrorIs(err, ErrBadMagic) //nolint:testifylint // independent non-blocking check

	_, err = extractPlainFooterRegion(good[:20])
	assert.ErrorIs(err, ErrTruncated) //nolint:testifylint // independent non-blocking check

	huge := append([]byte(nil), good...)
	huge[len(huge)-40] = 0xFF // inflate footer_len beyond the file size
	huge[len(huge)-39] = 0xFF
	huge[len(huge)-38] = 0xFF
	huge[len(huge)-37] = 0x7F
	_, err = extractPlainFooterRegion(huge)
	assert.Error(err, "absurd footer_len must fail before allocation")
}

func TestEncryptedTrailerRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	c, err := NewCrypter(testKey(7))
	require.NoError(err)
	packID := NewPackID()
	region := encodeFooterRegion(testEntries())
	sealed, err := c.SealObject("pack-footer", packID, region)
	require.NoError(err)

	body := []byte("MVPK\x01\x01somestoredframes")
	file := append(append([]byte(nil), body...),
		appendEncryptedTrailer(sealed, uint64(len(body)))...)

	gotSealed, gotOff, err := extractEncryptedFooter(file)
	require.NoError(err)
	assert.Equal(uint64(len(body)), gotOff)
	gotRegion, err := c.OpenObject("pack-footer", packID, gotSealed)
	require.NoError(err)
	assert.Equal(region, gotRegion)
}

func TestEncryptedTrailerRejects(t *testing.T) {
	assert := assert.New(t)

	_, _, err := extractEncryptedFooter([]byte("tiny"))
	assert.ErrorIs(err, ErrTruncated) //nolint:testifylint // independent non-blocking check

	body := []byte("MVPK\x01\x01data")
	file := append(append([]byte(nil), body...),
		appendEncryptedTrailer([]byte("sealedfooterbytes"), uint64(len(body)))...)

	badMagic := append([]byte(nil), file...)
	badMagic[len(badMagic)-1] ^= 0x01
	_, _, err = extractEncryptedFooter(badMagic)
	assert.ErrorIs(err, ErrBadMagic) //nolint:testifylint // independent non-blocking check

	badOff := append([]byte(nil), file...)
	badOff[len(badOff)-20] = 0xFF // footer_offset far beyond file size
	badOff[len(badOff)-19] = 0xFF
	_, _, err = extractEncryptedFooter(badOff)
	assert.ErrorIs(err, ErrTruncated)
}
