// Package chatwoot archives shared inboxes through Chatwoot's read-only account API.
package chatwoot

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

const SourceType = "chatwoot"

const actorUser = "user"

// Inbox deliberately excludes the channel credentials returned to administrators.
type Inbox struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	ChannelType string `json:"channel_type"`
}

type Actor struct {
	ID            int64  `json:"id"`
	Type          string `json:"type"`
	Name          string `json:"name"`
	AvailableName string `json:"available_name"`
	Email         string `json:"email"`
	PhoneNumber   string `json:"phone_number"`
	Identifier    string `json:"identifier"`
}

// Conversation is a safe context projection. Seed messages and arbitrary
// attributes are excluded, so private-policy exclusions also apply to context.
// ID is the account-local display ID accepted by the nested message routes.
type Conversation struct {
	ID        int64    `json:"id"`
	AccountID int64    `json:"account_id"`
	InboxID   int64    `json:"inbox_id"`
	Status    string   `json:"status"`
	CreatedAt int64    `json:"created_at"`
	UpdatedAt float64  `json:"updated_at"`
	Labels    []string `json:"labels"`
	Meta      struct {
		Sender       Actor  `json:"sender"`
		Assignee     Actor  `json:"assignee"`
		AssigneeType string `json:"assignee_type"`
		Channel      string `json:"channel"`
	} `json:"meta"`
}

type Message struct {
	ID                int64                     `json:"id"`
	AccountID         int64                     `json:"account_id"`
	InboxID           int64                     `json:"inbox_id"`
	ConversationID    int64                     `json:"conversation_id"`
	Content           string                    `json:"content"`
	ContentType       string                    `json:"content_type"`
	MessageType       int                       `json:"message_type"`
	CreatedAt         int64                     `json:"created_at"`
	Private           bool                      `json:"private"`
	Status            string                    `json:"status"`
	SourceID          string                    `json:"source_id"`
	SenderType        string                    `json:"sender_type"`
	SenderID          int64                     `json:"sender_id"`
	Sender            *Actor                    `json:"sender"`
	Attachments       []Attachment              `json:"attachments"`
	ContentAttributes map[string]jsontext.Value `json:"content_attributes"`
	Call              *Call                     `json:"call"`
	Raw               jsontext.Value            `json:"-"`
}

func (m *Message) UnmarshalJSON(b []byte) error {
	type wire Message
	var value wire
	if err := json.Unmarshal(b, &value); err != nil {
		return err
	}
	*m = Message(value)
	m.Raw = append(jsontext.Value(nil), b...)
	return nil
}

type Attachment struct {
	ID              int64          `json:"id"`
	MessageID       int64          `json:"message_id"`
	FileType        string         `json:"file_type"`
	ContentType     string         `json:"content_type"`
	DataURL         string         `json:"data_url"`
	ExternalURL     string         `json:"external_url"`
	Extension       string         `json:"extension"`
	FileSize        int64          `json:"file_size"`
	Width           int            `json:"width"`
	Height          int            `json:"height"`
	TranscribedText string         `json:"transcribed_text"`
	Raw             jsontext.Value `json:"-"`
}

func (a *Attachment) UnmarshalJSON(b []byte) error {
	type wire Attachment
	var value wire
	if err := json.Unmarshal(b, &value); err != nil {
		return err
	}
	*a = Attachment(value)
	a.Raw = append(jsontext.Value(nil), b...)
	return nil
}

// Call is the live object embedded in messages, distinct from /calls' serializer.
// Timestamps remain raw because ended_at is provider metadata, not a typed column.
type Call struct {
	ID                  int64          `json:"id"`
	ProviderCallID      string         `json:"provider_call_id"`
	Provider            string         `json:"provider"`
	Direction           string         `json:"direction"`
	Status              string         `json:"status"`
	DurationSeconds     *float64       `json:"duration_seconds"`
	AcceptedByAgentID   int64          `json:"accepted_by_agent_id"`
	AcceptedByAgentName string         `json:"accepted_by_agent_name"`
	StartedAt           jsontext.Value `json:"started_at"`
	EndedAt             jsontext.Value `json:"ended_at"`
	FromNumber          string         `json:"from_number"`
	ToNumber            string         `json:"to_number"`
	RecordingURL        string         `json:"recording_url"`
	Transcript          string         `json:"transcript"`
}
