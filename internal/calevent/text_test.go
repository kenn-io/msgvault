package calevent

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestSnippet(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "trims short input",
			body: "  Team sync notes  ",
			want: "Team sync notes",
		},
		{
			name: "trims before checking exact byte limit",
			body: " \t" + strings.Repeat("\u00e9", 100) + " \n",
			want: strings.Repeat("\u00e9", 100),
		},
		{
			name: "truncates long ASCII",
			body: strings.Repeat("a", 201),
			want: strings.Repeat("a", 200),
		},
		{
			name: "caps multibyte text by bytes",
			body: strings.Repeat("\u00e9", 101),
			want: strings.Repeat("\u00e9", 100),
		},
		{
			name: "keeps complete rune ending at byte 200",
			body: strings.Repeat("a", 197) + "\u2014" + "tail",
			want: strings.Repeat("a", 197) + "\u2014",
		},
		{
			name: "drops em dash crossing byte 200",
			body: strings.Repeat("a", 198) + "\u2014" + "tail",
			want: strings.Repeat("a", 198),
		},
		{
			name: "drops two-byte rune crossing byte 200",
			body: strings.Repeat("a", 199) + "\u00e9" + "tail",
			want: strings.Repeat("a", 199),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			got := Snippet(tt.body)

			assert.Equal(tt.want, got)
			assert.LessOrEqual(len(got), 200)
			assert.True(utf8.ValidString(got))
		})
	}
}
