package tiktok

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
)

const MessagesSourceType = "tiktok_messages"
const messageFormat = "tiktok-message-json"

type MessageActor struct {
	ID          ID     `json:"id,omitempty"`
	Role        string `json:"role,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}
type MessageMedia struct {
	ID ID `json:"media_id,omitempty"`
}
type MessageURL struct {
	URL string `json:"url,omitempty"`
}
type SharedPost struct {
	ID       ID     `json:"item_id,omitempty"`
	EmbedURL string `json:"embed_url,omitempty"`
}
type MessageText struct {
	Body string `json:"body,omitempty"`
}
type MessageReference struct {
	ID ID `json:"referenced_message_id,omitempty"`
}

// BusinessMessage retains both pull and webhook projections, including types
// the provider cannot currently download. Unknown fields round-trip as JSON.
type BusinessMessage struct {
	ID               ID                        `json:"message_id"`
	ConversationID   ID                        `json:"conversation_id"`
	Timestamp        Integer                   `json:"timestamp,omitempty"`
	Type             string                    `json:"type,omitempty"`
	MessageType      string                    `json:"message_type,omitempty"`
	From             string                    `json:"from,omitempty"`
	To               string                    `json:"to,omitempty"`
	Sender           string                    `json:"sender,omitempty"`
	Recipient        string                    `json:"recipient,omitempty"`
	UniqueIdentifier ID                        `json:"unique_identifier,omitempty"`
	FromUser         *MessageActor             `json:"from_user,omitempty"`
	ToUser           *MessageActor             `json:"to_user,omitempty"`
	Text             *MessageText              `json:"text,omitempty"`
	Image            *MessageMedia             `json:"image,omitempty"`
	Video            *MessageMedia             `json:"video,omitempty"`
	Sticker          *MessageURL               `json:"sticker,omitempty"`
	Emoji            *MessageURL               `json:"emoji,omitempty"`
	SharePost        *SharedPost               `json:"share_post,omitempty"`
	Template         jsontext.Value            `json:"template,omitempty"`
	Reference        *MessageReference         `json:"referenced_message_info,omitempty"`
	Extra            map[string]jsontext.Value `json:",unknown"`
}

func (m BusinessMessage) Kind() string {
	kind := m.Type
	if kind == "" {
		kind = m.MessageType
	}
	if kind == "" {
		return "other"
	}
	return strings.ToLower(kind)
}
func (m BusinessMessage) media() bool {
	switch m.Kind() {
	case "image", "video", "sticker", "emoji":
		return true
	default:
		return false
	}
}
func (m BusinessMessage) body() string {
	if m.Text != nil && (m.Text.Body != "" || m.Kind() == "text") {
		return m.Text.Body
	}
	if m.SharePost != nil {
		return "[shared post] " + m.SharePost.EmbedURL
	}
	if m.Sticker != nil {
		return "[sticker] " + m.Sticker.URL
	}
	if m.Emoji != nil {
		return "[emoji] " + m.Emoji.URL
	}
	if len(m.Template) > 0 {
		return "[template] " + string(m.Template)
	}
	return "[" + m.Kind() + "]"
}

type BusinessConversation struct {
	ID         ID      `json:"conversation_id"`
	UpdateTime Integer `json:"update_time,omitempty"`
}
type ConversationPage struct {
	Conversations []BusinessConversation `json:"conversations"`
	Cursor        ID                     `json:"cursor,omitempty"`
	HasMore       bool                   `json:"has_more"`
}
type MessagePage struct {
	Messages     []BusinessMessage `json:"messages"`
	Participants []MessageActor    `json:"participants,omitempty"`
}

type MessageEvent struct {
	ClientKey string  `json:"client_key"`
	Event     string  `json:"event"`
	Account   ID      `json:"user_openid"`
	CreatedAt Integer `json:"create_time,omitempty"`
	Content   string  `json:"content"`
}

var ErrInvalidEvent = errors.New("invalid TikTok message event")

func DecodeMessageEvent(app string, raw []byte) (MessageEvent, *BusinessMessage, error) {
	var event MessageEvent
	if json.Unmarshal(raw, &event) != nil || event.ClientKey != app || !ValidID(string(event.Account)) {
		return event, nil, ErrInvalidEvent
	}
	switch event.Event {
	case "im_receive_msg_eu":
		return event, nil, nil
	case "im_receive_msg", "im_send_msg":
		var message BusinessMessage
		if json.Unmarshal([]byte(event.Content), &message) != nil || !ValidID(string(message.ID)) || !ValidID(string(message.ConversationID)) {
			return event, nil, ErrInvalidEvent
		}
		return event, &message, nil
	default:
		return event, nil, nil
	}
}
