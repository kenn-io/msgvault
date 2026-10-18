// Package loops derives unanswered conversation turns from archived evidence.
package loops

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const forwardedMessageMarker = "---------- Forwarded message ----------"

var (
	replyAttribution         = regexp.MustCompile(`(?i)^On .+ wrote:$`)
	textURL                  = regexp.MustCompile(`(?i)(?:https?://|www\.)\S+|(?:[a-z0-9-]+\.)+[a-z]{2,}(?:[/?#]\S*)?`)
	acknowledgementWords     = `(?:thank you very much|thanks a lot|thanks so much|many many thanks|thanks|thank you|thx|ty|many thanks|got it|noted|received|ack|ok|okay|sounds good|cheers|perfect|great)`
	acknowledgement          = regexp.MustCompile(`(?i)^(?:` + acknowledgementWords + `[\s!.,]*)+$`)
	acknowledgementWithName  = regexp.MustCompile(`(?i)^((?:(?:` + acknowledgementWords + `)[\s!.,]*)+),?\s*(.+?)[\s!.]*$`)
	acknowledgementSeparator = regexp.MustCompile(`(?i)\s+(?:and|&)\s+`)
)

func unquotedText(body string) string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	kept := make([]string, 0, len(lines))
	inAttributedQuote := false
	quotedTextSeen := false
	quotedLineSeen := false
	// Attributions start quote tracking so bottom-posted and inline replies survive.
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if line == "-- " || strings.EqualFold(trimmed, forwardedMessageMarker) {
			break
		}
		if replyAttribution.MatchString(trimmed) {
			inAttributedQuote = true
			quotedTextSeen = false
			quotedLineSeen = false
			continue
		}
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), ">") {
			if inAttributedQuote {
				quotedTextSeen = true
				quotedLineSeen = true
			}
			continue
		}
		if inAttributedQuote {
			if trimmed == "" {
				if quotedTextSeen {
					inAttributedQuote = false
					quotedTextSeen = false
					quotedLineSeen = false
				}
				continue
			}
			if quotedLineSeen {
				inAttributedQuote = false
			} else {
				quotedTextSeen = true
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func asksQuestion(body string) bool {
	return strings.Contains(stripURLTokens(unquotedText(body)), "?")
}

func stripURLTokens(text string) string {
	return textURL.ReplaceAllStringFunc(text, func(raw string) string {
		trimmed := strings.TrimRight(raw, "!?.,;:)]}\"'")
		return raw[len(trimmed):]
	})
}

func isAcknowledgementOnly(body string, participants []LoopParticipant) bool {
	text := strings.Join(strings.Fields(unquotedText(body)), " ")
	if utf8.RuneCountInString(text) > 60 {
		return false
	}
	if strings.Contains(stripURLTokens(text), "?") {
		return false
	}
	if acknowledgement.MatchString(text) {
		return true
	}
	match := acknowledgementWithName.FindStringSubmatch(text)
	return len(match) == 3 && participantNameSuffix(match[2], participants)
}

func participantNameSuffix(suffix string, participants []LoopParticipant) bool {
	suffix = strings.TrimSpace(strings.Trim(suffix, " \t.!?,"))
	names := acknowledgementSeparator.Split(suffix, -1)
	if suffix == "" || len(names) > 3 {
		return false
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		matched := false
		for _, participant := range participants {
			words := strings.Fields(strings.TrimSpace(participant.DisplayName))
			if len(words) == 0 {
				continue
			}
			if strings.EqualFold(name, participant.DisplayName) || strings.EqualFold(name, words[0]) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
