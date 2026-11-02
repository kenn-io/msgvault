package msgsegment

import (
	"regexp"
	"strings"
)

var datedAttribution = regexp.MustCompile(`(?i)^On\s+(?:(?:Mon(?:day)?|Tue(?:sday)?|Wed(?:nesday)?|Thu(?:rsday)?|Fri(?:day)?|Sat(?:urday)?|Sun(?:day)?|Jan(?:uary)?|Feb(?:ruary)?|Mar(?:ch)?|Apr(?:il)?|May|Jun(?:e)?|Jul(?:y)?|Aug(?:ust)?|Sep(?:t(?:ember)?)?|Oct(?:ober)?|Nov(?:ember)?|Dec(?:ember)?)\b|\d).*?(?:\b(?:19|20)\d{2}\b|\d{1,2}:\d{2}|\d{1,2}/\d{1,2})`)
var wroteSuffix = regexp.MustCompile(`(?i)\bwrote:\s*$`)

// ReadableText preserves the CLI's display contract: remove angle-prefixed
// lines, complete dated English attribution lines followed by a quote, and the
// RFC signature tail. Broader Split heuristics do not change this display view.
func ReadableText(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	r := Result{body: body}
	lines := linesOf(body)
	for i, l := range lines {
		text := strings.TrimSuffix(body[l.start:l.end], "\n")
		trimmed := strings.TrimSpace(text)
		kind, basis := Authored, "unmarked"
		switch {
		case strings.HasPrefix(trimmed, ">"):
			kind, basis = Quoted, "angle_prefix"
		case text == "-- ":
			addSegment(&r, segment(Signature, l.start, len(body), "signature_marker", 0))
			return strings.TrimSpace(r.AuthoredText())
		case datedAttribution.MatchString(trimmed) && wroteSuffix.MatchString(trimmed) && displayQuoteFollows(lines[i+1:]):
			kind, basis = Quoted, "attribution_line"
		}
		addSegment(&r, segment(kind, l.start, l.end, basis, 0))
	}
	return strings.TrimSpace(r.AuthoredText())
}
func displayQuoteFollows(lines []line) bool {
	for _, l := range lines {
		if t := strings.TrimSpace(l.text); t != "" {
			return strings.HasPrefix(t, ">")
		}
	}
	return false
}
