package beeper

import (
	"context"
	"encoding/json"
	"time"
)

// ---- Beeper Desktop API objects (see /v1/spec, "Beeper Client API") ----

// Account is a chat account connected to Beeper Desktop (one per network login).
type Account struct {
	AccountID string `json:"accountID"`
	Network   string `json:"network"` // human-friendly name; may be empty
	User      User   `json:"user"`
}

// User identifies a person on a network. Only fields the importer consumes
// are modelled; the archived raw JSON preserves everything else.
type User struct {
	ID          string `json:"id"` // Matrix-style user ID, e.g. "@signal_<uuid>:beeper.local"
	Username    string `json:"username"`
	PhoneNumber string `json:"phoneNumber"` // E.164 when present
	Email       string `json:"email"`
	FullName    string `json:"fullName"`
	IsSelf      bool   `json:"isSelf"`
}

// Participant is a chat member: a User plus membership metadata.
type Participant struct {
	User

	IsAdmin bool `json:"isAdmin"`
}

// ChatParticipants wraps the (possibly truncated) participant list of a chat.
type ChatParticipants struct {
	Items   []Participant `json:"items"`
	HasMore bool          `json:"hasMore"`
	Total   int           `json:"total"`
}

// Chat is a conversation on one account.
type Chat struct {
	ID           string           `json:"id"` // Matrix room ID, globally unique
	AccountID    string           `json:"accountID"`
	Network      string           `json:"network"`
	Title        string           `json:"title"`
	Type         string           `json:"type"` // "single" | "group"
	Participants ChatParticipants `json:"participants"`
	LastActivity time.Time        `json:"lastActivity"`
}

// Reaction is one participant's reaction to a message.
type Reaction struct {
	ID            string `json:"id"`
	ReactionKey   string `json:"reactionKey"`
	ParticipantID string `json:"participantID"`
	Emoji         bool   `json:"emoji"`
}

// Transcription is an attachment transcription (voice notes).
type Transcription struct {
	Transcription string `json:"transcription"`
}

// Attachment is a media attachment on a message. The id is typically an
// mxc:// URL fetchable via GET /v1/assets/serve?url=.
type Attachment struct {
	ID            string         `json:"id"`
	Type          string         `json:"type"` // unknown | img | video | audio
	SrcURL        string         `json:"srcURL"`
	MimeType      string         `json:"mimeType"`
	FileName      string         `json:"fileName"`
	FileSize      float64        `json:"fileSize"`
	IsGif         bool           `json:"isGif"`
	IsSticker     bool           `json:"isSticker"`
	IsVoiceNote   bool           `json:"isVoiceNote"`
	Duration      float64        `json:"duration"` // seconds
	Transcription *Transcription `json:"transcription"`
	Size          *struct {
		Width  float64 `json:"width"`
		Height float64 `json:"height"`
	} `json:"size"`
}

// Message is one message event. type distinguishes regular content
// (TEXT/NOTICE/media types/LOCATION) from REACTION events.
type Message struct {
	ID              string       `json:"id"` // numeric string, unique per installation
	ChatID          string       `json:"chatID"`
	AccountID       string       `json:"accountID"`
	SenderID        string       `json:"senderID"`
	SenderName      string       `json:"senderName"`
	Timestamp       time.Time    `json:"timestamp"`
	SortKey         string       `json:"sortKey"`
	Type            string       `json:"type"`
	Text            string       `json:"text"`
	EditedTimestamp *time.Time   `json:"editedTimestamp"`
	IsSender        bool         `json:"isSender"`
	IsHidden        bool         `json:"isHidden"`
	IsDeleted       bool         `json:"isDeleted"`
	Attachments     []Attachment `json:"attachments"`
	LinkedMessageID string       `json:"linkedMessageID"` // reply target (or reaction target for REACTION events)
	Mentions        []string     `json:"mentions"`        // mentioned user IDs or "@room"; null for legacy messages
	Reactions       []Reaction   `json:"reactions"`
	// Raw holds the exact original JSON for this message, captured during decode
	// (see UnmarshalJSON). It is archived verbatim so no API field is lost to
	// our partial struct modelling.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes a Message while retaining the exact original bytes in
// Raw, so the archived raw blob is lossless even for fields we do not model.
func (m *Message) UnmarshalJSON(b []byte) error {
	type alias Message // avoid recursion into this method
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*m = Message(a)
	m.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// ListMessagesOutput is the page envelope of GET /v1/chats/{chatID}/messages.
// Cursors are opaque; direction=before pages older, direction=after newer.
type ListMessagesOutput struct {
	Items        []Message `json:"items"`
	HasMore      bool      `json:"hasMore"`
	OldestCursor string    `json:"oldestCursor"`
	NewestCursor string    `json:"newestCursor"`
}

// SearchChatsOutput is the page envelope of GET /v1/chats/search.
type SearchChatsOutput struct {
	Items        []Chat `json:"items"`
	HasMore      bool   `json:"hasMore"`
	OldestCursor string `json:"oldestCursor"`
	NewestCursor string `json:"newestCursor"`
}

// ---- Importer options/summary ----

// ImportOptions configures one Import run for a single Beeper account
// (= one msgvault source).
type ImportOptions struct {
	// AccountID is the Beeper account to sync (e.g. "whatsapp", "signal").
	// It doubles as the msgvault source identifier.
	AccountID string
	// Limit caps the number of messages processed per chat (0 = no limit).
	// A limited backfill leaves the chat resumable, not complete.
	Limit int
	// Full ignores the prior sync cursor and any interrupted checkpoint so
	// every chat is re-walked and every message re-persisted (repair path).
	Full bool
	// ChatID restricts the run to a single chat (scoped runs/debugging).
	ChatID string
	// Progress, if non-nil, is called after each chat with a human-readable
	// status line. Safe to leave nil (silent mode).
	Progress func(msg string) `json:"-"`
	// EmbedEnqueuer, if non-nil, queues persisted message IDs for vector
	// embedding. Enqueue failures are counted but do not abort the import.
	EmbedEnqueuer EmbedEnqueuer `json:"-"`
}

type EmbedEnqueuer interface {
	EnqueueMessages(ctx context.Context, messageIDs []int64) error
}

type ImportSummary struct {
	Duration           time.Duration
	SourceID           int64
	ChatsProcessed     int64
	MessagesProcessed  int64
	MessagesAdded      int64
	ReactionsRefreshed int64
	HiddenSkipped      int64
	// FetchErrors counts Beeper API fetch failures (a subset of Errors). Any
	// fetch failure keeps the discovery watermark from advancing so the
	// affected chats are re-visited next run.
	FetchErrors int64
	Errors      int64
}
