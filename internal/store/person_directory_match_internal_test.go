package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The verifier is only an indexed-hit check; contact thresholds remain at four runes.
func TestDirectoryLinearMatcherTokenParity(t *testing.T) {
	for _, tc := range []struct {
		token, term string
		quality     int
		match       bool
	}{
		{"alcie", "alice", 2, true}, {"abce", "abcd", 2, true}, {"ab1d", "abcd", 2, true},
		{"abc", "abd", 0, false},
		{"abcdef", "abcd", 1, true}, {"axcye", "abcde", 0, false},
	} {
		quality, ok := directoryCandidateQuality([]string{tc.token}, []string{tc.term})
		assert.Equal(t, tc.match, ok, "%q/%q", tc.token, tc.term)
		if ok {
			assert.Equal(t, tc.quality, quality)
		}
	}
}
