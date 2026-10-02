package preprocess

import (
	"github.com/stretchr/testify/assert"
	"strings"
	"testing"
)

func TestPreprocessBase64UnchangedAvoidsCopies(t *testing.T) {
	input := strings.Repeat("ordinary text https://example.com/path 日本語\n", 100)
	var got string
	var truncated bool
	allocs := testing.AllocsPerRun(100, func() {
		got, truncated = Preprocess("", input, 0, Config{StripBase64: true})
	})
	assert.Equal(t, strings.TrimSpace(input), got)
	assert.False(t, truncated)
	assert.Zero(t, allocs, "base64 removal should not copy unchanged text")
}

func TestPreprocessBase64Policy(t *testing.T) {
	for _, tc := range []struct {
		name, subject, body, want string
		maxChars                  int
		cfg                       Config
		truncated                 bool
	}{
		{name: "ordered passes", body: strings.Repeat("A", 200) + "/" + strings.Repeat("B", 100), want: "/" + strings.Repeat("B", 100), cfg: Config{StripBase64: true}},
		{name: "disabled stripping", body: "Start " + strings.Repeat("A", 200) + " End", want: "Start " + strings.Repeat("A", 200) + " End"},
		{name: "whitespace retained", body: "left " + strings.Repeat("A", 200) + " right", want: "left   right", cfg: Config{StripBase64: true}},
		{name: "whitespace collapsed", body: "left " + strings.Repeat("A", 200) + " right", want: "left right", cfg: Config{StripBase64: true, CollapseWhitespace: true}},
		{name: "data URI unicode case folding", body: "head data:teſt/plain;base64,KſAA== tail", want: "head   tail", cfg: Config{StripBase64: true}},
		{name: "pollution removed before unicode body cap", body: strings.Repeat("A", 200) + "\n日本語tail", want: "日本", cfg: Config{StripBase64: true, MaxBodyRunes: 4}, truncated: true},
		{name: "unicode final cap", subject: "X", body: strings.Repeat("A", 200) + "\n日本語tail", want: "Subject: X\n\n日本", maxChars: 14, cfg: Config{StripBase64: true}, truncated: true},
		{name: "no truncation after removal", body: strings.Repeat("A", 200) + " 日本語", want: "日本語", maxChars: 3, cfg: Config{StripBase64: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, truncated := Preprocess(tc.subject, tc.body, tc.maxChars, tc.cfg)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.truncated, truncated)
		})
	}
}
