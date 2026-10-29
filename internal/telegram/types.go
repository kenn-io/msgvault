// Package telegram imports a Telegram Desktop "Machine-readable JSON"
// export into an msgvault archive.
package telegram

import "time"

// SourceType is the archive source type written by this importer.
const SourceType = "telegram"

// MessageType is the messages.message_type value written by this importer.
const MessageType = "telegram"

// chatType names the raw chat "type" values Telegram Desktop documents for
// its machine-readable JSON export.
const (
	chatTypePersonalChat      = "personal_chat"
	chatTypeBotChat           = "bot_chat"
	chatTypePrivateGroup      = "private_group"
	chatTypePrivateSupergroup = "private_supergroup"
	chatTypePublicSupergroup  = "public_supergroup"
	chatTypeSavedMessages     = "saved_messages"
	chatTypePrivateChannel    = "private_channel"
	chatTypePublicChannel     = "public_channel"
	// chatTypeReplies is the "Replies" pseudo-chat: the thread list where
	// channel-post comments and discussion replies land. Its entries are
	// group-style discussions, so ConvType maps it with the groups.
	chatTypeReplies = "replies"
)

// Chat is one Telegram chat or channel from an export.
type Chat struct {
	// ID is Telegram's decimal chat ID. Group chats are negative, channels
	// and supergroups carry -100 prefixes; all fit int64.
	ID int64
	// Type is the raw export chat type (personal_chat, private_group, …).
	Type string
	// Name is the chat title. Personal chats carry the peer's display name.
	Name string
	// Messages are the chat's non-service and service messages in export
	// order.
	Messages []Message
}

// ConvType maps the export chat type onto an archive conversation type.
// Every documented type maps explicitly; an unknown value falls back to the
// generic "chat".
func (c *Chat) ConvType() string {
	switch c.Type {
	case chatTypePersonalChat, chatTypeBotChat, chatTypeSavedMessages:
		return "direct_chat"
	case chatTypePrivateGroup, chatTypePrivateSupergroup, chatTypePublicSupergroup,
		chatTypeReplies:
		return "group_chat"
	case chatTypePrivateChannel, chatTypePublicChannel:
		return "channel"
	default:
		return "chat"
	}
}

// Message is one entry of a chat's messages array.
type Message struct {
	// ID is Telegram's message ID, unique inside its chat.
	ID int64
	// Service marks a service message ("type": "service"): membership
	// changes, calls, pins. The importer drops them.
	Service bool
	// SentAt is the message timestamp; zero when the export carries none.
	SentAt time.Time
	// ReplyTo is the export's reply_to_message_id: the Telegram message ID
	// this message replies to inside the same chat, 0 when it is not a
	// reply.
	ReplyTo int64
	// Edited reports that the export carried an edited or edited_unixtime
	// field for this message.
	Edited bool
	// FromName is the sender's display name ("from").
	FromName string
	// FromID is the sender's raw prefixed ID ("from_id"), e.g. "user482111".
	// Empty for messages with no sender (e.g. channel posts in some
	// exports, saved-messages posts).
	FromID string
	// Text is the flattened message text: the concatenation of text or
	// text_entities parts in export order, with no inserted separators.
	Text string
	// Photo, File, and Thumbnail are export-relative media paths.
	Photo     string
	File      string
	Thumbnail string
	// MediaType is the export's media_type (photo, voice_message, sticker…)
	// for media messages; empty for plain text.
	MediaType string
	// MimeType is the export's mime_type for a file message; empty when the
	// export carries none.
	MimeType string
	// FileName is the export's official file_name for a file message; empty
	// when the export carries none.
	FileName string
	// Title is the export's title for a file message (its filename).
	Title string
}
