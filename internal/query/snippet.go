package query

import (
	"regexp"
	"strings"
)

var (
	// Heading markers require trailing whitespace so issue refs (#50362)
	// and hashtags (#launch) survive.
	snippetHeading    = regexp.MustCompile(`(^|\s)#{1,6}\s+`)
	snippetListMarker = regexp.MustCompile(`(?m)^\s{0,3}(?:[-*+]|\d{1,3}[.)])\s+`)
	snippetBold       = regexp.MustCompile(`\*\*([^\s*](?:[^*]*[^\s*])?)\*\*`)
	snippetInlineCode = regexp.MustCompile("`([^`]+)`")
	snippetWhitespace = regexp.MustCompile(`\s+`)
)

// FlattenSnippet renders stored snippet markup (markdown headings, list
// markers, bold/code emphasis) as plain single-line text. Meeting importers
// persist raw body prefixes as snippets, so structural markdown otherwise
// surfaces verbatim in explore excerpts.
func FlattenSnippet(snippet string) string {
	if snippet == "" {
		return snippet
	}
	flattened := snippetHeading.ReplaceAllString(snippet, "$1")
	flattened = snippetListMarker.ReplaceAllString(flattened, "")
	flattened = snippetBold.ReplaceAllString(flattened, "$1")
	flattened = snippetInlineCode.ReplaceAllString(flattened, "$1")
	flattened = snippetWhitespace.ReplaceAllString(flattened, " ")
	return strings.TrimSpace(flattened)
}
