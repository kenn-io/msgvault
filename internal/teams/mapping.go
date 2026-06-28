package teams

import (
	"database/sql"
	"net/url"
	"strings"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

// htmlToText converts an HTML string to plain text by delegating to
// mime.StripHTML, which strips tags, decodes entities, and normalises
// whitespace. It is a thin wrapper so tests can target it directly.
func htmlToText(html string) string {
	return mime.StripHTML(html)
}

func snippet(text string) string {
	r := []rune(text)
	if len(r) > 100 {
		return string(r[:100])
	}
	return text
}

// recordingLine renders a call-recording pointer for inclusion in the message
// body/snippet/FTS so the URL is visible and searchable.
func recordingLine(name, url string) string {
	if name != "" {
		return "📹 recording: " + name + " " + url
	}
	return "📹 recording: " + url
}

// mapMessage converts a Graph API ChatMessage into a store.Message and the
// plain-text body. conversationID and sourceID are the internal DB IDs.
func mapMessage(gm *ChatMessage, conversationID, sourceID int64, sourceMessageID string) (store.Message, string) {
	text := gm.Body.Content
	if strings.EqualFold(gm.Body.ContentType, "html") {
		text = htmlToText(gm.Body.Content)
	}
	attCount := 0
	for _, att := range gm.Attachments {
		if att.ContentURL != "" {
			attCount++
		}
	}
	attCount += len(hostedRe.FindAllString(gm.Body.Content, -1))
	if url, name, ok := gm.callRecording(); ok {
		line := recordingLine(name, url)
		if text != "" {
			text += "\n" + line
		} else {
			text = line
		}
		attCount++
	}
	msg := store.Message{
		ConversationID:  conversationID,
		SourceID:        sourceID,
		SourceMessageID: sourceMessageID,
		MessageType:     "teams",
		SentAt:          sql.NullTime{Time: gm.CreatedDateTime, Valid: !gm.CreatedDateTime.IsZero()},
		ReceivedAt:      sql.NullTime{Time: gm.CreatedDateTime, Valid: !gm.CreatedDateTime.IsZero()},
		Snippet:         sql.NullString{String: snippet(text), Valid: text != ""},
		HasAttachments:  attCount > 0,
		AttachmentCount: attCount,
	}
	if gm.Subject != "" {
		msg.Subject = sql.NullString{String: gm.Subject, Valid: true}
	}
	return msg, text
}

func chatSourceMessageID(chatID, messageID string) string {
	return "chat:" + escapeSourceIDPart(chatID) + ":" + escapeSourceIDPart(messageID)
}

func channelSourceMessageID(teamID, channelID, messageID string) string {
	return "channel:" + escapeSourceIDPart(teamID) + ":" + escapeSourceIDPart(channelID) + ":" + escapeSourceIDPart(messageID)
}

func escapeSourceIDPart(part string) string {
	return url.QueryEscape(part)
}

// conversationType maps a Graph API chatType string to the msgvault
// conversation type. "oneOnOne" becomes "direct_chat"; everything else
// (group, meeting, unknownFutureValue, …) becomes "group_chat".
func conversationType(chatType string) string {
	if chatType == "oneOnOne" {
		return "direct_chat"
	}
	return "group_chat"
}
