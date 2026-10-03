package cmd

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/textutil"
)

func TestSearchTableFitsDisplayWidthAndAlignsColumns(t *testing.T) {
	rows := []query.MessageSummary{
		{ID: 1, SentAt: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC), FromName: "F" + strings.Repeat("sender", 10), Subject: "S" + strings.Repeat("subject ", 40), SizeEstimate: 1024},
		{ID: 2027, SentAt: time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC), FromName: "F" + strings.Repeat("界e\u0301👩‍💻", 12), Subject: "S" + strings.Repeat("界e\u0301👩‍💻 ", 40), SizeEstimate: 100 * 1024 * 1024},
	}
	for _, width := range []int{40, 80, 120, 160, 200, 500} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			assert := assert.New(t)
			var out bytes.Buffer
			require.NoError(t, writeSearchResultsTableWidth(&out, rows, width))
			lines := strings.Split(out.String(), "\n")
			for _, line := range lines[:4] {
				assert.LessOrEqual(uniseg.StringWidth(line), width, "%q", line)
				assert.True(utf8.ValidString(line))
			}
			assert.Equal([]string{"ID", "DATE", "FROM", "SUBJECT", "SIZE"}, strings.Fields(lines[0]))
			for i, size := range []string{"1.0K", "100.0M"} {
				line := lines[i+2]
				assert.Contains(line, size)
				assertTableColumn(t, lines[0], "DATE", line, rows[i].SentAt.Format("2006-01-02"))
				assertTableColumn(t, lines[0], "FROM", line, "F")
				assertTableColumn(t, lines[0], "SUBJECT", line, "S")
				assertTableColumn(t, lines[0], "SIZE", line, size)
			}
			assert.Contains(lines[3], "2027")
			if width == 500 {
				assert.Contains(lines[2], rows[0].Subject)
				assert.Contains(lines[3], rows[1].Subject)
			}
		})
	}
}

func assertTableColumn(t *testing.T, header, label, row, value string) {
	t.Helper()
	headerIndex := strings.Index(header, label)
	rowIndex := strings.Index(row, value)
	require.NotEqual(t, -1, headerIndex)
	require.NotEqual(t, -1, rowIndex)
	assert.Equal(t, uniseg.StringWidth(header[:headerIndex]), uniseg.StringWidth(row[:rowIndex]), label)
}

func TestSearchTableShortSenderLeavesRoomForSubject(t *testing.T) {
	subject := strings.Repeat("complete subject ", 5)
	var out bytes.Buffer
	require.NoError(t, writeSearchResultsTableWidth(&out, []query.MessageSummary{{ID: 1, FromName: "F", Subject: subject}}, 120))
	assert.Contains(t, out.String(), strings.TrimSpace(subject))
}

