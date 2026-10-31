// Package telegram imports Telegram Desktop JSON exports.
package telegram

import (
	"encoding/json/jsontext"
	"strconv"
	"time"
)

const SourceType = "telegram"
const MessageType = "telegram"

type Chat struct {
	ID       int64     `json:"id"`
	Type     string    `json:"type"`
	Name     string    `json:"name"`
	Messages []Message `json:"messages"`
}

var chatTypes = map[string]struct{ prefix, conversation string }{
	"personal_chat":      {"user", "direct_chat"},
	"bot_chat":           {"user", "direct_chat"},
	"saved_messages":     {"user", "direct_chat"},
	"replies":            {"user", "group_chat"},
	"verification_codes": {"user", "direct_chat"},
	"private_group":      {"chat", "group_chat"},
	"private_supergroup": {"channel", "group_chat"},
	"public_supergroup":  {"channel", "group_chat"},
	"private_channel":    {"channel", "channel"},
	"public_channel":     {"channel", "channel"},
}

func (c *Chat) PeerKey() string {
	kind, ok := chatTypes[c.Type]
	if !ok {
		return ""
	}
	return kind.prefix + strconv.FormatInt(c.ID, 10)
}

func (c *Chat) ConvType() string { return chatTypes[c.Type].conversation }

type Message struct {
	ID             int64          `json:"id"`
	Type           string         `json:"type"`
	DateUnixtime   string         `json:"date_unixtime"`
	Edited         string         `json:"edited"`
	EditedUnixtime string         `json:"edited_unixtime"`
	ReplyTo        int64          `json:"reply_to_message_id"`
	ReplyToPeerID  string         `json:"reply_to_peer_id"`
	FromName       string         `json:"from"`
	FromID         string         `json:"from_id"`
	RawText        jsontext.Value `json:"text"`
	RichMessage    jsontext.Value `json:"rich_message,omitempty"`
	TextEntities   []struct {
		Text string `json:"text"`
	} `json:"text_entities"`
	ContactVCard string    `json:"contact_vcard"`
	Photo        string    `json:"photo"`
	File         string    `json:"file"`
	Thumbnail    string    `json:"thumbnail"`
	MediaType    string    `json:"media_type"`
	MimeType     string    `json:"mime_type"`
	FileName     string    `json:"file_name"`
	Text         string    `json:"-"`
	SentAt       time.Time `json:"-"`
}

func (m *Message) supported() bool { return m.Type == "message" && len(m.RichMessage) == 0 }
