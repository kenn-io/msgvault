package mime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStripHTMLPrefixSuppressesUnterminatedHiddenElements(t *testing.T) {
	for _, tc := range []struct {
		name string
		html string
		want string
	}{
		{"style", `<p>visible</p><style>.secret { color: red; } hidden`, "visible"},
		{"script", `<p>visible</p><script>const secret = "hidden";`, "visible"},
		{"unterminated head", `<head><title>hidden</title>`, ""},
		{"implicit head close", `<html><head><title>hidden title</title><script>hidden script</script><meta charset="utf-8"><p>visible body</p>`, "visible body"},
		{"implicit head close before text", `<head><title>hidden title</title>visible body text`, "visible body text"},
		{"closed style", `<p>visible</p><style>.secret { color: red; }</style><p>after</p>`, "visible\n\nafter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, StripHTMLPrefix(tc.html))
		})
	}
}
