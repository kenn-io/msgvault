package meetingarchive

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestFormatTranscriptLine(t *testing.T) {
	assert := assert.New(t)
	tests := []struct {
		offset  time.Duration
		speaker string
		want    string
	}{
		{0, "A", "[00:00] A: x"},
		{71 * time.Second, "A", "[01:11] A: x"},
		{3692 * time.Second, "A", "[1:01:32] A: x"},
		{-5 * time.Second, "A", "[00:00] A: x"},
		{0, "", "[00:00] : x"},
	}
	for _, tc := range tests {
		assert.Equal(tc.want, FormatTranscriptLine(tc.offset, tc.speaker, "x"))
	}
}

func TestSnippet(t *testing.T) {
	assert := assert.New(t)

	got := Snippet(strings.Repeat("a", 199) + "é" + "tail")

	assert.True(utf8.ValidString(got))
	assert.Equal(strings.Repeat("a", 199)+"é", got)
	assert.Equal("short body", Snippet("  short body\n"))
}
