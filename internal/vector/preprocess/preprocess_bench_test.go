package preprocess

import (
	"strings"
	"testing"
)

// Keep these inputs synthetic and stable so preprocessing costs can be
// compared without a private message archive or a running embedding server.
func BenchmarkPreprocess(b *testing.B) {
	inputs := []struct {
		name string
		body string
	}{
		{"prose", strings.Repeat("Please review the attached report and reply with any questions.\n", 160)},
		{"html", strings.Repeat(`<p>Report <a href="https://example.com/report?utm_source=email">details</a></p>`+"\n", 160)},
		{"base64", "data:image/png;base64," + strings.Repeat("aB9+/", 200000) + "\nReport details follow."},
		{"bare-base64", strings.Repeat("aB9+/", 200000) + "\nReport details follow."},
		{"long-nonmatch", strings.Repeat(strings.Repeat("a", 199)+"!", 1000)},
	}
	cfg := Config{
		StripQuotes: true, StripSignatures: true, StripHTML: true,
		StripBase64: true, StripURLTracking: true, CollapseWhitespace: true,
		MaxBodyRunes: 1_228_800, // 1200-rune windows, 64 spans, 16x raw-body budget.
	}
	for _, input := range inputs {
		b.Run(input.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(input.body)))
			for b.Loop() {
				Preprocess("Test report", input.body, 0, cfg)
			}
		})
	}
}

func BenchmarkPreprocessPasses(b *testing.B) {
	body := strings.Repeat("Please review the attached report and reply with any questions.\n", 160)
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"base64", Config{StripBase64: true}},
		{"html", Config{StripHTML: true}},
		{"quotes", Config{StripQuotes: true}},
		{"signatures", Config{StripSignatures: true}},
		{"whitespace", Config{CollapseWhitespace: true}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				Preprocess("", body, 0, tc.cfg)
			}
		})
	}
}
