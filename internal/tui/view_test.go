package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// assertHighlight checks that applyHighlight produces the expected plain text
// (after stripping ANSI) and validates ANSI behavior based on wantANSI:
// - wantANSI=true: output must contain ANSI escapes and differ from input
// - wantANSI=false: output must be unchanged from input.
func assertHighlight(t *testing.T, text string, terms []string, wantANSI bool) {
	t.Helper()
	result := applyHighlight(text, terms)
	stripped := stripANSI(result)

	// Content integrity check
	assert.Equal(t, text, stripped, "text content mismatch")

	// ANSI/change check
	if wantANSI {
		assert.NotEqual(t, text, result, "expected highlighting (ANSI) but output was unchanged")
		assert.Contains(t, result, ansiStart, "expected output to contain ANSI start sequence")
	} else {
		assert.Equal(t, text, result, "expected unchanged output")
	}
}

func TestApplyHighlight(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		terms    []string
		wantANSI bool
	}{
		{"no terms", "hello world", nil, false},
		{"single match", "hello world", []string{"world"}, true},
		{"case insensitive", "Hello World", []string{"hello"}, true},
		{"multiple terms", "hello world foo", []string{"hello", "foo"}, true},
		{"overlapping matches", "abcdef", []string{"abcd", "cdef"}, true},
		{"adjacent matches", "aabb", []string{"aa", "bb"}, true},
		{"nested matches", "abcdef", []string{"abcdef", "cd"}, true},
		{"no match", "hello world", []string{"xyz"}, false},
		{"unicode text", "café résumé", []string{"café"}, true},
		{"unicode case folding", "Ünïcödé", []string{"ünïcödé"}, true},
		{"empty text", "", []string{"hello"}, false},
		{"empty term filtered", "hello", []string{""}, false},
		{"CJK characters", "hello 世界 world", []string{"世界"}, true},
		{"repeated matches", "ababab", []string{"ab"}, true},
	}

	forceColorProfile(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertHighlight(t, tt.text, tt.terms, tt.wantANSI)
		})
	}
}

func TestApplyHighlightPreservesExistingANSISequences(t *testing.T) {
	assert := assert.New(t)
	forceColorProfile(t)
	text := "\x1b[1mHeading\x1b[0m"

	escapeOnlyResult := applyHighlight(text, []string{"m"})
	assert.Equal(text, escapeOnlyResult)

	result := applyHighlight(text, []string{"heading"})

	assert.Equal("Heading", stripANSI(result))
	assert.Contains(result, "\x1b[1m")
	assert.Contains(result, "\x1b[0m")
	assert.NotEqual(text, result)
}
