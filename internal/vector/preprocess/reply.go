package preprocess

import "regexp"

// notReplyStart begins a line that is not itself an "On ..." attribution, so an
// authored "On Monday..." line cannot absorb the next reply header.
const notReplyStart = `(?:[^o \t\r\n]|o[^n\r\n]|on[^ \t\r\n])[^\n]*`

const replyAttribution = `(?:on [^\n]*\bwrote:|on [^\n]*\r?\n(?:[ \t]*(?:` + notReplyStart + `)?\r?\n){0,2}?[ \t]*(?:` + notReplyStart + `)?\bwrote:)`

var (
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
	body = quotedReply.ReplaceAllString(body, "")
	if loc := quotedStart.FindStringIndex(body); loc != nil {
		body = body[:loc[0]]
	}
	return quotedLine.ReplaceAllString(body, "")
}