func TestSearchTableEastAsianWidthSetting(t *testing.T) {
	// ANSI reads this setting at package initialization. A child of the real
	// test binary exercises that startup boundary and the production renderer.
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(executable, "-test.run=^TestSearchTableShortSenderLeavesRoomForSubject$")
	cmd.Env = append(os.Environ(), "RUNEWIDTH_EASTASIAN=1")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestSearchTableTooNarrowPreservesFixedFields(t *testing.T) {
	assert := assert.New(t)
	var out bytes.Buffer
	require.NoError(t, writeSearchResultsTableWidth(&out, []query.MessageSummary{{ID: 1234567890123, FromName: "界👩‍💻", Subject: strings.Repeat("界", 50), SizeEstimate: 100 * 1024 * 1024}}, 20))
	line := strings.Split(out.String(), "\n")[2]
	assert.True(utf8.ValidString(line))
	assert.Contains(line, "1234567890123")
	assert.Contains(line, "0001-01-01")
	assert.Contains(line, "100.0M")
	assert.Greater(uniseg.StringWidth(line), 20)
}

func TestSearchTableHybridScoreWidthsAndBoostBudget(t *testing.T) {
	rrf, bm25, vec := 0.5, -1234567890123.125, 987654.25
	rows := []daemonclient.CLIHybridSearchResult{
		{ID: 123456789, SentAt: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC), FromEmail: "F" + strings.Repeat("sender", 10), Subject: "S" + strings.Repeat("界e\u0301👩‍💻", 40), SubjectBoosted: true, RRFScore: &rrf, BM25Score: &bm25, VectorScore: &vec},
		{ID: 2, SentAt: time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC), Message: query.MessageSummary{FromName: "F界", Snippet: "Short"}, SubjectBoosted: true},
	}
	for _, explain := range []bool{false, true} {
		for _, width := range []int{80, 120, 160, 200} {
			t.Run(fmt.Sprintf("%t/%d", explain, width), func(t *testing.T) {
				assert := assert.New(t)
				var out bytes.Buffer
				require.NoError(t, writeHybridResultsTableWidth(&out, rows, explain, false, width))
				lines := strings.Split(out.String(), "\n")
				for _, line := range lines[:4] {
					assert.True(utf8.ValidString(line))
					assert.LessOrEqual(uniseg.StringWidth(line), width, "%q", line)
				}
				for _, line := range lines[2:4] {
					assert.Contains(line, " *")
					assertTableColumn(t, lines[0], "FROM", line, "F")
					assertTableColumn(t, lines[0], "SUBJECT", line, "S")
				}
				assert.Contains(lines[2], "123456789")
				if explain {
					for i, score := range []string{"0.5000", "-1234567890123.1250", "987654.2500"} {
						assertTableColumn(t, lines[0], []string{"RRF", "BM25", "VEC"}[i], lines[2], score)
					}
				}
			})
		}
	}
}

func TestSearchTableKeycapGraphemesFitSenderAndSubject(t *testing.T) {
	for _, tt := range []struct {
		name  string
		row   query.MessageSummary
		width int
	}{
		{"sender", query.MessageSummary{ID: 1, FromName: strings.Repeat("1️⃣", 4)}, 37},
		{"subject", query.MessageSummary{ID: 1, FromName: "F", Subject: "a" + strings.Repeat("1️⃣", 4)}, 35},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			var out bytes.Buffer
			if !assert.NotPanics(func() {
				require.NoError(t, writeSearchResultsTableWidth(&out, []query.MessageSummary{tt.row}, tt.width))
			}) {
				return
			}
			lines := strings.Split(out.String(), "\n")
			for _, line := range lines[:3] {
				assert.LessOrEqual(ansi.StringWidth(line), tt.width, "%q", line)
			}
			assert.NotContains(lines[2], "1️⃣1️⃣1️⃣...")
		})
	}
	t.Run("hybrid subject", func(t *testing.T) {
		var out bytes.Buffer
		require.NoError(t, writeHybridResultsTableWidth(&out, []daemonclient.CLIHybridSearchResult{{ID: 1, FromEmail: "F", Subject: "a" + strings.Repeat("1️⃣", 6), SubjectBoosted: true}}, false, false, 33))
		line := strings.Split(out.String(), "\n")[2]
		assert.LessOrEqual(t, ansi.StringWidth(line), 33)
		assert.Contains(t, line, " *")
	})
}

type searchTableFailAfterWriter struct{ remaining int }

func (w *searchTableFailAfterWriter) Write(p []byte) (int, error) {
	if w.remaining == 0 {
		return 0, io.ErrClosedPipe
	}
	w.remaining--
	return len(p), nil
}

func TestSearchTablePropagatesEveryWriteError(t *testing.T) {
	for _, writes := range []int{0, 1, 2, 3} {
		err := writeSearchResultsTableWidth(&searchTableFailAfterWriter{remaining: writes}, []query.MessageSummary{{ID: 1}}, 80)
		require.ErrorIs(t, err, io.ErrClosedPipe)
	}
	for _, writes := range []int{0, 1, 2} {
		err := writeHybridResultsTableWidth(&searchTableFailAfterWriter{remaining: writes}, []daemonclient.CLIHybridSearchResult{{ID: 1}}, true, false, 80)
		require.ErrorIs(t, err, io.ErrClosedPipe)
	}
}

