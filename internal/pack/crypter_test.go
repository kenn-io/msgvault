package pack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKey(b byte) [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = b
	}
	return k
}

func TestCrypterBlobRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, err := NewCrypter(testKey(1))
	require.NoError(err)

	frame := []byte("stored frame bytes")
	id := ComputeBlobID([]byte("raw content"))
	sealed, err := c.SealBlob(id, frame)
	require.NoError(err)
	assert.Len(sealed, len(frame)+24+16, "overhead is nonce(24)+tag(16)")

	got, err := c.OpenBlob(id, sealed)
	require.NoError(err)
	assert.Equal(frame, got)

	again, err := c.SealBlob(id, frame)
	require.NoError(err)
	assert.NotEqual(sealed, again, "random nonces: sealing twice must differ")
}

func TestCrypterRejections(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, err := NewCrypter(testKey(1))
	require.NoError(err)
	id := ComputeBlobID([]byte("raw"))
	sealed, err := c.SealBlob(id, []byte("frame"))
	require.NoError(err)

	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0x01
	_, err = c.OpenBlob(id, tampered)
	assert.ErrorIs(err, ErrDecrypt, "tampered ciphertext must fail") //nolint:testifylint // independent non-blocking check

	otherID := ComputeBlobID([]byte("different"))
	_, err = c.OpenBlob(otherID, sealed)
	assert.ErrorIs(err, ErrDecrypt, "wrong blob ID must fail") //nolint:testifylint // independent non-blocking check

	wrongKey, err := NewCrypter(testKey(2))
	require.NoError(err)
	_, err = wrongKey.OpenBlob(id, sealed)
	assert.ErrorIs(err, ErrDecrypt, "wrong key must fail") //nolint:testifylint // independent non-blocking check

	_, err = c.OpenBlob(id, sealed[:10])
	assert.ErrorIs(err, ErrDecrypt, "too-short input must fail")
}

func TestCrypterObjectRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, err := NewCrypter(testKey(3))
	require.NoError(err)

	sealed, err := c.SealObject("pack-footer", "01hzxyzpackid00000000000000", []byte("footer"))
	require.NoError(err)

	got, err := c.OpenObject("pack-footer", "01hzxyzpackid00000000000000", sealed)
	require.NoError(err)
	assert.Equal([]byte("footer"), got)

	_, err = c.OpenObject("pack-footer", "01other0000000000000000000", sealed)
	assert.ErrorIs(err, ErrDecrypt, "different object ID must fail") //nolint:testifylint // independent non-blocking check
	_, err = c.OpenObject("manifest", "01hzxyzpackid00000000000000", sealed)
	assert.ErrorIs(err, ErrDecrypt, "different role must fail")
}
