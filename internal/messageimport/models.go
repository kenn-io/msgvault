// Package messageimport validates and stores provider-neutral messages
// submitted through the msgvault HTTP API.
package messageimport

import (
	"net/url"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/msgvault/internal/apiimport"
	"go.kenn.io/msgvault/internal/store"
)

const (
	SourceType       = "message_import"
	ConversationType = "channel"
	RawFormat        = "message_json"
	MaxRequestBytes  = apiimport.MaxRequestBytes

	// SenderIdentifierType is the participant identifier type of a sender
	// that has no email or phone. See senderIdentifier for its values.
	SenderIdentifierType = "message_import"

	maxSourceIdentifierChars  = 128
	maxSourceDisplayNameChars = 256
	maxExternalIDChars        = 256
	maxMessageTypeChars       = 64
	maxSenderNameChars        = 256
	maxSenderIDChars          = 200
	maxTitleChars             = 4096
	maxURLChars               = 4096
)

var (
	ErrMalformedRequest = apiimport.ErrMalformedRequest
	ErrRequestTooLarge  = apiimport.ErrRequestTooLarge
	ErrValidation       = apiimport.ErrValidation
)

type MessageImportRequest struct {
	Source  MessageImportSource `json:"source"`
	Message ImportedMessage     `json:"message"`
}

// Request is kept as a concise internal alias for the message import wire
// contract. The named type gives generated API clients an unambiguous schema.
type Request = MessageImportRequest

type MessageImportSource struct {
	Identifier   string `json:"identifier" maxLength:"128"`
	DisplayName  string `json:"display_name,omitempty" maxLength:"256"`
	AccountEmail string `json:"account_email" format:"email"`
}

// ImportedMessage is one message from a source msgvault has no connector
// for. Exactly one body field may be set, and a message needs a title or a
// body.
type ImportedMessage struct {
	ExternalID   string                    `json:"external_id" maxLength:"256" doc:"Stable ID of the message in its source; with source.identifier it is the idempotency key"`
	MessageType  string                    `json:"message_type" maxLength:"64" pattern:"^[a-z][a-z0-9_]*$" doc:"Caller-chosen type such as feed_item; lowercase letters, digits and underscores, starting with a letter"`
	Conversation MessageImportConversation `json:"conversation"`
	Sender       MessageImportSender       `json:"sender"`
	Title        string                    `json:"title,omitempty" maxLength:"4096"`
	URL          string                    `json:"url,omitempty" format:"uri" maxLength:"4096" doc:"Canonical URL of the message"`
	SentAt       string                    `json:"sent_at" format:"date-time"`
	BodyMarkdown string                    `json:"body_markdown,omitempty"`
	BodyHTML     string                    `json:"body_html,omitempty"`
	BodyText     string                    `json:"body_text,omitempty"`
	Metadata     map[string]any            `json:"metadata,omitempty"`
}

// MessageImportConversation groups messages, such as all items of one feed.
type MessageImportConversation struct {
	ExternalID string `json:"external_id" maxLength:"256" doc:"Stable ID of the conversation in its source"`
	Title      string `json:"title,omitempty" maxLength:"4096"`
}

// MessageImportSender is the author of a message. It needs at least one
// field. An email or phone makes the sender that address's participant;
// otherwise ID, then URL, then name identifies the sender.
type MessageImportSender struct {
	Name  string `json:"name,omitempty" maxLength:"256"`
	URL   string `json:"url,omitempty" format:"uri" maxLength:"4096"`
	Email string `json:"email,omitempty" format:"email"`
	Phone string `json:"phone,omitempty" doc:"International phone number starting with + or 00; normalized to E.164"`
	ID    string `json:"id,omitempty" maxLength:"200" doc:"Stable identifier for this sender in the import source"`
}

// BodyFormat names which body field a message carried.
type BodyFormat string

const (
	BodyFormatNone     BodyFormat = ""
	BodyFormatMarkdown BodyFormat = "markdown"
	BodyFormatHTML     BodyFormat = "html"
	BodyFormatText     BodyFormat = "text"
)

// NormalizedRequest is a validated request. Message holds the trimmed,
// canonical fields and is stored as the message's raw evidence; SentAt and
// Body are parsed from it.
type NormalizedRequest struct {
	Source     MessageImportSource
	Message    ImportedMessage
	SentAt     time.Time
	Body       string
	BodyFormat BodyFormat
}

func (r Request) Normalize() (NormalizedRequest, error) {
	source, err := normalizeSource(r.Source)
	if err != nil {
		return NormalizedRequest{}, err
	}
	out, err := normalizeMessage(r.Message)
	if err != nil {
		return NormalizedRequest{}, err
	}
	out.Source = source
	return out, nil
}

func normalizeSource(source MessageImportSource) (MessageImportSource, error) {
	source.Identifier = strings.TrimSpace(source.Identifier)
	if err := apiimport.ValidateBoundedRequired("source.identifier", source.Identifier, maxSourceIdentifierChars); err != nil {
		return MessageImportSource{}, err
	}

	source.DisplayName = strings.TrimSpace(source.DisplayName)
	if err := apiimport.ValidateBoundedOptional("source.display_name", source.DisplayName, maxSourceDisplayNameChars); err != nil {
		return MessageImportSource{}, err
	}

	accountEmail, err := apiimport.NormalizeEmail("source.account_email", source.AccountEmail)
	if err != nil {
		return MessageImportSource{}, err
	}
	source.AccountEmail = accountEmail
	return source, nil
}

