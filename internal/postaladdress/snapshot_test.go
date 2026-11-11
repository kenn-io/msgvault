package postaladdress

import (
	"testing"

	testifyassert "github.com/stretchr/testify/assert"
	testifyrequire "github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestSnapshotRejectsMalformedAndUnknownVersions(t *testing.T) {
	require := testifyrequire.New(t)
	assert := testifyassert.New(t)
	_, err := EncodeOriginalInput(Input{OriginalValue: string([]byte{0xff})})
	require.Error(err)
	_, ok, err := DecodeOriginalInput("legacy original")
	require.NoError(err)
	assert.False(ok)
	for _, raw := range []string{"msgvault:postal-input:v2\n{}", "msgvault:postal-input:v1\nnot json", "msgvault:postal-input:v1\n{}", "msgvault:postal-input:v1\nnull", `msgvault:postal-input:v1
{"schema":1,"input":{},"unexpected":true}`} {
		_, ok, err := DecodeOriginalInput(raw)
		assert.False(ok, raw)
		assert.Error(err, raw)
	}
}

func TestOriginalSnapshotProperty(t *testing.T) {
	require := testifyrequire.New(t)
	assert := testifyassert.New(t)
	input := Input{AddressKind: "postal", StreetAddress: "12 Example Street", OriginalValue: "msgvault:postal-input:v1\n" + `{"schema":1,"input":{"address_kind":"postal","original_value":"literal payload"}}`}
	encoded, err := EncodeOriginalInput(input)
	require.NoError(err)
	decoded, ok, err := DecodeOriginalInput(encoded)
	require.NoError(err)
	require.True(ok)
	assert.Equal(input, decoded)
	hegel.Test(t, func(ht *hegel.T) {
		require := testifyrequire.New(ht)
		assert := testifyassert.New(ht)
		input := drawPostalInput(ht)
		encoded, err := EncodeOriginalInput(input)
		require.NoError(err)
		decoded, ok, err := DecodeOriginalInput(encoded)
		require.NoError(err)
		require.True(ok)
		assert.Equal(input, decoded)
	}, hegel.WithTestCases(300), hegel.WithDatabase(""))
}
