package vcard

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vcard/registry"
)

func TestConformanceCorpusRoundTripsWithoutSemanticLoss(t *testing.T) {
	paths, err := filepath.Glob("testdata/*.vcf")
	require.NoError(t, err)
	require.NotEmpty(t, paths)

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			require := require.New(t)
			input, err := os.ReadFile(path)
			require.NoError(err)
			assertCRLFOnly(t, input)

			first, err := Decode(bytes.NewReader(input))
			require.NoError(err)
			rendered, err := Marshal(first)
			require.NoError(err)
			second, err := Decode(bytes.NewReader(rendered))
			require.NoError(err)

			assert.Equal(t, first, second)
		})
	}
}

func TestLegacyBareParametersRoundTrip(t *testing.T) {
	tests := []struct {
		name       string
		parameter  string
		wantValues []ParameterValue
	}{
		{
			name:      "quoted value",
			parameter: `"HOME"`,
			wantValues: []ParameterValue{{
				Raw:      "HOME",
				Decoded:  "HOME",
				Quoted:   true,
				RawValid: true,
			}},
		},
		{
			name:      "comma-separated values",
			parameter: "HOME,VOICE",
			wantValues: []ParameterValue{
				{Raw: "HOME", Decoded: "HOME", RawValid: true},
				{Raw: "VOICE", Decoded: "VOICE", RawValid: true},
			},
		},
		{
			name:      "quoted delimiter",
			parameter: `"HOME,VOICE"`,
			wantValues: []ParameterValue{{
				Raw:      "HOME,VOICE",
				Decoded:  "HOME,VOICE",
				Quoted:   true,
				RawValid: true,
			}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			input := []byte("BEGIN:VCARD\r\nVERSION:2.1\r\nTEL;" +
				test.parameter + ":+12025550123\r\nEND:VCARD\r\n")

			first, err := Decode(bytes.NewReader(input))
			require.NoError(err)
			require.Len(first.Cards, 1)
			require.Len(first.Cards[0].Properties, 2)
			require.Len(first.Cards[0].Properties[1].Parameters, 1)
			parameter := first.Cards[0].Properties[1].Parameters[0]
			assert.True(parameter.Bare)
			assert.Equal(test.wantValues, parameter.Values)

			rendered, err := Marshal(first)
			require.NoError(err)
			assert.Equal(input, rendered)

			second, err := Decode(bytes.NewReader(rendered))
			require.NoError(err)
			assert.Equal(first, second)
		})
	}
}

func TestConformanceRegistrySmokeCoversEveryContentPropertyOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	input, err := os.ReadFile(filepath.Join("testdata", "v4-registry-smoke.vcf"))
	require.NoError(err)
	document, err := Decode(bytes.NewReader(input))
	require.NoError(err)
	require.Len(document.Cards, 1)
	require.NotEmpty(document.Cards[0].Properties)
	assert.Equal("VERSION", document.Cards[0].Properties[0].Name)

	counts := make(map[string]int)
	for _, property := range document.Cards[0].Properties {
		counts[strings.ToUpper(property.Name)]++
	}

	snapshot, err := registry.Load()
	require.NoError(err)
	for _, registered := range snapshot.Properties {
		handling, ok := PropertyHandling(registered.Name)
		require.True(ok, "missing handling for %s", registered.Name)
		if handling.Strategy == HandlingFraming {
			assert.Zero(counts[registered.Name], "%s belongs to framing", registered.Name)
			continue
		}
		assert.Equal(1, counts[registered.Name], "%s fixture occurrences", registered.Name)
		delete(counts, registered.Name)
	}
	assert.Empty(counts, "fixture contains unregistered content properties")
}

func assertCRLFOnly(tb testing.TB, input []byte) {
	tb.Helper()
	require.True(tb, bytes.Contains(input, []byte("\r\n")), "fixture has no CRLF")
	withoutCRLF := bytes.ReplaceAll(input, []byte("\r\n"), nil)
	assert.NotContains(tb, withoutCRLF, []byte{'\n'}, "fixture contains bare LF")
	assert.NotContains(tb, withoutCRLF, []byte{'\r'}, "fixture contains bare CR")
}
