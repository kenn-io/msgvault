package cmd

import (
	"strings"

	"go.kenn.io/msgvault/internal/msgsegment"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/textutil"
	"go.kenn.io/msgvault/internal/vector/embed"
)

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
		if msgsegment.IsDatedAttribution(trimmed) && msgsegment.QuotedHistoryFollows(lines[i+1:]) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
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

func messageTextBody(message *query.MessageDetail, stripQuoted bool) (string, bool) {
	body := embed.BodyTextForEmbedding(message.BodyText, message.BodyHTML)
	hasBody := body != ""
	if !hasBody {
		body = message.Snippet
	}
	if stripQuoted {
		body = readableMessageBody(body)
	}
	return body, hasBody
}
