package tui

import (
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func plainMarkdownLines(lines []string) []string {
	plain := make([]string, len(lines))
	for i, line := range lines {
		plain[i] = strings.TrimSpace(stripANSI(line))
	}
	return plain
}

func TestRenderMarkdownLinesFormatsStructuredMeetingNotes(t *testing.T) {
	assert := assert.New(t)
	lines := renderMarkdownLines(
		"### Decisions\n\n- Keep **one archive**\n- Ship `today`\n\nTranscript:\n[00:00] Speaker A: Hello",
		80, true, true,
	)
	plain := plainMarkdownLines(lines)
	joined := strings.Join(plain, "\n")

	assert.Contains(plain, "### Decisions")
	assert.Contains(plain, "• Keep one archive")
	assert.Contains(plain, "• Ship today")
	assert.Contains(plain, "[00:00] Speaker A: Hello")
	assert.NotContains(joined, "**")
	assert.NotContains(joined, "`today`")
}

func TestRenderMarkdownLinesAppliesTerminalStyles(t *testing.T) {
	lines := renderMarkdownLines("### Heading\n\nSome **bold** text.", 80, true, false)

	assert.Contains(t, strings.Join(lines, "\n"), "\x1b[")
}

func TestRenderMarkdownLinesPreservesPlainTranscriptLines(t *testing.T) {
	lines := renderMarkdownLines(
		"[00:00] Speaker A: First line\n[00:05] Speaker B: Second line\n[00:09] Speaker A: Third line",
		80, true, true,
	)
	plain := plainMarkdownLines(lines)

	assert.Contains(t, plain, "[00:00] Speaker A: First line")
	assert.Contains(t, plain, "[00:05] Speaker B: Second line")
	assert.Contains(t, plain, "[00:09] Speaker A: Third line")
}

func TestRenderMarkdownLinesNoColorHasNoANSI(t *testing.T) {
	lines := renderMarkdownLines("# Heading\n\nSome **bold** text.", 80, true, true)

	assert.NotContains(t, strings.Join(lines, "\n"), "\x1b[")
}

func TestRenderMarkdownLinesStripsTerminalCommandsAndCapsWidth(t *testing.T) {
	lines := renderMarkdownLines(
		"Safe\x1b[2J text with a deliberately long sentence that must fit the viewport width.",
		32, true, false,
	)

	joined := strings.Join(lines, "\n")
	assert.NotContains(t, joined, "\x1b[2J")
	for _, line := range lines {
		assert.LessOrEqual(t, xansi.StringWidth(line), 32)
	}
}

func TestSanitizeMarkdownSourcePreservesLinesAndStripsAllControls(t *testing.T) {
	input := "line one\r\n" +
		"\x1b[31mred\x1b[0m\n" +
		"csi:\u009b2J\n" +
		"osc:\u009dtitle\u009c\n" +
		"lone:\x1b\n" +
		"partial:\x1b[31"

	got := sanitizeMarkdownSource(input)

	assert.Equal(t, "line one\nred\ncsi:2J\nosc:title\nlone:\npartial:", got)
}

func TestSanitizeMarkdownSourceDecodesEntitiesBeforeFiltering(t *testing.T) {
	got := sanitizeMarkdownSource("before &#27;[2J after")

	assert.Equal(t, "before  after", got)
}

func TestSanitizeMarkdownSourcePreservesPrintableEntities(t *testing.T) {
	input := "&#35; literal heading marker\n&gt; literal quote marker\n&amp; literal ampersand"

	assert.Equal(t, input, sanitizeMarkdownSource(input))
}

func TestSanitizeMarkdownEscapesRejectsControlInterleavedCommands(t *testing.T) {
	assert := assert.New(t)
	got := sanitizeMarkdownEscapes("before \x1b[\x012J after")

	assert.NotContains(got, "\x1b")
	assert.NotContains(got, "\x01")
	assert.Contains(got, "before")
	assert.Contains(got, "after")
}

func TestRenderMarkdownLinesRejectsSourceProvidedSGR(t *testing.T) {
	lines := renderMarkdownLines("before \x1b[31mINJECTED\x1b[0m after", 80, true, false)
	joined := strings.Join(lines, "\n")

	assert.Contains(t, stripANSI(joined), "before INJECTED after")
	assert.NotContains(t, joined, "\x1b[31mINJECTED")
}

func TestRenderMarkdownLinesRejectsEntityEncodedInterleavedTerminalCommand(t *testing.T) {
	assert := assert.New(t)
	lines := renderMarkdownLines(
		"before &#27;[&#1;2J forged screen after",
		80, true, false,
	)
	joined := strings.Join(lines, "\n")

	assert.Contains(stripANSI(joined), "before")
	assert.Contains(stripANSI(joined), "after")
	assert.NotContains(joined, "\x1b[2J")
	assert.NotContains(markdownAllSGR.ReplaceAllString(joined, ""), "\x1b")
	for _, r := range joined {
		assert.False(r < 0x20 && r != '\t' && r != '\n' && r != 0x1b,
			"unexpected control character U+%04X", r)
	}
}

func TestMeetingMarkdownCacheInvalidatesOnContentAndWidth(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cache := newMarkdownCache(true, true)
	first := cache.meetingLinesFor(1, "# First", 80)
	same := cache.meetingLinesFor(1, "# First", 80)
	require.NotEmpty(first)
	require.NotEmpty(same)
	assert.Same(&first[0], &same[0])

	changedText := cache.meetingLinesFor(1, "# Second", 80)
	require.NotEmpty(changedText)
	assert.NotSame(&first[0], &changedText[0])

	changedWidth := cache.meetingLinesFor(1, "# Second", 40)
	require.NotEmpty(changedWidth)
	assert.NotSame(&changedText[0], &changedWidth[0])
}
