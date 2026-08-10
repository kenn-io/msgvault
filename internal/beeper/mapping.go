package beeper

import (
	"database/sql"
	"html"
	"regexp"
	"strings"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// messageType is the msgvault message_type for all Beeper-archived messages.
// The originating network (WhatsApp, Signal, …) is distinguished by the
// per-account source, not by message_type: Beeper's network set is open-ended,
// while message_type values live in fixed lists across the query layer.
const messageType = "beeper"

// htmlElementRe matches an opening or closing tag of an HTML element Beeper
// actually emits in message text. Matching a known element name (rather than
// any "<...>") keeps ordinary prose containing angle brackets — "a < b", "<3",
// "see <name> below" — from being mistaken for markup and mangled.
var htmlElementRe = regexp.MustCompile(`(?i)</?(a|b|i|u|s|p|br|em|strong|del|code|pre|span|div|img|ul|ol|li|h[1-6]|blockquote|font|table|tr|td|th|mx-reply)(\s[^<>]*)?/?>`)

// mxReplyFallbackRe removes Matrix's quoted reply fallback before generic HTML
// conversion. The block repeats the parent message for clients that do not
// understand reply relations; keeping it would index the parent as new text.
var mxReplyFallbackRe = regexp.MustCompile(`(?is)<mx-reply(?:\s[^<>]*)?>.*?</mx-reply\s*>`)

// htmlEntityRe matches the named and numeric entities that show up in
// otherwise tag-free text (e.g. "at 2:15&amp;k?"). A bare "&" is left alone,
// so plain messages like "Help & About" are never rewritten.
var htmlEntityRe = regexp.MustCompile(`&(amp|lt|gt|quot|apos|nbsp|#\d+|#x[0-9a-fA-F]+);`)

// plainText renders a Beeper message's text as plain text.
//
// The API's `text` field is HTML for a large minority of messages (formatted
// Matrix messages, Telegram custom emoji, link previews) and genuinely plain
// for the rest, with no field distinguishing the two — so the shape of the
// value has to be detected before converting. Storing it verbatim puts markup
// into message bodies, snippets and the FTS index, where `target="_blank"`
// swamps searches for the ordinary word "target".
//
// The archived raw JSON (raw_format = beeper_json) keeps the original HTML, so
// this conversion is never lossy for the archive itself.
func plainText(s string) string {
	s = mxReplyFallbackRe.ReplaceAllString(s, "")
	var text string
	switch {
	case htmlElementRe.MatchString(s):
		text = mime.StripHTML(s)
	case htmlEntityRe.MatchString(s):
		text = html.UnescapeString(s)
	default:
		text = s
	}
	return textutil.SanitizeTerminalMultiline(text)
}

func snippet(text string) string {
	r := []rune(text)
	if len(r) > 100 {
		return string(r[:100])
	}
	return text
}

// typeImage is the Beeper message type for a photo — including the link
// previews that arrive typed as images rather than as the media they preview.
const typeImage = "IMAGE"

// placeholderBody synthesizes a searchable body line for messages that carry
// no text (media, stickers, locations).
func placeholderBody(m *Message) string {
	switch m.Type {
	case typeImage:
		return "[image]"
	case "VIDEO":
		return "[video]"
	case "VOICE":
		return "[voice message]"
	case "AUDIO":
		return "[audio]"
	case "STICKER":
		return "[sticker]"
	case "LOCATION":
		return "[location]"
	case "FILE":
		for _, att := range m.Attachments {
			if att.FileName != "" {
				return "[file: " + att.FileName + "]"
			}
		}
		return "[file]"
	default:
		return ""
	}
}

// bodyText renders the plain-text body: the message text (or a placeholder
// for text-less media), plus any voice-note transcriptions so they are
// visible to FTS and embeddings.
func bodyText(m *Message) string {
	var parts []string
	if text := strings.TrimSpace(plainText(m.Text)); text != "" {
		parts = append(parts, text)
	} else if ph := placeholderBody(m); ph != "" {
		parts = append(parts, ph)
	}
	for _, att := range m.Attachments {
		if att.Transcription != nil && strings.TrimSpace(att.Transcription.Transcription) != "" {
			parts = append(parts, "🎤 transcript: "+strings.TrimSpace(att.Transcription.Transcription))
		}
	}
	return strings.Join(parts, "\n")
}

// urlOnlyRe matches a body consisting of exactly one URL and nothing else.
var urlOnlyRe = regexp.MustCompile(`^https?://\S+$`)

// sharedLink reports the URL a message is forwarding, when the message is a
// link share rather than something the sender composed: its whole body is a
// single URL and it carries an attachment, which is the shape every network
// uses for a link preview (an Instagram reel, an x.com post, a GitHub link).
//
// The distinction matters for storage accounting. A forwarded reel and a photo
// a friend took are both media rows, but the reel's bytes are a preview of
// somebody else's public post — recoverable from the URL — while the photo is
// not. Recording the share URL lets those be told apart after the fact without
// re-reading every archived blob.
func sharedLink(m *Message) string {
	if len(m.Attachments) == 0 {
		return ""
	}
	text := strings.TrimSpace(plainText(m.Text))
	if !urlOnlyRe.MatchString(text) {
		return ""
	}
	return text
}

// mapMessage converts a Beeper API Message into a store.Message plus the
// plain-text body. conversationID and sourceID are internal DB IDs. The
// message's own numeric id is the source_message_id (unique per installation;
// guarded by the SyncState anchor probe).
func mapMessage(m *Message, conversationID, sourceID int64) (store.Message, string) {
	text := bodyText(m)
	msg := store.Message{
		ConversationID:  conversationID,
		SourceID:        sourceID,
		SourceMessageID: m.ID,
		MessageType:     messageType,
		SentAt:          sql.NullTime{Time: m.Timestamp, Valid: !m.Timestamp.IsZero()},
		ReceivedAt:      sql.NullTime{Time: m.Timestamp, Valid: !m.Timestamp.IsZero()},
		IsFromMe:        m.IsSender,
		Snippet:         sql.NullString{String: snippet(text), Valid: text != ""},
		HasAttachments:  len(m.Attachments) > 0,
		AttachmentCount: len(m.Attachments),
	}
	return msg, text
}

// chatTypeSingle is the Beeper chat type for direct messages (vs "group").
const chatTypeSingle = "single"

// conversationType maps a Beeper chat type to the msgvault conversation type:
// "single" becomes "direct_chat"; everything else becomes "group_chat".
func conversationType(chatType string) string {
	if chatType == chatTypeSingle {
		return "direct_chat"
	}
	return "group_chat"
}
