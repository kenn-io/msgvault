package postaladdress

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestSnapshotRejectsMalformedAndUnknownVersions(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	_, err := EncodeOriginalInput(Input{OriginalValue: string([]byte{0xff})})
	req.Error(err)
	_, ok, err := DecodeOriginalInput("legacy original")
	req.NoError(err)
	check.False(ok)
	for _, raw := range []string{"msgvault:postal-input:v2\n{}", "msgvault:postal-input:v1\nnot json", "msgvault:postal-input:v1\n{}", "msgvault:postal-input:v1\nnull", `msgvault:postal-input:v1
{"schema":1,"input":{},"unexpected":true}`} {
		_, ok, err := DecodeOriginalInput(raw)
		check.False(ok, raw)
		check.Error(err, raw)
	}
}

func TestOriginalSnapshotProperty(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	input := Input{AddressKind: "postal", StreetAddress: "12 Example Street", OriginalValue: "msgvault:postal-input:v1\n" + `{"schema":1,"input":{"address_kind":"postal","original_value":"literal payload"}}`}
	encoded, err := EncodeOriginalInput(input)
	req.NoError(err)
	decoded, ok, err := DecodeOriginalInput(encoded)
	req.NoError(err)
	req.True(ok)
	check.Equal(input, decoded)
	hegel.Test(t, func(ht *hegel.T) {
		req := require.New(ht)
		check := assert.New(ht)
		input := drawPostalInput(ht)
		encoded, err := EncodeOriginalInput(input)
		req.NoError(err)
		decoded, ok, err := DecodeOriginalInput(encoded)
		req.NoError(err)
		req.True(ok)
		check.Equal(input, decoded)
	}, hegel.WithTestCases(300), hegel.WithDatabase(""))
}
