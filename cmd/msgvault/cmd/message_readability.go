package cmd

import (
	"regexp"
	"strings"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/textutil"
)

// datedAttribution matches a reply header start: "On", then a date with a year, clock time or numeric day.
var datedAttribution = regexp.MustCompile(`(?i)^On\s+(?:(?:Mon(?:day)?|Tue(?:sday)?|Wed(?:nesday)?|Thu(?:rsday)?|Fri(?:day)?|Sat(?:urday)?|Sun(?:day)?|Jan(?:uary)?|Feb(?:ruary)?|Mar(?:ch)?|Apr(?:il)?|May|Jun(?:e)?|Jul(?:y)?|Aug(?:ust)?|Sep(?:t(?:ember)?)?|Oct(?:ober)?|Nov(?:ember)?|Dec(?:ember)?)\b|\d).*?(?:\b(?:19|20)\d{2}\b|\d{1,2}:\d{2}|\d{1,2}/\d{1,2})`)
var wroteSuffix = regexp.MustCompile(`(?i)\bwrote:\s*$`)

// readableMessageBody changes presentation only; the archived body is untouched.
func readableMessageBody(body string) string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	kept := make([]string, 0, len(lines))
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, ">") {
			continue
		}
		if line == "-- " {
			break
		}
		// Only complete single-line headers identify quoted history without guessing at authored text.
		if datedAttribution.MatchString(trimmed) && wroteSuffix.MatchString(trimmed) && quotedHistoryFollows(lines[i+1:]) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func quotedHistoryFollows(lines []string) bool {
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return strings.HasPrefix(trimmed, ">")
		}
	}
	return false
}

func searchSnippetText(text string) string {
	text = normalizeSearchTableText(text)
	if short := textutil.PrefixRunes(text, 160); short != text {
		return short + "…"
	}
	return text
}

func readableMessageDetail(message *query.MessageDetail) *query.MessageDetail {
	display := *message
	display.BodyText = readableMessageBody(message.BodyText)
	if message.BodyText != "" && display.BodyText == "" {
		display.Snippet = ""
	} else if message.BodyText == "" {
		display.Snippet = readableMessageBody(message.Snippet)
	}
	return &display
}