func FuzzSearchTableFullText(f *testing.F) {
	for _, text := range []string{
		strings.Repeat("long complete text ", 20),
		strings.Repeat("界", 80),
		strings.Repeat("e\u0301", 80),
		strings.Repeat("👩‍💻", 80),
		"\x1b[31mhello\n\tworld\x1b[0m\u009b31m",
		"\xff\u0301\u200d\u200b\ufe0f",
	} {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) {
		assert := assert.New(t)
		require := require.New(t)
		// The sanitizer owns control removal. The renderer must preserve its
		// complete normalized output, independent of the search shape.
		from := strings.Join(strings.Fields(textutil.SanitizeTerminal("F:"+text)), " ")
		subject := strings.Join(strings.Fields(textutil.SanitizeTerminal("S:"+text)), " ")
		message := query.MessageSummary{ID: 1, FromName: "F:" + text, Snippet: "S:" + text}
		var out bytes.Buffer
		require.NoError(writeSearchResultsTableWidth(&out, []query.MessageSummary{message}, 0))
		assert.Contains(out.String(), from)
		assert.Contains(out.String(), subject)
		for _, explain := range []bool{false, true} {
			out.Reset()
			require.NoError(writeHybridResultsTableWidth(&out, []daemonclient.CLIHybridSearchResult{{ID: 1, Message: message, SubjectBoosted: true}}, explain, false, 0))
			assert.Contains(out.String(), from)
			assert.Contains(out.String(), subject+" *")
		}
	})
}

func FuzzSearchTableBoundedText(f *testing.F) {
	for _, text := range []string{
		strings.Repeat("long text ", 20), strings.Repeat("界", 80),
		strings.Repeat("e\u0301", 80), strings.Repeat("👩‍💻", 80),
		"\xff\x1b[31m\u0301\u200d\u200b\ufe0f", "👍🏽🇺🇳1️⃣界",
	} {
		f.Add(text, uint64(7), true)
		f.Add(text, uint64(80), false)
		f.Add(text, uint64(496), false)
	}
	f.Add(strings.Repeat("1️⃣", 4), uint64(2), false)
	f.Add("a"+strings.Repeat("1️⃣", 4), uint64(3), false)
	f.Add(strings.Repeat("👩‍💻", 80), uint64(math.MaxUint64), true)
	f.Fuzz(func(t *testing.T, text string, extra uint64, boosted bool) {
		assert := assert.New(t)
		require := require.New(t)
		text = strings.Join(strings.Fields(textutil.SanitizeTerminal(text)), " ")
		// Saturate only at the machine integer boundary; keep the full generated
		// width domain without allocating a buffer proportional to the budget.
		width := 4 + int(min(extra, uint64(math.MaxInt-7)))
		marker := ""
		if boosted {
			width += 3 // Subject minimum is seven cells, including the marker.
			marker = " *"
		}
		got := (searchTableCell{text: text, marker: marker}).display(width)
		require.True(utf8.ValidString(got))
		assert.LessOrEqual(uniseg.StringWidth(got), width, "%q", got)
		assert.LessOrEqual(ansi.StringWidth(got), width, "%q", got)
		require.True(strings.HasSuffix(got, marker))
		if ansi.StringWidth(text+marker) <= width {
			assert.Equal(text+marker, got)
			return
		}
		require.True(strings.HasSuffix(strings.TrimSuffix(got, marker), "..."))
		// A truncated value must be a prefix ending at a whole grapheme,
		// followed by the ellipsis and the complete boost marker.
		prefix := strings.TrimSuffix(strings.TrimSuffix(got, marker), "...")
		require.True(strings.HasPrefix(text, prefix), "%q is not a prefix of %q", prefix, text)
		clusters := uniseg.NewGraphemes(text)
		boundary := len(prefix) == 0
		for clusters.Next() {
			_, end := clusters.Positions()
			if end == len(prefix) {
				boundary = true
				break
			}
		}
		assert.True(boundary, "truncated inside a grapheme: %q -> %q", text, got)
	})
}
