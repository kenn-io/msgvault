package feeds

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAbsoluteURLs(t *testing.T) {
	t.Parallel()
	const base = "https://example.com/blog/feed.atom"
	for _, tc := range []struct {
		name, in, want string
	}{
		{"relative image", `<p>Chart: <img src="images/chart.png" alt="chart"></p>`,
			`<p>Chart: <img src="https://example.com/blog/images/chart.png" alt="chart"></p>`},
		{"root-relative link", `<a href="/about">About</a>`, `<a href="https://example.com/about">About</a>`},
		{"protocol-relative", `<img src="//cdn.example.net/x.png">`, `<img src="https://cdn.example.net/x.png">`},
		{"srcset", `<img srcset="a.png 1x, b.png 2x">`, `<img srcset="https://example.com/blog/a.png 1x, https://example.com/blog/b.png 2x">`},
		{"fragment stays", `<a href="#notes">Notes</a>`, `<a href="#notes">Notes</a>`},
		{"absolute stays", `<a href="https://other.example/x">x</a>`, `<a href="https://other.example/x">x</a>`},
		{"mailto stays", `<a href="mailto:a@example.com">a</a>`, `<a href="mailto:a@example.com">a</a>`},
		{"data srcset stays", `<img srcset="data:image/png;base64,AA== 1x">`, `<img srcset="data:image/png;base64,AA== 1x">`},
		{"untouched bytes kept", "<p class='x'>Text &amp; more</p>\n<!-- note -->", "<p class='x'>Text &amp; more</p>\n<!-- note -->"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, absoluteURLs(tc.in, base))
		})
	}
}
