package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/pack"
)

func testIndexEntries(t *testing.T) []IndexEntry {
	t.Helper()
	p1, p2 := pack.NewPackID(), pack.NewPackID()
	return []IndexEntry{
		{Blob: pack.ComputeBlobID([]byte("beta")), PackID: p2, Offset: 6, StoredLen: 100, Flags: pack.BlobCompressed},
		{Blob: pack.ComputeBlobID([]byte("alpha")), PackID: p1, Offset: 106, StoredLen: 5, Flags: 0},
		{Blob: pack.ComputeBlobID([]byte("gamma")), PackID: p1, Offset: 111, StoredLen: 42, Flags: pack.BlobCompressed},
	}
}

func TestIndexRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	entries := testIndexEntries(t)
	data, err := EncodeIndex(entries)
	require.NoError(err)
	got, err := DecodeIndex(data)
	require.NoError(err)
	require.Len(got, len(entries))
	for i := 1; i < len(got); i++ {
		assert.Less(got[i-1].Blob.String(), got[i].Blob.String(), "sorted by blob id")
	}
	byBlob := map[pack.BlobID]IndexEntry{}
	for _, e := range got {
		byBlob[e.Blob] = e
	}
	for _, want := range entries {
		g, ok := byBlob[want.Blob]
		require.True(ok, want.Blob.String())
		assert.Equal(want.PackID, g.PackID)
		assert.Equal(want.Offset, g.Offset)
		assert.Equal(want.StoredLen, g.StoredLen)
		assert.Equal(want.Flags, g.Flags)
	}
}

// TestEncodeIndexRejectsDuplicateBlobIDs pins the fix making EncodeIndex
// self-consistent with DecodeIndex, which already rejects non-strictly-
// increasing adjacent blob IDs. Without this check, EncodeIndex would sort
// two entries sharing a Blob ID next to each other and publish an index that
// DecodeIndex can never load.
func TestEncodeIndexRejectsDuplicateBlobIDs(t *testing.T) {
	require := require.New(t)
	dup := pack.ComputeBlobID([]byte("dup"))
	p1, p2 := pack.NewPackID(), pack.NewPackID()
	_, err := EncodeIndex([]IndexEntry{
		{Blob: dup, PackID: p1, Offset: 0, StoredLen: 1},
		{Blob: dup, PackID: p2, Offset: 1, StoredLen: 1},
	})
	require.Error(err)
	require.Contains(err.Error(), "duplicate blob id")
}

func TestIndexEmptyRoundTrip(t *testing.T) {
	require := require.New(t)
	data, err := EncodeIndex(nil)
	require.NoError(err)
	got, err := DecodeIndex(data)
	require.NoError(err)
	require.Empty(got)
}

func TestDecodeIndexRejectsDamage(t *testing.T) {
	require := require.New(t)
	data, err := EncodeIndex(testIndexEntries(t))
	require.NoError(err)

	cases := map[string]func([]byte) []byte{
		"flip magic":     func(b []byte) []byte { c := append([]byte{}, b...); c[0] ^= 0xff; return c },
		"flip version":   func(b []byte) []byte { c := append([]byte{}, b...); c[4] ^= 0xff; return c },
		"flip body byte": func(b []byte) []byte { c := append([]byte{}, b...); c[len(c)/2] ^= 0x01; return c },
		"truncate":       func(b []byte) []byte { return append([]byte{}, b[:len(b)-3]...) },
		"empty":          func([]byte) []byte { return nil },
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			_, decodeErr := DecodeIndex(damage(data))
			require.Error(decodeErr)
		})
	}
}

func TestWriteAndLoadBlobIndex(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	first := testIndexEntries(t)[:2]
	second := testIndexEntries(t)[2:]
	id1, err := r.WriteIndex(first)
	require.NoError(err)
	assert.True(pack.IsValidPackID(id1))
	_, err = r.WriteIndex(second)
	require.NoError(err)

	m, err := r.LoadBlobIndex()
	require.NoError(err)
	assert.Len(m, 3)
	for _, e := range append(first, second...) {
		assert.Equal(e.PackID, m[e.Blob].PackID)
	}
}
