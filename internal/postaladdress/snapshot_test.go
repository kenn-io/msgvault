package postaladdress

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestSnapshotRejectsMalformedAndUnknownVersions(t *testing.T) {
	_, err := EncodeOriginalInput(Input{OriginalValue: string([]byte{0xff})})
	require.Error(t, err)
	_, ok, err := DecodeOriginalInput("legacy original")
	require.NoError(t, err)
	assert.False(t, ok)
	for _, raw := range []string{"msgvault:postal-input:v2\n{}", "msgvault:postal-input:v1\nnot json", "msgvault:postal-input:v1\n{}", "msgvault:postal-input:v1\nnull", `msgvault:postal-input:v1
{"schema":1,"input":{},"unexpected":true}`} {
		_, ok, err := DecodeOriginalInput(raw)
		assert.False(t, ok, raw)
		assert.Error(t, err, raw)
	}
}

func TestOriginalSnapshotProperty(t *testing.T) {
	input := Input{AddressKind: "postal", StreetAddress: "12 Example Street", OriginalValue: "msgvault:postal-input:v1\n" + `{"schema":1,"input":{"address_kind":"postal","original_value":"literal payload"}}`}
	encoded, err := EncodeOriginalInput(input)
	require.NoError(t, err)
	decoded, ok, err := DecodeOriginalInput(encoded)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, input, decoded)
	hegel.Test(t, func(ht *hegel.T) {
		input := drawPostalInput(ht)
		encoded, err := EncodeOriginalInput(input)
		require.NoError(ht, err)
		decoded, ok, err := DecodeOriginalInput(encoded)
		require.NoError(ht, err)
		require.True(ht, ok)
		assert.Equal(ht, input, decoded)
	}, hegel.WithTestCases(300), hegel.WithDatabase(""))
}
