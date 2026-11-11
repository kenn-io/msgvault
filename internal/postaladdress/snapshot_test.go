package postaladdress

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOriginalSnapshotRoundTrip(t *testing.T) {
	input := Input{AddressKind: "postal", StreetAddress: "  12 Example St\n  second line  ", ExtendedAddress: " Apt 42 ", PostOfficeBox: " Box 9 ", OriginalValue: " \tRaw\ntext\n ", ExtendedComponents: "[\"raw\"]", FreeText: "  自由  ", CountryCode: " ca "}
	encoded, err := EncodeOriginalInput(input)
	require := require.New(t)
	require.NoError(err)
	assert := assert.New(t)
	assert.Equal(encoded, strings.TrimSpace(encoded))
	got, ok, err := DecodeOriginalInput(encoded)
	require.NoError(err)
	require.True(ok)
	assert.Equal(input, got)
	// Callers editing known stored data decode before capturing the next raw draft.
	input, ok, err = DecodeOriginalInput(encoded)
	require.NoError(err)
	require.True(ok)
	encodedAgain, err := EncodeOriginalInput(input)
	require.NoError(err)
	got, ok, err = DecodeOriginalInput(encodedAgain)
	require.NoError(err)
	require.True(ok)
	assert.Equal(" \tRaw\ntext\n ", got.OriginalValue)
}

func TestSnapshotRejectsMalformedAndUnknownVersions(t *testing.T) {
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

func TestLiteralSnapshotTextIsRawInput(t *testing.T) {
	input := Input{AddressKind: "postal", StreetAddress: "12 Example Street", OriginalValue: "msgvault:postal-input:v1\n" + `{"schema":1,"input":{"address_kind":"postal","original_value":"literal payload"}}`}
	encoded, err := EncodeOriginalInput(input)
	require.NoError(t, err)
	decoded, ok, err := DecodeOriginalInput(encoded)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, input, decoded)
}
