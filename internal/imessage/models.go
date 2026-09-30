// Package imessage reads from macOS's iMessage chat.db and imports
// messages into the msgvault store.
package imessage

// messageRow holds a row from the iMessage chat.db message table
// joined with chat and handle info.
type messageRow struct {
	ROWID           int64
	GUID            string
	Text            *string // nullable - some messages only have attributedBody
	AttributedBody  []byte  // NSKeyedArchiver blob; fallback when Text is nil (macOS Ventura+)
	Date            int64   // Apple epoch timestamp (seconds or nanoseconds)
	IsFromMe        int
	Service         *string // "iMessage", "SMS", or NULL for system messages
	HasAttachments  int
	HandleID        *string // handle.id (phone or email), NULL for is_from_me
	ChatROWID       *int64  // chat.ROWID for participant lookup
	ChatGUID        *string // chat.guid, used as conversation/thread ID
	ChatDisplayName *string // chat.display_name (set for group chats)
	ChatIdentifier  *string // chat.chat_identifier
}

// ImportSummary holds statistics from a completed import run.
type ImportSummary struct {
	MessagesImported      int
	ConversationsImported int
	ParticipantsResolved  int
	// DatesCleared counts existing archived dates removed for invalid source timestamps.
	DatesCleared int
	Skipped      int
}
