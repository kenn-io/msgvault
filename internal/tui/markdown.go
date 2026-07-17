package tui

import (
	"html"
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
	markdownAllSGR          = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	markdownTrailingPadding = regexp.MustCompile(`(\s|\x1b\[[0-9;]*m)+$`)
	markdownEntity          = regexp.MustCompile(`&(?:#[xX][0-9A-Fa-f]+;?|#[0-9]+;?|[A-Za-z][A-Za-z0-9]+;)`)
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
	text = markdownEntity.ReplaceAllStringFunc(text, func(entity string) string {
		decoded := html.UnescapeString(entity)
		for _, r := range decoded {
			if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
				return decoded
			}
		}
		return entity
	})
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

// sanitizeMarkdownEscapes retains only Glamour's SGR styling. Controls are
// removed before escape parsing so interleaved bytes cannot reconstruct a
// terminal command after validation.
func sanitizeMarkdownEscapes(line string) string {
	var normalized strings.Builder
	for _, r := range line {
		switch {
		case r == 0x1b:
			normalized.WriteRune(r)
		case r == '\t':
			normalized.WriteByte(' ')
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			continue
		default:
			normalized.WriteRune(r)
		}
	}
	line = normalized.String()

	var b strings.Builder
	for i := 0; i < len(line); {
		if line[i] != 0x1b {
			b.WriteByte(line[i])
			i++
			continue
		}
		if i+1 >= len(line) || line[i+1] != '[' {
			i++
			continue
		}
		end := i + 2
		for end < len(line) && ((line[end] >= 0x30 && line[end] <= 0x3f) ||
			(line[end] >= 0x20 && line[end] <= 0x2f)) {
			end++
		}
		if end >= len(line) || line[end] < 0x40 || line[end] > 0x7e {
			i++
			continue
		}
		sequence := line[i : end+1]
		if safeMarkdownSGR(sequence) {
			b.WriteString(sequence)
		}
		i = end + 1
	}
	return b.String()
}

func safeMarkdownSGR(sequence string) bool {
	if len(sequence) < 3 || !strings.HasPrefix(sequence, "\x1b[") || sequence[len(sequence)-1] != 'm' {
		return false
	}
	for _, c := range sequence[2 : len(sequence)-1] {
		if (c < '0' || c > '9') && c != ';' {
			return false
		}
	}
	return true
}

func stripMarkdownPadding(line string, noColor bool) string {
	line = markdownTrailingPadding.ReplaceAllString(line, "")
	if noColor {
		return markdownAllSGR.ReplaceAllString(line, "")
	}
	return line + "\x1b[0m"
}
