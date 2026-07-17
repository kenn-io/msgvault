package tui

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
)

// highlightTerms applies highlight styling to all occurrences of search terms in text.
// Terms are extracted from a search query string using search.Parse().
// Highlighting is case-insensitive. Returns the original text with ANSI highlight codes.
func highlightTerms(text, searchQuery string) string {
	if searchQuery == "" || text == "" {
		return text
	}
	terms := extractSearchTerms(searchQuery)
	if len(terms) == 0 {
		return text
	}
	return applyHighlight(text, terms)
}

// extractSearchTerms extracts displayable search terms from a query string.
func extractSearchTerms(queryStr string) []string {
	q := search.Parse(queryStr)
	var terms []string
	terms = append(terms, q.TextTerms...)
	terms = append(terms, q.FromAddrs...)
	terms = append(terms, q.ToAddrs...)
	terms = append(terms, q.SubjectTerms...)
	// Deduplicate and filter empty
	seen := make(map[string]bool, len(terms))
	filtered := terms[:0]
	for _, t := range terms {
		lower := strings.ToLower(t)
		if t != "" && !seen[lower] {
			seen[lower] = true
			filtered = append(filtered, t)
		}
	}
	return filtered
}

type highlightInterval struct{ start, end int }

// applyHighlight wraps all case-insensitive occurrences of any term in text
// with highlightStyle. Matches are located in ANSI-free text, then styling is
// inserted around printable runes so existing terminal sequences remain intact.
func applyHighlight(text string, terms []string) string {
	if len(terms) == 0 {
		return text
	}
	plainText := ansi.Strip(text)
	intervals := findHighlightIntervals(plainText, terms)
	if len(intervals) == 0 {
		return text
	}

	marker := "\x00"
	styledMarker := highlightStyle.Render(marker)
	prefix, suffix, found := strings.Cut(styledMarker, marker)
	if !found || prefix == "" {
		return text
	}

	var result strings.Builder
	var sgrHistory strings.Builder
	result.Grow(len(text) + len(intervals)*(len(prefix)+len(suffix)))
	state := byte(0)
	visibleRune := 0
	intervalIndex := 0
	inHighlight := false
	for len(text) > 0 {
		sequence, _, bytesRead, newState := ansi.DecodeSequence(text, state, nil)
		if bytesRead == 0 {
			result.WriteString(text)
			break
		}
		state = newState
		text = text[bytesRead:]
		plainSequence := ansi.Strip(sequence)
		if plainSequence == "" {
			result.WriteString(sequence)
			if strings.HasPrefix(sequence, "\x1b[") && strings.HasSuffix(sequence, "m") {
				sgrHistory.WriteString(sequence)
				if inHighlight {
					result.WriteString(prefix)
				}
			}
			continue
		}

		for _, char := range plainSequence {
			if intervalIndex < len(intervals) && visibleRune == intervals[intervalIndex].start {
				result.WriteString(prefix)
				inHighlight = true
			}
			result.WriteRune(char)
			visibleRune++
			if intervalIndex < len(intervals) && visibleRune == intervals[intervalIndex].end {
				result.WriteString(suffix)
				result.WriteString(sgrHistory.String())
				inHighlight = false
				intervalIndex++
			}
		}
	}
	return result.String()
}

// findHighlightIntervals returns merged [start, end) rune offsets in plain
// text. Rune offsets avoid byte-index mismatches when Unicode case folding
// changes encoded length.
func findHighlightIntervals(text string, terms []string) []highlightInterval {
	lowerRunes := []rune(strings.ToLower(text))
	var intervals []highlightInterval
	for _, term := range terms {
		termLowerRunes := []rune(strings.ToLower(term))
		tLen := len(termLowerRunes)
		if tLen == 0 {
			continue
		}
		for i := 0; i <= len(lowerRunes)-tLen; i++ {
			match := true
			for j := range tLen {
				if lowerRunes[i+j] != termLowerRunes[j] {
					match = false
					break
				}
			}
			if match {
				intervals = append(intervals, highlightInterval{i, i + tLen})
				i += tLen - 1 // skip past this match
			}
		}
	}
	if len(intervals) == 0 {
		return nil
	}
	// Sort and merge overlapping intervals
	// Simple insertion sort since we expect few intervals
	for i := 1; i < len(intervals); i++ {
		for j := i; j > 0 && intervals[j].start < intervals[j-1].start; j-- {
			intervals[j], intervals[j-1] = intervals[j-1], intervals[j]
		}
	}
	merged := []highlightInterval{intervals[0]}
	for _, iv := range intervals[1:] {
		last := &merged[len(merged)-1]
		if iv.start <= last.end {
			if iv.end > last.end {
				last.end = iv.end
			}
		} else {
			merged = append(merged, iv)
		}
	}
	return merged
}

