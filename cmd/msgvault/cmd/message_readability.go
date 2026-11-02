package cmd

import (
	"go.kenn.io/msgvault/internal/msgsegment"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/textutil"
)

// readableMessageBody changes presentation only; the archived body is untouched.
func readableMessageBody(body string) string {
	return msgsegment.ReadableText(body)
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
	return &display
}

func messageTextBody(message *query.MessageDetail, stripQuoted bool) string {
	body := message.BodyText
	if body == "" {
		body = message.Snippet
	}
	if stripQuoted {
		body = readableMessageBody(body)
	}
	return body
}
