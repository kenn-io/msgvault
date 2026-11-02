package mime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStripHTMLPrefixSuppressesTruncatedHiddenElements(t *testing.T) {
	for _, tc := range []struct {
		name, rawHTML, want string
	}{
		{
			name:    "head ends at prefix boundary",
			rawHTML: `<head><style>.hidden { color: red; }`,
			want:    "",
		},
		{
			name:    "style ends at prefix boundary",
			rawHTML: `<body><p>Visible body text</p><style>.hidden { color: red; }`,
			want:    "Visible body text",
		},
		{
			name:    "script ends at prefix boundary",
			rawHTML: `<body><p>Visible body text</p><script>const hidden = "secret";`,
			want:    "Visible body text",
		},
		{
			name:    "head closes implicitly",
			rawHTML: `<head><title>Hidden title</title><p>Visible body text</p>`,
			want:    "Visible body text",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, StripHTMLPrefix(tc.rawHTML))
		})
	}
}
