package tui

import (
	"os"
	"regexp"
	"strings"

	"charm.land/glamour/v2"
	gansi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	xansi "github.com/charmbracelet/x/ansi"
	"go.kenn.io/msgvault/internal/textutil"
)

var (
	markdownNonCSIEscape = regexp.MustCompile(
		`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?` +
			`|\x1bP[^\x1b]*(?:\x1b\\)?` +
			`|\x1b[^[\]P]`,
	)
	markdownCSI = regexp.MustCompile(
		`\x1b\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]`,
	)
	markdownSGR             = regexp.MustCompile(`^\x1b\[[0-9;]*m$`)
	markdownAllSGR          = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	markdownTrailingPadding = regexp.MustCompile(`(\s|\x1b\[[0-9;]*m)+$`)
)

// markdownCache avoids rerendering long meeting notes on every scroll frame.
// The pointer is shared across Bubble Tea model copies, matching the cache
// pattern used for other presentation-only state.
type markdownCache struct {
	dark    bool
	noColor bool

	meetingID    int64
	meetingText  string
	meetingWidth int
	meetingDark  bool
	meetingLines []string
}

func newMarkdownCache(dark, noColor bool) *markdownCache {
	return &markdownCache{dark: dark, noColor: noColor}
}

func (c *markdownCache) setDark(dark bool) {
	if c == nil || c.dark == dark {
		return
	}
	c.dark = dark
	c.meetingLines = nil
}

func (c *markdownCache) meetingLinesFor(id int64, text string, width int) []string {
	if c == nil {
		return renderMarkdownLines(text, width, false, noColorRequested())
	}
	if c.meetingID == id && c.meetingText == text && c.meetingWidth == width &&
		c.meetingDark == c.dark && c.meetingLines != nil {
		return c.meetingLines
	}
	c.meetingLines = renderMarkdownLines(text, width, c.dark, c.noColor)
	c.meetingID = id
	c.meetingText = text
	c.meetingWidth = width
	c.meetingDark = c.dark
	return c.meetingLines
}

func noColorRequested() bool {
	value, ok := os.LookupEnv("NO_COLOR")
	return ok && value != ""
}

func markdownStyle(dark bool) gansi.StyleConfig {
	style := styles.LightStyleConfig
	if dark {
		style = styles.DarkStyleConfig
	}
	zero := uint(0)
	style.Document.Margin = &zero
	style.CodeBlock.Margin = &zero
	style.Code.Prefix = ""
	style.Code.Suffix = ""
	return style
}

// renderMarkdownLines renders Markdown for the meeting detail viewport while
// preserving author-provided line breaks. Unsafe terminal controls are removed
// after rendering, and malformed Markdown falls back to safe plain-text wrap.
func renderMarkdownLines(text string, width int, dark, noColor bool) []string {
	if width <= 0 {
		width = 80
	}
	text = sanitizeMarkdownSource(text)
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(markdownStyle(dark)),
		glamour.WithWordWrap(width),
		glamour.WithPreservedNewLines(),
	)
	if err != nil {
		return wrapSafeMarkdownFallback(text, width)
	}
	out, err := renderer.Render(text)
	if err != nil {
		return wrapSafeMarkdownFallback(text, width)
	}
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return []string{""}
	}
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		line = stripMarkdownPadding(line, noColor)
		line = sanitizeMarkdownEscapes(line)
		if xansi.StringWidth(line) > width {
			line = xansi.Truncate(line, width, "")
		}
		lines[i] = line
	}
	return lines
}

// sanitizeMarkdownSource removes every source-provided terminal control before
// Glamour adds its own SGR styling. Sanitizing line by line applies the shared
// single-line security contract without collapsing Markdown structure.
func sanitizeMarkdownSource(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = textutil.SanitizeTerminal(line)
	}
	return strings.Join(lines, "\n")
}

func wrapSafeMarkdownFallback(text string, width int) []string {
	var lines []string
	for line := range strings.SplitSeq(text, "\n") {
		lines = append(lines, wrapText(textutil.SanitizeTerminal(line), width)...)
	}
	return lines
}

// sanitizeMarkdownEscapes retains Glamour's SGR styling while removing
// cursor movement, title changes, device commands, and overwrite controls.
func sanitizeMarkdownEscapes(line string) string {
	line = markdownNonCSIEscape.ReplaceAllString(line, "")
	line = markdownCSI.ReplaceAllStringFunc(line, func(sequence string) string {
		if markdownSGR.MatchString(sequence) {
			return sequence
		}
		return ""
	})
	var b strings.Builder
	for _, r := range line {
		if r < 0x20 && r != '\t' && r != 0x1b {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func stripMarkdownPadding(line string, noColor bool) string {
	line = markdownTrailingPadding.ReplaceAllString(line, "")
	if noColor {
		return markdownAllSGR.ReplaceAllString(line, "")
	}
	return line + "\x1b[0m"
}
