// Package groupme imports owner-supplied GroupMe JSON exports locally.
package groupme

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

const SourceType = "groupme"
const MessageType = "groupme"

type Message struct {
	ID             string         `json:"id"`
	GroupID        string         `json:"group_id"`
	ConversationID string         `json:"conversation_id"`
	SenderID       string         `json:"sender_id"`
	UserID         string         `json:"user_id"`
	Name           string         `json:"name"`
	CreatedAt      *int64         `json:"created_at"`
	Text           string         `json:"text"`
	SenderType     string         `json:"sender_type"`
	System         bool           `json:"system"`
	RawAttachments jsontext.Value `json:"attachments"`
	Attachments    []Attachment   `json:"-"`
}

type Attachment struct {
	Type    string `json:"type"`
	URL     string `json:"url"`
	ReplyID string `json:"reply_id"`
}

type member struct {
	UserID   string `json:"user_id"`
	Name     string `json:"name"`
	Nickname string `json:"nickname"`
}

type conversation struct {
	Chat struct {
		OtherUser struct {
			Name string `json:"name"`
		} `json:"other_user"`
	} `json:"chat"`
	Name    string   `json:"name"`
	Members []member `json:"members"`
}

type chat struct {
	folder, key, kind string
	metadata          conversation
}

func (m *Message) peer() (string, string) {
	if m.ConversationID != "" {
		return "dm:" + m.ConversationID, "direct_chat"
	}
	if m.GroupID != "" {
		return "group:" + m.GroupID, "group_chat"
	}
	return "", ""
}

// Unknown attachment shapes do not discard the ordinary text beside them.
func (m *Message) decodeAttachments() {
	if len(m.RawAttachments) == 0 || m.RawAttachments.Kind() == 'n' {
		return
	}
	var values []jsontext.Value
	if err := json.Unmarshal(m.RawAttachments, &values); err != nil {
		m.Attachments = []Attachment{{Type: "unsupported"}}
		return
	}
	for _, raw := range values {
		var a Attachment
		if raw.Kind() != '{' || json.Unmarshal(raw, &a) != nil {
			a = Attachment{Type: "unsupported"}
		}
		m.Attachments = append(m.Attachments, a)
	}
}

func (m *Message) isService() bool {
	return m.System || m.SenderType == "system" || m.SenderType == "service"
}
