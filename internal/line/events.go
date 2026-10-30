package line

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/msgvault/internal/jsonexact"
)

const SourceType = "line"

// Source identifies the chat and, when consent permits, the sending user.
type Source struct {
	Type    string `json:"type"`
	UserID  string `json:"userId,omitempty"`
	GroupID string `json:"groupId,omitempty"`
	RoomID  string `json:"roomId,omitempty"`
}

// ConversationID returns the provider key and msgvault conversation type.
// Group and room events may lack a user ID; direct events cannot.
func (s Source) ConversationID() (string, string, error) {
	switch s.Type {
	case "user":
		if s.UserID != "" {
			return "user:" + s.UserID, "direct_chat", nil
		}
	case "group":
		if s.GroupID != "" {
			return "group:" + s.GroupID, "group_chat", nil
		}
	case "room":
		if s.RoomID != "" {
			return "room:" + s.RoomID, "group_chat", nil
		}
	}
	return "", "", errors.New("line: invalid conversation source")
}

type ContentProvider struct {
	Type               string `json:"type"`
	OriginalContentURL string `json:"originalContentUrl,omitempty"`
	PreviewImageURL    string `json:"previewImageUrl,omitempty"`
}

// Message contains common native fields. Raw is the sanitized, lossless
// snapshot, including fields introduced by LINE after this client was built.
// File messages have no ContentProvider and use LINE's content endpoint.
type Message struct {
	ID                  string           `json:"id"`
	Type                string           `json:"type"`
	Text                string           `json:"text,omitempty"`
	FileName            string           `json:"fileName,omitempty"`
	FileSize            int64            `json:"fileSize,omitempty"`
	QuotedMessageID     string           `json:"quotedMessageId,omitempty"`
	QuoteToken          string           `json:"quoteToken,omitempty"`
	ContentProvider     *ContentProvider `json:"contentProvider,omitempty"`
	Duration            int64            `json:"duration,omitempty"`
	PackageID           string           `json:"packageId,omitempty"`
	StickerID           string           `json:"stickerId,omitempty"`
	StickerResourceType string           `json:"stickerResourceType,omitempty"`
	Keywords            []string         `json:"keywords,omitempty"`
	Title               string           `json:"title,omitempty"`
	Address             string           `json:"address,omitempty"`
	Latitude            float64          `json:"latitude,omitempty"`
	Longitude           float64          `json:"longitude,omitempty"`
	Emojis              jsontext.Value   `json:"emojis,omitempty"`
	Mention             jsontext.Value   `json:"mention,omitempty"`
	ImageSet            jsontext.Value   `json:"imageSet,omitempty"`
	Raw                 jsontext.Value   `json:"-"`
}

type Unsend struct {
	MessageID string `json:"messageId"`
}
type Members struct {
	Members []Source `json:"members"`
}

// Event retains sanitized evidence even for unrecognized event types.
// messageEdited has the same message shape as message (group text only).
type Event struct {
	Type            string         `json:"type"`
	Timestamp       int64          `json:"timestamp"`
	WebhookEventID  string         `json:"webhookEventId"`
	Source          Source         `json:"source"`
	Message         *Message       `json:"message,omitempty"`
	Unsend          *Unsend        `json:"unsend,omitempty"`
	Joined          *Members       `json:"joined,omitempty"`
	Left            *Members       `json:"left,omitempty"`
	Mode            string         `json:"mode,omitempty"`
	DeliveryContext jsontext.Value `json:"deliveryContext,omitempty"`
	Raw             jsontext.Value `json:"-"`
}

func (e *Event) UnmarshalJSON(raw []byte) error {
	type plain Event
	clean, err := SanitizeEvent(raw)
	if err != nil {
		return err
	}
	var common struct {
		Type           string `json:"type"`
		Timestamp      int64  `json:"timestamp"`
		WebhookEventID string `json:"webhookEventId"`
	}
	if err := json.Unmarshal(clean, &common); err != nil {
		return errors.New("line: invalid event fields")
	}
	switch common.Type {
	case "message", "messageEdited", "unsend", "memberJoined", "memberLeft", "join", "leave", "follow", "unfollow":
	default:
		var compatible plain
		if json.Unmarshal(clean, &compatible) == nil {
			compatible.Raw = clean
			*e = Event(compatible)
			return nil
		}
		*e = Event{Type: common.Type, Timestamp: common.Timestamp, WebhookEventID: common.WebhookEventID, Raw: clean}
		return nil
	}
	var decoded plain
	if err := json.Unmarshal(clean, &decoded); err != nil {
		return errors.New("line: invalid event fields")
	}
	decoded.Raw = clean
	*e = Event(decoded)
	return nil
}

func (m *Message) UnmarshalJSON(raw []byte) error {
	type plain Message
	clean, err := SanitizeEvent(raw)
	if err != nil {
		return err
	}
	var common struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(clean, &common); err != nil {
		return errors.New("line: invalid message fields")
	}
	switch common.Type {
	case "text", "image", "video", "audio", "file", "location", "sticker":
	default:
		var compatible plain
		if json.Unmarshal(clean, &compatible) == nil {
			compatible.Raw = clean
			*m = Message(compatible)
			return nil
		}
		*m = Message{ID: common.ID, Type: common.Type, Raw: clean}
		return nil
	}
	var decoded plain
	if err := json.Unmarshal(clean, &decoded); err != nil {
		return errors.New("line: invalid message fields")
	}
	decoded.Raw = clean
	*m = Message(decoded)
	return nil
}

// SanitizeEvent removes ephemeral capabilities at every depth, including
// unknown event payloads. Numbers retain their original precision.
func SanitizeEvent(raw []byte) ([]byte, error) {
	var value map[string]any
	if err := json.Unmarshal(raw, &value, jsonexact.PreserveNumbers); err != nil || value == nil {
		return nil, errors.New("line: invalid event JSON")
	}
	removeCapabilities(value)
	clean, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("line: cannot sanitize event JSON")
	}
	return clean, nil
}

func removeCapabilities(value any) {
	switch value := value.(type) {
	case map[string]any:
		delete(value, "replyToken")
		delete(value, "markAsReadToken")
		for _, child := range value {
			removeCapabilities(child)
		}
	case []any:
		for _, child := range value {
			removeCapabilities(child)
		}
	}
}
