package loops

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/vector/preprocess"
)

var (
	textURL = regexp.MustCompile(`(?i)(?:https?://|www\.)\S+|(?:[a-z0-9-]+\.)+[a-z]{2,}(?:[/?#]\S*)?`)
	// English only: a request is a question mark, "can you", "let me know",
	// or "please" plus an action verb.
	request = regexp.MustCompile(`(?i)\?|\b(?:(?:can|could|would|will)\s+you|let\s+(?:me|us)\s+know|lmk|please\s+(?:(?:kindly|just)\s+)?(?:send|confirm|review|reply|advise|share|check|call))\b`)

	acknowledgement = regexp.MustCompile(`(?i)^(?:(?:thanks|thank you|thx|ty|many thanks|got it|noted|received|ack|ok|okay|cheers|perfect|great)[\s!.,]*)+$`)
)

// ownText removes loop-specific quotations while preserving replies above and below them.
func ownText(body string) string {
	text, _ := preprocess.Preprocess("", preprocess.StripReplyQuotes(body), 0, preprocess.Config{StripSignatures: true})
	return text
}

// asks reports whether the sender's own text asks a question or makes a request.
func asks(text string) bool {
	return request.MatchString(stripURLTokens(text))
}

// stripURLTokens drops a URL's own characters but keeps trailing punctuation,
// so "?" inside a link is not a question.
func stripURLTokens(text string) string {
	return textURL.ReplaceAllStringFunc(text, func(raw string) string {
		trimmed := strings.TrimRight(raw, "!?.,;:)]}\"'")
		return raw[len(trimmed):]
	})
}

func isAcknowledgementOnly(text string) bool {
	text = strings.Join(strings.Fields(text), " ")
	return utf8.RuneCountInString(text) <= 60 && acknowledgement.MatchString(text)
}
