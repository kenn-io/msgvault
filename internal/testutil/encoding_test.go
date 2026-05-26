package testutil

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEncodedSamplesDefensiveCopy verifies that EncodedSamples returns a fresh
// copy each time, so mutations by one test don't affect other tests.
func TestEncodedSamplesDefensiveCopy(t *testing.T) {
	first := EncodedSamples()
	original := bytes.Clone(first.ShiftJIS_Konnichiwa)

	// Mutate the returned slice
	first.ShiftJIS_Konnichiwa[0] ^= 0xFF

	// A second call must return the original, unmodified bytes
	second := EncodedSamples()
	require.Equal(t, original, second.ShiftJIS_Konnichiwa, "EncodedSamples() returned mutated data")
}

// TestEncodedSamplesNonEmpty verifies all sample fields have content.
// This catches copy-paste errors where a field is defined but not initialized.
func TestEncodedSamplesNonEmpty(t *testing.T) {
	s := EncodedSamples()

	// Byte slice fields - verify non-empty
	byteFields := map[string][]byte{
		"ShiftJIS_Konnichiwa":     s.ShiftJIS_Konnichiwa,
		"GBK_Nihao":               s.GBK_Nihao,
		"Big5_Nihao":              s.Big5_Nihao,
		"EUCKR_Annyeong":          s.EUCKR_Annyeong,
		"Win1252_SmartQuoteRight": s.Win1252_SmartQuoteRight,
		"Win1252_EnDash":          s.Win1252_EnDash,
		"Win1252_EmDash":          s.Win1252_EmDash,
		"Win1252_DoubleQuotes":    s.Win1252_DoubleQuotes,
		"Win1252_Trademark":       s.Win1252_Trademark,
		"Win1252_Bullet":          s.Win1252_Bullet,
		"Win1252_Euro":            s.Win1252_Euro,
		"Latin1_OAcute":           s.Latin1_OAcute,
		"Latin1_CCedilla":         s.Latin1_CCedilla,
		"Latin1_UUmlaut":          s.Latin1_UUmlaut,
		"Latin1_NTilde":           s.Latin1_NTilde,
		"Latin1_Registered":       s.Latin1_Registered,
		"Latin1_Degree":           s.Latin1_Degree,
		"ShiftJIS_Long":           s.ShiftJIS_Long,
		"GBK_Long":                s.GBK_Long,
		"Big5_Long":               s.Big5_Long,
		"EUCKR_Long":              s.EUCKR_Long,
	}
	for name, data := range byteFields {
		assert.NotEmpty(t, data, "%s is empty", name)
	}

	// String fields - verify non-empty
	stringFields := map[string]string{
		"ShiftJIS_Long_UTF8": s.ShiftJIS_Long_UTF8,
		"GBK_Long_UTF8":      s.GBK_Long_UTF8,
		"Big5_Long_UTF8":     s.Big5_Long_UTF8,
		"EUCKR_Long_UTF8":    s.EUCKR_Long_UTF8,
	}
	for name, data := range stringFields {
		assert.NotEmpty(t, data, "%s is empty", name)
	}
}

// MAINTAINER NOTE: Do not add reflection-based "automatic" field iteration tests.
// The explicit field listing above is intentional - it's easy to read, easy to
// maintain, and catches real bugs. Reflection-based testing of this simple struct:
// - Adds significant complexity (handling all reflect.Kind types)
// - Requires tests for the test helpers themselves
// - Provides no practical benefit over explicit listing
//
// If you add a field to EncodedSamplesT, add it to the maps above. That's it.
