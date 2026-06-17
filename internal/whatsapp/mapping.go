package whatsapp

import (
	"database/sql"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/store"
)

// isGroupChat returns true if the chat represents a group conversation.
// A chat is a group if group_type > 0 OR if the JID server is "g.us".
// Some groups (e.g. WhatsApp Communities and their sub-groups) have
// group_type = 0 despite being groups; the JID server is the
// definitive signal.
func isGroupChat(chat waChat) bool {
	return chat.GroupType > 0 || chat.Server == "g.us"
}

// mapConversation maps a WhatsApp chat to a msgvault conversation.
// Returns the source_conversation_id, conversation_type, and title.
func mapConversation(chat waChat) (sourceConvID, convType, title string) {
	sourceConvID = chat.RawString

	if isGroupChat(chat) {
		convType = "group_chat"
		if chat.Subject.Valid {
			title = chat.Subject.String
		}
	} else {
		convType = "direct_chat"
		// No title for direct chats (resolved via participant lookup)
	}

	return sourceConvID, convType, title
}

// mapMessage maps a WhatsApp message to a msgvault Message struct.
// The conversationID and sourceID must be resolved before calling.
func mapMessage(msg waMessage, conversationID, sourceID int64, senderID sql.NullInt64) store.Message {
	sentAt := sql.NullTime{}
	if msg.Timestamp > 0 {
		// WhatsApp timestamps are in milliseconds since epoch.
		sentAt = sql.NullTime{
			Time:  time.Unix(msg.Timestamp/1000, (msg.Timestamp%1000)*1e6),
			Valid: true,
		}
	}

	snippet := sql.NullString{}
	if msg.TextData.Valid && msg.TextData.String != "" {
		s := msg.TextData.String
		if utf8.RuneCountInString(s) > 100 {
			// Truncate to 100 runes, preserving multi-byte characters.
			runes := []rune(s)
			s = string(runes[:100])
		}
		snippet = sql.NullString{String: s, Valid: true}
	}

	return store.Message{
		ConversationID:  conversationID,
		SourceID:        sourceID,
		SourceMessageID: msg.KeyID,
		MessageType:     "whatsapp",
		SentAt:          sentAt,
		SenderID:        senderID,
		IsFromMe:        msg.FromMe == 1,
		Snippet:         snippet,
		HasAttachments:  isMediaType(msg.MessageType),
		AttachmentCount: boolToInt(isMediaType(msg.MessageType)),
		ArchivedAt:      time.Now(),
	}
}

// mapMediaType maps a WhatsApp message_type integer to a media type string.
// Returns empty string for non-media types.
func mapMediaType(waMessageType int) string {
	switch waMessageType {
	case 1:
		return "image"
	case 2:
		return "video"
	case 3:
		return "audio"
	case 4:
		return "gif"
	case 5:
		return "voice_note"
	case 13:
		return "document"
	case 90:
		return "sticker"
	default:
		return ""
	}
}

// isMediaType returns true if the WhatsApp message_type represents media.
func isMediaType(waMessageType int) bool {
	return mapMediaType(waMessageType) != ""
}

// isSkippedType returns true if the message type should be skipped during import.
// System messages, calls, locations, contacts, and polls are not imported.
func isSkippedType(waMessageType int) bool {
	switch waMessageType {
	case 7: // system message
		return true
	case 9: // location share
		return true
	case 10: // contact card
		return true
	case 15: // voice/video call
		return true
	case 64: // call (missed)
		return true
	case 66: // call (group)
		return true
	case 99: // poll
		return true
	case 11: // status/story
		return true
	default:
		return false
	}
}

// normalizePhone normalizes a WhatsApp JID user+server to an E.164 phone number.
// Input: user="447700900000", server="s.whatsapp.net"
// Output: "+447700900000"
// Returns empty string for non-phone JIDs (e.g., lid:..., status@broadcast).
func normalizePhone(user, server string) string {
	if user == "" {
		return ""
	}

	// Strip the @server suffix if present in user.
	user = strings.TrimSuffix(user, "@"+server)

	// Already in E.164 format?
	if strings.HasPrefix(user, "+") {
		return user
	}

	// Reject non-numeric JID users (e.g., "lid:123", "status", broadcast addresses).
	// Valid phone numbers contain only digits.
	for _, c := range user {
		if c < '0' || c > '9' {
			return ""
		}
	}

	// Must be at least a few digits to be a plausible phone number,
	// and no more than 15 (E.164 max) to prevent data pollution.
	if len(user) < 4 || len(user) > 15 {
		return ""
	}

	// Prepend + for E.164.
	return "+" + user
}

// resolveLidSender resolves a "lid" JID sender to a phone number via the
// jid_map lookup table. Only activates when the sender's server is "lid".
// Returns a normalised E.164 phone number, or empty string if unresolvable.
func resolveLidSender(jidRowID sql.NullInt64, server string, lidMap map[int64]waLidMapping) string {
	if server != "lid" || !jidRowID.Valid {
		return ""
	}
	mapping, ok := lidMap[jidRowID.Int64]
	if !ok {
		return ""
	}
	return normalizePhone(mapping.PhoneUser, mapping.PhoneServer)
}

// reactionTypeEmoji is the only reaction_type WhatsApp exports; every
// reaction is an emoji.
const reactionTypeEmoji = "emoji"

// mapReaction returns the reaction_value for a WhatsApp reaction. The
// reaction_type is always reactionTypeEmoji, so callers supply it directly.
func mapReaction(r waReaction) (reactionValue string) {
	if r.ReactionValue.Valid && r.ReactionValue.String != "" {
		return r.ReactionValue.String
	}
	return ""
}

// mapGroupRole maps a WhatsApp admin level to a conversation participant role.
func mapGroupRole(admin int) string {
	switch admin {
	case 1:
		return "admin"
	case 2:
		return "admin" // superadmin → admin (msgvault doesn't distinguish)
	default:
		return "member"
	}
}

// chatTitle returns a display title for a chat for progress reporting.
func chatTitle(chat waChat) string {
	if chat.Subject.Valid && chat.Subject.String != "" {
		return chat.Subject.String
	}
	if chat.User != "" {
		return normalizePhone(chat.User, chat.Server)
	}
	return chat.RawString
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
