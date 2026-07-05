package pack

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncryptedPackRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, err := NewCrypter(testKey(9))
	require.NoError(err)
	blobs := testBlobs(t)
	path, wrote := buildTestPack(t, blobs, c)

	r, err := OpenReader(path, c)
	require.NoError(err)
	defer func() { _ = r.Close() }()
	require.Equal(wrote, r.Entries())
	for i, e := range r.Entries() {
		assert.NotZero(e.Flags&BlobEncrypted, "blob %d carries the encrypted flag", i)
		got, err := r.ReadBlob(e)
		require.NoError(err)
		assert.Equal(blobs[i], got, "blob %d", i)
		assert.NoError(r.VerifyStored(e), "CRC verification needs no key")
	}
}

func TestEncryptedPackAccessControl(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, err := NewCrypter(testKey(9))
	require.NoError(err)
	path, _ := buildTestPack(t, [][]byte{[]byte("secret")}, c)

	_, err = OpenReader(path, nil)
	assert.ErrorIs(err, ErrEncrypted) //nolint:testifylint // independent non-blocking check

	wrong, err := NewCrypter(testKey(10))
	require.NoError(err)
	_, err = OpenReader(path, wrong)
	assert.ErrorIs(err, ErrDecrypt) //nolint:testifylint // independent non-blocking check

	// Renaming an encrypted pack breaks the footer AAD (pack-swap detection).
	renamed := filepath.Join(filepath.Dir(path), NewPackID()+".mvpack")
	require.NoError(os.Rename(path, renamed))
	_, err = OpenReader(renamed, c)
	assert.ErrorIs(err, ErrDecrypt)
}

func TestPlainPackIgnoresCrypter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, err := NewCrypter(testKey(9))
	require.NoError(err)
	blobs := [][]byte{[]byte("public")}
	path, _ := buildTestPack(t, blobs, nil)

	r, err := OpenReader(path, c)
	require.NoError(err)
	defer func() { _ = r.Close() }()
	got, err := r.ReadBlob(r.Entries()[0])
	require.NoError(err)
	assert.Equal(blobs[0], got)
}
