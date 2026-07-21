package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFlattenSnippet(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain text unchanged", "Quarterly planning notes", "Quarterly planning notes"},
		{"leading heading", "### Architecture\nMonorepo vs. two repos", "Architecture Monorepo vs. two repos"},
		{"mid-string heading after join", "sync When: 09:00 ### Architecture: Monorepo", "sync When: 09:00 Architecture: Monorepo"},
		{"issue reference preserved", "someuser opened a new pull request, #50362", "someuser opened a new pull request, #50362"},
		{"hashtag preserved", "tagged #launch in the notes", "tagged #launch in the notes"},
		{"bullet list", "- keep module separate\n- draw a clear boundary", "keep module separate draw a clear boundary"},
		{"numbered list", "1. kickoff talk\n2) judging", "kickoff talk judging"},
		{"bold and code stripped", "**Consensus:** keep `alpha module` separate", "Consensus: keep alpha module separate"},
		{"whitespace collapsed", "line one\n\n\tline two", "line one line two"},
		{"dunder identifier unchanged", "call __init__ on the object", "call __init__ on the object"},
		{"exponent operator unchanged", "compute 2 ** 3 before summing", "compute 2 ** 3 before summing"},
		{"paired backticks stripped", "run `code` now", "run code now"},
		{"paired bold stripped", "**bold** text", "bold text"},
		{"unpaired bold left alone", "keep ** unmatched", "keep ** unmatched"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, FlattenSnippet(tt.in))
		})
	}
}
