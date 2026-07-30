package vcard

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func FuzzDecodeMarshalDecode(f *testing.F) {
	for _, name := range []string{
		"v4-registry-smoke.vcf",
		"v4-rfc9554.vcf",
		"v3-apple-groups.vcf",
		"v21-quoted-printable.vcf",
		"unknown-extensions.vcf",
	} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		require.NoError(f, err)
		f.Add(data)
	}

	f.Fuzz(func(t *testing.T, input []byte) {
		first, err := DecodeWithOptions(bytes.NewReader(input), DecodeOptions{
			MaxPhysicalLineBytes: 1 << 20,
			MaxLogicalLineBytes:  1 << 20,
			MaxCards:             100,
			AllowV21:             true,
		})
		if err != nil {
			return
		}
		rendered, err := Marshal(first)
		require.NoError(t, err)
		second, err := Decode(bytes.NewReader(rendered))
		require.NoError(t, err)
		assert.Equal(t, first, second)
	})
}
