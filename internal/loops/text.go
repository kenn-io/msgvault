// Package loops derives unanswered conversation turns from archived evidence.
package loops

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	replyAttribution = regexp.MustCompile(`(?i)^On .+ wrote:$`)
	textURL          = regexp.MustCompile(`(?i)https?://\S+|www\.\S+`)
	// Keep single-word names case-insensitive; later name words require capitals
	// so a short answer such as "Thanks, I will send" remains substantive.
	acknowledgement = regexp.MustCompile(`^(?i:((thanks|thank you|thx|ty|many thanks|got it|noted|received|ack|ok|okay|sounds good|cheers|perfect|great)[\s!.,]*)+)(,?\s*(?i:\p{Lu}\p{Ll}{0,19})(?:\s+(?:(?i:and|&)\s+)?\p{Lu}\p{Ll}{0,19}){0,2}[\s!.]*)?$`)
)

func unquotedText(body string) string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if line == "-- " || replyAttribution.MatchString(line) {
			break
		}
		if strings.HasPrefix(line, ">") {
			continue
		}
		kept = append(kept, line)
	}
	return textURL.ReplaceAllString(strings.Join(kept, "\n"), "")
}

func asksQuestion(body string) bool {
	return strings.Contains(unquotedText(body), "?")
}

func isAcknowledgementOnly(body string) bool {
	text := strings.Join(strings.Fields(unquotedText(body)), " ")
	return utf8.RuneCountInString(text) <= 60 && acknowledgement.MatchString(text)
}
