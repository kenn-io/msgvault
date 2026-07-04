package pack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeBlobID(t *testing.T) {
	// SHA-256("abc") is a published test vector (FIPS 180-2).
	id := ComputeBlobID([]byte("abc"))
	assert.Equal(t,
		"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		id.String())

	empty := ComputeBlobID(nil)
	assert.Equal(t,
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		empty.String())
}

func TestParseBlobID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	want := ComputeBlobID([]byte("abc"))
	got, err := ParseBlobID(want.String())
	require.NoError(err)
	assert.Equal(want, got)

	_, err = ParseBlobID("zz")
	require.Error(err)
	_, err = ParseBlobID(want.String() + "00")
	require.Error(err)
}
