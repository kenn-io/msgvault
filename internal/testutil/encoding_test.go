package testutil

import (
	"bytes"
	"testing"

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
