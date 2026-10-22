package preprocess

import (
	"regexp"
	"strings"
)

const replyAttribution = `(?:on [^\n]*\bwrote:|on [^\n]*\r?\n(?:[^\n]*\r?\n){0,2}?[^\n]*\bwrote:)`

var (
	replyStart = regexp.MustCompile(`(?im)^[ \t]*on `)
	// quotedReply drops a wrapped attribution and its quoted lines, keeping
	// any authored reply above or below them.
	quotedReply = regexp.MustCompile(`(?im)^[ \t]*` + replyAttribution + `[ \t]*\r?\n(?:[ \t]*\r?\n)*(?:[ \t]*>+[ \t]?.*(?:\r?\n|$))+`)
	// quotedStart is where the quoted part begins: a reply attribution that
	// may wrap onto a second line, an "Original Message" or "Forwarded
	// message" separator, or Outlook's From:/Sent: header pair.
	quotedStart = regexp.MustCompile(`(?im)^[ \t]*(?:` + replyAttribution + `[ \t\r]*$|-{2,}\s*(?:original message|forwarded message)\b|begin forwarded message:\s*$|from:[ \t][^\n]*\n[ \t]*sent:[ \t])`)

	quotedLine = regexp.MustCompile(`(?m)^[ \t]*>+[ \t]?.*\n?`)
)

// StripReplyQuotes removes wrapped and indented quotations for loop detection.
// It keeps authored text above and below marked quotations.
func StripReplyQuotes(body string) string {
	// Treat each attribution separately so an authored "On Monday..." line
	// cannot absorb the next reply header as its wrapped continuation.
	boundaries := replyStart.FindAllStringIndex(body, -1)
	boundaries = append(boundaries, []int{len(body)})
	var text strings.Builder
	start := 0
	for _, boundary := range boundaries {
		part := quotedReply.ReplaceAllString(body[start:boundary[0]], "")
		stop := false
		if loc := quotedStart.FindStringIndex(part); loc != nil {
			part, stop = part[:loc[0]], true
		}
		text.WriteString(quotedLine.ReplaceAllString(part, ""))
		if stop {
			break
		}
		start = boundary[0]
	}
	return text.String()
}
