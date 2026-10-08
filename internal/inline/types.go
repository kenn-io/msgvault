// Package inline archives the authenticated account's Inline conversations.
package inline

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

const (
	SourceType       = "inline"
	ProductionOrigin = "api.inline.chat"
	RawMCPFormat     = "inline_mcp_json"
	RawCLIFormat     = "inline_cli_json"
)

type Account struct {
	UserID      int64
	Origin      string
	DisplayName string
}

func (a Account) Identifier() string { return fmt.Sprintf("%s:user:%d", ProductionOrigin, a.UserID) }

type Conversation struct {
	ID                          int64
	Type, Title                 string
	ParentChatID, RootMessageID int64
	MemberCount                 int
	MemberCountKnown            bool
	Raw                         jsontext.Value
}

type Message struct {
	ID, ChatID, SenderID int64
	Text, SenderName     string
	SentAt, EditedAt     time.Time
	IsFromMe             bool
	ReplyToMessageID     int64
	Media                []Media
	Reactions            []Reaction
	Raw                  jsontext.Value
	RawFormat            string
}

type Page struct {
	Messages     []Message
	HasMore      bool
	NextBeforeID int64
}

// Media identifies one source occurrence; URL is a current signed download URL.
// A URL is never an archive identity or a destination for account credentials.
type Media struct {
	ID         string `json:"ID"`
	ChatID     int64  `json:"ChatID"`
	MessageID  int64  `json:"MessageID"`
	Filename   string `json:"Filename"`
	MIMEType   string `json:"MIMEType"`
	URL        string `json:"URL"`
	Size       int64  `json:"Size"`
	Width      int    `json:"Width"`
	Height     int    `json:"Height"`
	DurationMS int    `json:"DurationMS"`
	Role       string `json:"Role"`
}

type Reaction struct {
	UserID    int64
	Emoji     string
	Count     int
	CreatedAt time.Time
}

// Client exposes reads only. Pages descend strictly by message ID; beforeID is
// exclusive and zero requests the latest page. Files refreshes signed URLs.
type Client interface {
	Me(ctx context.Context) (Account, error)
	Discover(ctx context.Context) ([]Conversation, error)
	Conversation(ctx context.Context, chatID int64) (Conversation, error)
	Messages(ctx context.Context, chatID, beforeID int64) (Page, error)
	Files(ctx context.Context, chatID int64, messageIDs []int64) ([]Media, error)
	Close() error
}

type ImportOptions struct {
	Account        Account
	ChatIDs        []int64
	AttachmentsDir string
	MediaPolicy    attachmentpolicy.Policy
	NoMedia, Full  bool
	Limit          int
	Progress       func(string)
}

type ImportSummary struct {
	SourceID                                                                  int64
	ConversationsProcessed, MessagesProcessed, MessagesAdded, MessagesUpdated int
	AttachmentsDownloaded, AttachmentsPending, AttachmentsSkipped, Errors     int
	Duration                                                                  time.Duration
}

func chatKey(id int64) string { return fmt.Sprintf("chat:%d", id) }
func messageKey(chatID, messageID int64) string {
	return fmt.Sprintf("chat:%d:message:%d", chatID, messageID)
}

func conversationType(t string) string {
	switch t {
	case "dm", "direct", "direct_chat":
		return "direct_chat"
	case "channel", "space_chat":
		return "channel"
	default:
		return "group_chat"
	}
}
