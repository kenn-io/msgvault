package query

import (
	"slices"
	"strconv"
	"strings"
)

// Chat classification shared by the Go and SQL entry-key builders. Messages
// whose type is a known text/chat type — or whose type is ambiguous while the
// containing conversation is chat-shaped — collapse into one conversation
// entry; every other message stays its own entry.
const conversationTypeDirectChat = "direct_chat"

var (
	chatFallbackMessageTypes = []string{"", "chat", "text"}
	chatConversationTypes    = []string{conversationTypeDirectChat, "group_chat", "channel", "chat"}
)

// EntryKeyFacts carries the archive identities that determine the canonical
// explore entry key of one message.
type EntryKeyFacts struct {
	SourceID         int64
	SourceMessageID  string
	MessageID        int64
	ConversationID   int64
	MessageType      string
	ConversationType string
}

// EntryKey returns the canonical explore entry key for the facts:
// "source:<source-id>:conversation:<conversation-id>" for chat-classified
// messages, otherwise "source:<source-id>:message:<source-message-id>" with
// the internal message ID as the fallback when the source assigned no ID.
// It must stay equivalent to sqlEntryKeyExpr, which renders the same key
// inside DuckDB; TestGetFileEntryKeyMatchesExploreProducedKey pins that
// equivalence through the HTTP API.
func (f EntryKeyFacts) EntryKey() string {
	source := strconv.FormatInt(f.SourceID, 10)
	if IsChatEntry(f.MessageType, f.ConversationType) {
		return "source:" + source + ":conversation:" + strconv.FormatInt(f.ConversationID, 10)
	}
	messageID := f.SourceMessageID
	if messageID == "" {
		messageID = strconv.FormatInt(f.MessageID, 10)
	}
	return "source:" + source + ":message:" + messageID
}

// IsChatEntry reports whether a message is grouped under its conversation
// entry. It is the Go equivalent of sqlIsChatPredicate.
func IsChatEntry(messageType, conversationType string) bool {
	messageType = strings.ToLower(messageType)
	if slices.Contains(TextMessageTypes, messageType) {
		return true
	}
	return slices.Contains(chatFallbackMessageTypes, messageType) &&
		slices.Contains(chatConversationTypes, strings.ToLower(conversationType))
}

// sqlQuotedList renders trusted package constants as a quoted SQL IN-list.
func sqlQuotedList(values []string) string {
	return "'" + strings.Join(values, "','") + "'"
}

// sqlMessageEntryKeyExpr renders the per-message entry key. alias prefixes
// the source_id, source_message_id, and message_id columns ("" or "s.").
func sqlMessageEntryKeyExpr(alias string) string {
	return "'source:' || CAST(" + alias + "source_id AS VARCHAR) || ':message:' || " +
		"COALESCE(NULLIF(" + alias + "source_message_id, ''), CAST(" + alias + "message_id AS VARCHAR))"
}

// sqlConversationEntryKeyExpr renders the chat conversation entry key.
func sqlConversationEntryKeyExpr(alias string) string {
	return "'source:' || CAST(" + alias + "source_id AS VARCHAR) || ':conversation:' || " +
		"CAST(" + alias + "conversation_id AS VARCHAR)"
}

// sqlEntryKeyExpr renders the canonical entry key of one message row: the
// conversation key for chat-classified rows, the message key otherwise.
// Keep equivalent to EntryKeyFacts.EntryKey.
func sqlEntryKeyExpr(alias string) string {
	return "CASE WHEN " + sqlIsChatPredicate(alias+"message_type", alias+"conversation_type") +
		"\n\t\tTHEN " + sqlConversationEntryKeyExpr(alias) +
		"\n\t\tELSE " + sqlMessageEntryKeyExpr(alias) + " END"
}
