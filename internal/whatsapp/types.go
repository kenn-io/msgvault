// Package whatsapp provides import functionality for WhatsApp message backups.
// It reads from a decrypted WhatsApp msgstore.db (SQLite) and maps messages
// into the msgvault unified schema.
package whatsapp

import (
	"database/sql"
	"time"
)

// waChat represents a chat/conversation from the WhatsApp jid + chat tables.
type waChat struct {
	RowID                int64          // chat._id
	JIDRowID             int64          // chat.jid_row_id → jid._id
	RawString            string         // jid.raw_string (e.g., "447700900000@s.whatsapp.net")
	User                 string         // jid.user (phone number part)
	Server               string         // jid.server (s.whatsapp.net or g.us)
	Subject              sql.NullString // chat.subject (group name)
	GroupType            int            // chat.group_type: 0=individual (but see Server), >0=group
	Hidden               int            // chat.hidden
	LastMessageTimestamp int64          // chat.sort_timestamp
}

// waMessage represents a message from the WhatsApp message table.
type waMessage struct {
	RowID           int64          // message._id
	ChatRowID       int64          // message.chat_row_id
	FromMe          int            // message.from_me (0=received, 1=sent)
	KeyID           string         // message.key_id (unique message ID)
	SenderJIDRowID  sql.NullInt64  // message.sender_jid_row_id → jid._id
	SenderRawString sql.NullString // jid.raw_string of sender
	SenderUser      sql.NullString // jid.user of sender
	SenderServer    sql.NullString // jid.server of sender
	Timestamp       int64          // message.timestamp (ms since epoch)
	MessageType     int            // message.message_type
	TextData        sql.NullString // message.text_data
	Status          int            // message.status
	Starred         int            // message.starred
}

// waMedia represents media metadata from the message_media table.
type waMedia struct {
	MessageRowID  int64          // message_media.message_row_id
	MimeType      sql.NullString // message_media.mime_type
	MediaCaption  sql.NullString // message_media.media_caption
	FileSize      sql.NullInt64  // message_media.file_size
	FilePath      sql.NullString // message_media.file_path
	Width         sql.NullInt64  // message_media.width
	Height        sql.NullInt64  // message_media.height
	MediaDuration sql.NullInt64  // message_media.media_duration (seconds)
}

// waReaction represents a reaction from the message_add_on table.
type waReaction struct {
	MessageRowID    int64          // FK to message._id
	SenderJIDRowID  sql.NullInt64  // jid of reactor
	SenderRawString sql.NullString // jid.raw_string
	SenderUser      sql.NullString // jid.user
	SenderServer    sql.NullString // jid.server
	ReactionValue   sql.NullString // emoji character
	Timestamp       int64          // timestamp (ms)
}

// waGroupMember represents a member of a group chat.
type waGroupMember struct {
	GroupJID     string // group_participants.gjid (text, raw JID string)
	MemberJID    string // group_participants.jid (text, raw JID string)
	MemberUser   string // jid.user (parsed from MemberJID)
	MemberServer string // jid.server (parsed from MemberJID)
	Admin        int    // group_participants.admin (0=member, 1=admin, 2=superadmin)
}

// waQuoted represents a quoted/replied-to message reference.
type waQuoted struct {
	MessageRowID int64  // the message that quotes
	QuotedKeyID  string // message_quoted.key_id of the quoted message
}

// waLidMapping maps a "lid" JID row to its corresponding phone JID,
// populated from the WhatsApp jid_map table.
type waLidMapping struct {
	LidRowID    int64  // jid._id for the lid entry
	PhoneUser   string // jid.user for the phone entry (e.g., "447700900000")
	PhoneServer string // jid.server for the phone entry (e.g., "s.whatsapp.net")
}

// ImportOptions configures the WhatsApp import process.
type ImportOptions struct {
	// Phone is the user's own phone number in E.164 format (e.g., "+447700900000").
	Phone string

	// DisplayName is an optional display name for the user.
	DisplayName string

	// MediaDir is an optional path to the decrypted Media folder.
	// If set, media files will be copied to content-addressed storage.
	MediaDir string

	// AttachmentsDir is the root directory for content-addressed attachment storage.
	// This should be cfg.AttachmentsDir() (e.g., ~/.msgvault/attachments/).
	// Required when MediaDir is set.
	AttachmentsDir string

	// MaxMediaFileSize is the maximum size of a single media file to copy (in bytes).
	// Files larger than this are skipped. Default: 100MB.
	MaxMediaFileSize int64

	// Limit limits the number of messages imported (0 = no limit, for testing).
	Limit int

	// BatchSize is the number of messages to process per batch (default: 1000).
	BatchSize int
}

// DefaultOptions returns ImportOptions with sensible defaults.
func DefaultOptions() ImportOptions {
	return ImportOptions{
		BatchSize:        1000,
		MaxMediaFileSize: 100 * 1024 * 1024, // 100MB
	}
}

// ImportSummary holds statistics from a completed import.
type ImportSummary struct {
	Duration          time.Duration
	SourceID          int64
	ChatsProcessed    int64
	MessagesProcessed int64
	MessagesAdded     int64
	MessagesSkipped   int64
	ReactionsAdded    int64
	AttachmentsFound  int64
	MediaCopied       int64
	Participants      int64
	Errors            int64
}

// ImportProgress provides callbacks for import progress reporting.
type ImportProgress interface {
	OnStart()
	OnChatStart(chatJID string, chatTitle string, messageCount int)
	OnProgress(processed, added, skipped int64)
	OnChatComplete(chatJID string, messagesAdded int64)
	OnComplete(summary *ImportSummary)
	OnError(err error)
}

// NullProgress is a no-op implementation of ImportProgress.
type NullProgress struct{}

func (NullProgress) OnStart()                        {}
func (NullProgress) OnChatStart(string, string, int) {}
func (NullProgress) OnProgress(int64, int64, int64)  {}
func (NullProgress) OnChatComplete(string, int64)    {}
func (NullProgress) OnComplete(*ImportSummary)       {}
func (NullProgress) OnError(error)                   {}