func normalizeMessage(message ImportedMessage) (NormalizedRequest, error) {
	var out NormalizedRequest
	if err := apiimport.ValidateMetadataText("message.metadata", message.Metadata); err != nil {
		return out, err
	}
	normalized := ImportedMessage{Metadata: message.Metadata}

	normalized.ExternalID = strings.TrimSpace(message.ExternalID)
	if err := apiimport.ValidateBoundedRequired("message.external_id", normalized.ExternalID, maxExternalIDChars); err != nil {
		return out, err
	}

	messageType, err := normalizeMessageType(message.MessageType)
	if err != nil {
		return out, err
	}
	normalized.MessageType = messageType

	normalized.Conversation.ExternalID = strings.TrimSpace(message.Conversation.ExternalID)
	if err := apiimport.ValidateBoundedRequired("message.conversation.external_id", normalized.Conversation.ExternalID, maxExternalIDChars); err != nil {
		return out, err
	}
	normalized.Conversation.Title = strings.TrimSpace(message.Conversation.Title)
	if err := apiimport.ValidateBoundedOptional("message.conversation.title", normalized.Conversation.Title, maxTitleChars); err != nil {
		return out, err
	}

	if normalized.Sender, err = normalizeSender(message.Sender); err != nil {
		return out, err
	}

	normalized.Title = strings.TrimSpace(message.Title)
	if err := apiimport.ValidateBoundedOptional("message.title", normalized.Title, maxTitleChars); err != nil {
		return out, err
	}
	if normalized.URL, err = normalizeURL("message.url", message.URL); err != nil {
		return out, err
	}
	if out.SentAt, err = apiimport.ParseTimestamp("message.sent_at", message.SentAt); err != nil {
		return out, err
	}
	normalized.SentAt = out.SentAt.Format(time.RFC3339Nano)

	bodies := []struct {
		field  string
		value  string
		format BodyFormat
		stored *string
	}{
		{"message.body_markdown", message.BodyMarkdown, BodyFormatMarkdown, &normalized.BodyMarkdown},
		{"message.body_html", message.BodyHTML, BodyFormatHTML, &normalized.BodyHTML},
		{"message.body_text", message.BodyText, BodyFormatText, &normalized.BodyText},
	}
	for _, body := range bodies {
		value := strings.TrimSpace(body.value)
		if value == "" {
			continue
		}
		if out.BodyFormat != BodyFormatNone {
			return out, apiimport.ValidationError("message.body_markdown, message.body_html and message.body_text are mutually exclusive")
		}
		if err := apiimport.ValidateText(body.field, value); err != nil {
			return out, err
		}
		*body.stored = value
		out.Body = value
		out.BodyFormat = body.format
	}
	if normalized.Title == "" && out.BodyFormat == BodyFormatNone {
		return out, apiimport.ValidationError("message requires a title or a body")
	}
	out.Message = normalized
	return out, nil
}

// normalizeMessageType keeps caller-chosen types to the shape msgvault's own
// types use, so they work as search and export filters.
func normalizeMessageType(value string) (string, error) {
	value = strings.TrimSpace(value)
	if err := apiimport.ValidateBoundedRequired("message.message_type", value, maxMessageTypeChars); err != nil {
		return "", err
	}
	if !store.IsWellFormedMessageType(value) {
		return "", apiimport.ValidationError("message.message_type must be lowercase letters, digits and underscores, starting with a letter")
	}
	return value, nil
}

func normalizeSender(sender MessageImportSender) (MessageImportSender, error) {
	out := MessageImportSender{
		Name: strings.TrimSpace(sender.Name),
		ID:   strings.TrimSpace(sender.ID),
	}
	if err := apiimport.ValidateBoundedOptional("message.sender.name", out.Name, maxSenderNameChars); err != nil {
		return MessageImportSender{}, err
	}
	if err := apiimport.ValidateBoundedOptional("message.sender.id", out.ID, maxSenderIDChars); err != nil {
		return MessageImportSender{}, err
	}
	if strings.IndexFunc(out.ID, unicode.IsControl) >= 0 {
		return MessageImportSender{}, apiimport.ValidationError("message.sender.id must not contain control characters")
	}
	var err error
	if out.URL, err = normalizeURL("message.sender.url", sender.URL); err != nil {
		return MessageImportSender{}, err
	}
	if strings.TrimSpace(sender.Email) != "" {
		if out.Email, err = apiimport.NormalizeEmail("message.sender.email", sender.Email); err != nil {
			return MessageImportSender{}, err
		}
	}
	if strings.TrimSpace(sender.Phone) != "" {
		if out.Phone, err = apiimport.NormalizePhone("message.sender.phone", sender.Phone); err != nil {
			return MessageImportSender{}, err
		}
	}
	if out == (MessageImportSender{}) {
		return MessageImportSender{}, apiimport.ValidationError("message.sender requires a name, url, email, phone or id")
	}
	return out, nil
}

// normalizeURL accepts one absolute URL. Errors name the field, never the URL.
func normalizeURL(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if err := apiimport.ValidateBoundedOptional(field, value, maxURLChars); err != nil {
		return "", err
	}
	if strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", apiimport.ValidationError("%s must not contain whitespace or control characters", field)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || (parsed.Host == "" && parsed.Opaque == "") {
		return "", apiimport.ValidationError("%s must be an absolute URL", field)
	}
	return value, nil
}
