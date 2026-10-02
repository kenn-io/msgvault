package preprocess

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Frozen policy oracles from before the scanner optimization. Keep them in
// tests so the generation fingerprint's byte-for-byte contract is checked by
// an independent implementation.
var (
	legacyBase64      = regexp.MustCompile(`[A-Za-z0-9+]{200,}={0,2}`)
	legacyBase64Slash = regexp.MustCompile(`[A-Za-z0-9+/]{300,}={0,2}`)
	legacyDataURI     = regexp.MustCompile(`(?i)data:[a-zA-Z0-9./+\-]{0,128};base64,[A-Za-z0-9+/]+={0,2}`)
)

func TestStripBase64Runs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		input  string
		want   string
		minRun int
		slash  bool
	}{
		{"empty", "", "", 200, false},
		{"below threshold", strings.Repeat("A", 199), strings.Repeat("A", 199), 200, false},
		{"at threshold", strings.Repeat("A", 200), " ", 200, false},
		{"above threshold", strings.Repeat("A", 201), " ", 200, false},
		{"one padding", strings.Repeat("A", 200) + "=", " ", 200, false},
		{"two padding", strings.Repeat("A", 200) + "==", " ", 200, false},
		{"extra padding", strings.Repeat("A", 200) + "===", " =", 200, false},
		{"short padded run", strings.Repeat("A", 199) + "==", strings.Repeat("A", 199) + "==", 200, false},
		{"slash breaks first pass", strings.Repeat("A", 100) + "/" + strings.Repeat("B", 100), strings.Repeat("A", 100) + "/" + strings.Repeat("B", 100), 200, false},
		{"slash below threshold", strings.Repeat("a/", 149) + "a", strings.Repeat("a/", 149) + "a", 300, true},
		{"slash at threshold", strings.Repeat("a/", 150), " ", 300, true},
		{"slash above threshold", strings.Repeat("a/", 150) + "a", " ", 300, true},
		{"all alphabet classes", strings.Repeat("Az09+", 40), " ", 200, false},
		{"unicode separators", "前" + strings.Repeat("A", 200) + "Kſ後", "前 Kſ後", 200, false},
		{"invalid UTF8 separators", "\xff" + strings.Repeat("A", 200) + "\xfe", "\xff \xfe", 200, false},
		{"multiple matches", "!" + strings.Repeat("A", 200) + "==," + strings.Repeat("B", 200) + "===?", "! , =?", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stripBase64Runs(tc.input, tc.minRun, tc.slash))
		})
	}
}

func TestStripBase64PassOrder(t *testing.T) {
	input := strings.Repeat("A", 200) + "/" + strings.Repeat("B", 100)
	first := stripBase64Runs(input, 200, false)
	assert.Equal(t, " /"+strings.Repeat("B", 100), stripBase64Runs(first, 300, true))
}

func TestStripBase64UnchangedNoAllocations(t *testing.T) {
	input := strings.Repeat("ordinary text https://example.com/path 日本語\xff\n", 100)
	var got string
	allocs := testing.AllocsPerRun(100, func() {
		got = stripBase64Runs(input, 200, false)
		got = stripBase64Runs(got, 300, true)
	})
	assert.Equal(t, input, got)
	require.Zero(t, allocs, "unchanged bare-base64 scans should not allocate")
}

func FuzzStripBase64Runs(f *testing.F) {
	for _, n := range []int{199, 200, 201, 299, 300, 301} {
		for _, padding := range []string{"", "=", "==", "==="} {
			f.Add("前\xff!" + strings.Repeat("A", n) + padding + "?後")
			f.Add(strings.Repeat("a/", n/2) + "a" + padding)
		}
	}
	f.Add(strings.Repeat("A", 200) + "/" + strings.Repeat("B", 100))
	f.Add("http://example.com/a/b+c==?x=1\r\n\xff")
	for mask := range 16 {
		prefix := []byte("data:")
		for i := range 4 {
			if mask&(1<<i) != 0 {
				prefix[i] -= 'a' - 'A'
			}
		}
		f.Add("head " + string(prefix) + "teſt/plain;Baſe64,KſAA== tail")
	}
	f.Add("data:" + strings.Repeat("ſ", 128) + ";base64,AAAA==")
	f.Add("data:" + strings.Repeat("ſ", 129) + ";base64,AAAA==")
	f.Fuzz(func(t *testing.T, input string) {
		// Bound the expensive historical oracle, not the production scanner.
		if len(input) > 4096 {
			t.Skip()
		}
		assert.Equal(t, legacyBase64.ReplaceAllString(input, " "), stripBase64Runs(input, 200, false))
		assert.Equal(t, legacyBase64Slash.ReplaceAllString(input, " "), stripBase64Runs(input, 300, true))
		want := legacyBase64Slash.ReplaceAllString(legacyBase64.ReplaceAllString(input, " "), " ")
		got := stripBase64Runs(stripBase64Runs(input, 200, false), 300, true)
		assert.Equal(t, want, got)
		normalized := strings.ReplaceAll(input, "\r\n", "\n")
		want = strings.TrimSpace(legacyBase64Slash.ReplaceAllString(legacyBase64.ReplaceAllString(legacyDataURI.ReplaceAllString(normalized, " "), " "), " "))
		got, truncated := Preprocess("", input, 0, Config{StripBase64: true})
		assert.Equal(t, want, got)
		assert.False(t, truncated)
	})
}
