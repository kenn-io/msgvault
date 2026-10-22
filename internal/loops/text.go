package loops

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/vector/preprocess"
)

const replyAttribution = `(?:on [^\n]*\bwrote:|on [^\n]*,[ \t]*\r?\n(?:[^\n]*\r?\n){0,2}?[^\n]*\bwrote:)`

var (
	// quotedReply drops a wrapped attribution and its quoted lines, keeping
	// any authored reply above or below them.
	quotedReply = regexp.MustCompile(`(?im)^[ \t]*` + replyAttribution + `[ \t]*\r?\n(?:[ \t]*\r?\n)*(?:[ \t]*>+[ \t]?.*(?:\r?\n|$))+`)
	// quotedStart is where the quoted part begins: a reply attribution that
	// may wrap onto a second line, an "Original Message" or "Forwarded
	// message" separator, or Outlook's From:/Sent: header pair.
	quotedStart = regexp.MustCompile(`(?im)^[ \t]*(?:` + replyAttribution + `[ \t\r]*$|-{2,}\s*(?:original message|forwarded message)\b|begin forwarded message:\s*$|from:[ \t][^\n]*\n[ \t]*sent:[ \t])`)
	textURL     = regexp.MustCompile(`(?i)(?:https?://|www\.)\S+|(?:[a-z0-9-]+\.)+[a-z]{2,}(?:[/?#]\S*)?`)
	// English only: a request is a question mark, "can you", "let me know",
	// or "please" plus an action verb.
	request = regexp.MustCompile(`(?i)\?|\b(?:(?:can|could|would|will)\s+you|let\s+(?:me|us)\s+know|lmk|please\s+(?:(?:kindly|just)\s+)?(?:send|confirm|review|reply|advise|share|check|call))\b`)

	acknowledgement = regexp.MustCompile(`(?i)^(?:(?:thanks|thank you|thx|ty|many thanks|got it|noted|received|ack|ok|okay|cheers|perfect|great)[\s!.,]*)+$`)
)

// ownText is what the sender wrote: quotedReply drops wrapped quotations,
// preprocess strips quotes and signatures, and unquotedText cuts the rest.
func ownText(body string) string {
	text, _ := preprocess.Preprocess("", quotedReply.ReplaceAllString(body, ""), 0, preprocess.Config{StripQuotes: true, StripSignatures: true})
	return unquotedText(text)
}

// unquotedText stops where the quoted part begins.
func unquotedText(body string) string {
	if loc := quotedStart.FindStringIndex(body); loc != nil {
		return body[:loc[0]]
	}
	return body
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