// formatBytes formats a byte count as a human-readable string (e.g., "1.5 KB").
func formatBytes(bytes int64) string {
	if bytes == 0 {
		return "-"
	}
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// formatCount formats a count as a human-readable string (e.g., "1.5K", "2.3M").
func formatCount(n int64) string {
	if n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	if n < 1000000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1000000)
}

// padRight pads a string with spaces to fill width terminal cells.
// Uses lipgloss.Width to correctly handle ANSI codes and full-width characters.
func padRight(s string, width int) string {
	sw := lipgloss.Width(s)
	if sw >= width {
		// Use ANSI-aware truncation
		return ansi.Truncate(s, width, "")
	}
	return s + strings.Repeat(" ", width-sw)
}

// truncateRunes truncates a string to fit within maxWidth terminal cells.
// Uses runewidth to correctly handle full-width characters (CJK, emoji, etc.)
// that occupy 2 terminal cells but count as 1 rune.
// Also sanitizes the string by removing newlines and other control characters
// that could break the display layout.
func truncateRunes(s string, maxWidth int) string {
	// Remove newlines and carriage returns that could break layout
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\t", " ")

	width := runewidth.StringWidth(s)
	if width <= maxWidth {
		return s
	}
	if maxWidth <= 3 {
		return runewidth.Truncate(s, maxWidth, "")
	}
	return runewidth.Truncate(s, maxWidth, "...")
}

// formatAddresses formats a slice of addresses as a comma-separated string.
func formatAddresses(addrs []query.Address) string {
	parts := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if addr.Name != "" {
			parts = append(parts, fmt.Sprintf("%s <%s>", addr.Name, addr.Email))
		} else {
			parts = append(parts, addr.Email)
		}
	}
	return strings.Join(parts, ", ")
}

// wrapText wraps text to fit within width terminal cells.
// Uses runewidth to correctly handle full-width characters (CJK, emoji, etc.)
func wrapText(text string, width int) []string {
	if width <= 0 {
		width = 80
	}

	var result []string
	lines := strings.SplitSeq(text, "\n")

	for line := range lines {
		lineWidth := runewidth.StringWidth(line)
		if lineWidth <= width {
			result = append(result, line)
			continue
		}

		// Wrap long lines using terminal cell width
		runes := []rune(line)
		for len(runes) > 0 {
			// Find how many runes fit within width
			currentWidth := 0
			breakAt := 0
			lastSpace := -1

			for i, r := range runes {
				rw := runewidth.RuneWidth(r)
				if currentWidth+rw > width {
					break
				}
				currentWidth += rw
				breakAt = i + 1
				if r == ' ' {
					lastSpace = i
				}
			}

			// Prefer breaking at a space if we found one in the latter half
			if lastSpace > breakAt/2 && breakAt < len(runes) {
				breakAt = lastSpace
			}

			if breakAt == 0 {
				// Single character too wide, take it anyway
				breakAt = 1
			}

			result = append(result, string(runes[:breakAt]))
			runes = runes[breakAt:]

			// Skip leading spaces on continuation lines
			for len(runes) > 0 && runes[0] == ' ' {
				runes = runes[1:]
			}
		}
	}

	return result
}

// truncateToWidth returns the prefix of s that fits within maxWidth visual columns.
// Uses ANSI-aware truncation to preserve escape sequences.
func truncateToWidth(s string, maxWidth int) string {
	return ansi.Truncate(s, maxWidth, "")
}

// skipToWidth returns the suffix of s starting after skipWidth visual columns.
// Uses ANSI-aware cutting to preserve escape sequences.
func skipToWidth(s string, skipWidth int) string {
	// Cut from skipWidth to a large number (beyond any reasonable line width)
	return ansi.Cut(s, skipWidth, 10000)
}
