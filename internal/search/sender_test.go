package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSenderEscapeLike(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"hello", "hello"}, {"", ""}, {"100%", `100\%`}, {"in_box", `in\_box`}, {`path\raw`, `path\\raw`}, {`%_\`, `\%\_\\`},
	} {
		t.Run(tc.input, func(t *testing.T) { assert.Equal(t, tc.want, escapeLike(tc.input)) })
	}
}
