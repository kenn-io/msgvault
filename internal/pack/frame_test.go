package pack

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeFrameCompressible(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	raw := bytes.Repeat([]byte("msgvault backup substrate "), 4096)
	stored, compressed := encodeFrame(raw, DefaultZstdLevel)
	require.True(compressed, "repetitive text must compress")
	require.Less(len(stored), len(raw))

	got, err := decodeFrame(stored, true, uint64(len(raw)))
	require.NoError(err)
	assert.Equal(raw, got)
}

func TestEncodeFrameIncompressible(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	raw := make([]byte, 64*1024)
	_, err := rand.Read(raw)
	require.NoError(err)

	stored, compressed := encodeFrame(raw, DefaultZstdLevel)
	require.False(compressed, "random bytes must be stored raw")
	assert.Equal(raw, stored)

	got, err := decodeFrame(stored, false, uint64(len(raw)))
	require.NoError(err)
	assert.Equal(raw, got)
}

func TestEncodeFrameEmpty(t *testing.T) {
	stored, compressed := encodeFrame(nil, DefaultZstdLevel)
	require.False(t, compressed)
	got, err := decodeFrame(stored, false, 0)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestMinCompressionSavings(t *testing.T) {
	tests := []struct {
		name         string
		rawLen       int
		wantMinSaved int
	}{
		{"zero raw length floors to one", 0, 1},
		{"one byte floors to one", 1, 1},
		{"below one percent point", 33, 1},
		{"just above one percent point", 34, 2},
		{"just under three percent boundary", 99, 3},
		{"exactly three percent boundary", 100, 3},
		{"just over three percent boundary", 101, 4},
		{"large power of ten", 1000, 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantMinSaved, minCompressionSavings(tt.rawLen))
		})
	}
}

func TestMinCompressionSavingsRejectsJustUnderThreePercent(t *testing.T) {
	// rawLen=99: a stored length of 97 (a 2-byte saving) is 2.02%, just under
	// the 3% bar. The contract is "keep raw unless zstd saves at least 3%",
	// so encodeFrame's guard (len(c) > len(raw)-minSavings keeps raw) must
	// reject this saving rather than accept it via floor-division rounding.
	const rawLen = 99
	minSaved := minCompressionSavings(rawLen)
	justUnderStoredLen := rawLen - 2
	assert.Greater(t, justUnderStoredLen, rawLen-minSaved,
		"a 2-byte saving on 99 raw bytes must be stored raw, not compressed")
}

func TestDecodeFrameErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	_, err := decodeFrame([]byte("definitely not zstd"), true, 19)
	require.ErrorIs(err, ErrCorrupt, "corrupt zstd input")

	raw := bytes.Repeat([]byte("abc"), 1000)
	stored, compressed := encodeFrame(raw, DefaultZstdLevel)
	assert.True(compressed)
	_, err = decodeFrame(stored, true, uint64(len(raw))+1)
	require.ErrorIs(err, ErrCorrupt, "raw length mismatch must be corrupt")

	_, err = decodeFrame([]byte("xy"), false, 3)
	require.ErrorIs(err, ErrCorrupt, "uncompressed length mismatch must be corrupt")
}
