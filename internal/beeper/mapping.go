package beeper

import (
	"database/sql"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

// MessageType is the msgvault message_type for all Beeper-archived messages.
// The originating network (WhatsApp, Signal, …) is distinguished by the
// per-account source, not by message_type: Beeper's network set is open-ended,
// while message_type values live in fixed lists across the query layer.
const MessageType = "beeper"

func snippet(text string) string {
	r := []rune(text)
	if len(r) > 100 {
		return string(r[:100])
	}
	return text
}

// placeholderBody synthesizes a searchable body line for messages that carry
// no text (media, stickers, locations).
func placeholderBody(m *Message) string {
	switch m.Type {
	case "IMAGE":
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
	if text := strings.TrimSpace(m.Text); text != "" {
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
		MessageType:     MessageType,
		SentAt:          sql.NullTime{Time: m.Timestamp, Valid: !m.Timestamp.IsZero()},
		ReceivedAt:      sql.NullTime{Time: m.Timestamp, Valid: !m.Timestamp.IsZero()},
		IsFromMe:        m.IsSender,
		Snippet:         sql.NullString{String: snippet(text), Valid: text != ""},
		HasAttachments:  len(m.Attachments) > 0,
		AttachmentCount: len(m.Attachments),
	}
	return msg, text
}

// conversationType maps a Beeper chat type to the msgvault conversation type:
// "single" becomes "direct_chat"; everything else becomes "group_chat".
func conversationType(chatType string) string {
	if chatType == "single" {
		return "direct_chat"
	}
	return "group_chat"
}
