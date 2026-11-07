// Package messageimport validates and stores provider-neutral messages
// submitted through the msgvault HTTP API.
package messageimport

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/store"
)

const (
	SourceType       = "message_import"
	ConversationType = "channel"
	RawFormat        = "message_json"
	MaxRequestBytes  = int64(16 << 20)

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
	ErrMalformedRequest = errors.New("malformed message import request")
	ErrRequestTooLarge  = errors.New("message import request too large")
	ErrValidation       = errors.New("message import validation failed")
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

type NormalizedRequest struct {
	Source  MessageImportSource
	Message NormalizedMessage
}

type NormalizedMessage struct {
	ExternalID   string
	MessageType  string
	Conversation MessageImportConversation
	Sender       MessageImportSender
	Title        string
	URL          string
	SentAt       time.Time
	Body         string
	BodyFormat   BodyFormat
	Metadata     map[string]any
}

func (r Request) Normalize() (NormalizedRequest, error) {
	source, err := normalizeSource(r.Source)
	if err != nil {
		return NormalizedRequest{}, err
	}
	message, err := normalizeMessage(r.Message)
	if err != nil {
		return NormalizedRequest{}, err
	}
	return NormalizedRequest{Source: source, Message: message}, nil
}

func normalizeSource(source MessageImportSource) (MessageImportSource, error) {
	source.Identifier = strings.TrimSpace(source.Identifier)
	if err := validateBoundedRequired("source.identifier", source.Identifier, maxSourceIdentifierChars); err != nil {
		return MessageImportSource{}, err
	}

	source.DisplayName = strings.TrimSpace(source.DisplayName)
	if err := validateBoundedOptional("source.display_name", source.DisplayName, maxSourceDisplayNameChars); err != nil {
		return MessageImportSource{}, err
	}

	accountEmail, err := normalizeEmail("source.account_email", source.AccountEmail)
	if err != nil {
		return MessageImportSource{}, err
	}
	source.AccountEmail = accountEmail
	return source, nil
}

func normalizeMessage(message ImportedMessage) (NormalizedMessage, error) {
	out := NormalizedMessage{Metadata: message.Metadata}

	out.ExternalID = strings.TrimSpace(message.ExternalID)
	if err := validateBoundedRequired("message.external_id", out.ExternalID, maxExternalIDChars); err != nil {
		return NormalizedMessage{}, err
	}

	messageType, err := normalizeMessageType(message.MessageType)
	if err != nil {
		return NormalizedMessage{}, err
	}
	out.MessageType = messageType

	out.Conversation.ExternalID = strings.TrimSpace(message.Conversation.ExternalID)
	if err := validateBoundedRequired("message.conversation.external_id", out.Conversation.ExternalID, maxExternalIDChars); err != nil {
		return NormalizedMessage{}, err
	}
	out.Conversation.Title = strings.TrimSpace(message.Conversation.Title)
	if err := validateBoundedOptional("message.conversation.title", out.Conversation.Title, maxTitleChars); err != nil {
		return NormalizedMessage{}, err
	}

	sender, err := normalizeSender(message.Sender)
	if err != nil {
		return NormalizedMessage{}, err
	}
	out.Sender = sender

	out.Title = strings.TrimSpace(message.Title)
	if err := validateBoundedOptional("message.title", out.Title, maxTitleChars); err != nil {
		return NormalizedMessage{}, err
	}
	if out.URL, err = normalizeURL("message.url", message.URL); err != nil {
		return NormalizedMessage{}, err
	}
	if out.SentAt, err = parseTimestamp("message.sent_at", message.SentAt); err != nil {
		return NormalizedMessage{}, err
	}

	bodies := []struct {
		field  string
		value  string
		format BodyFormat
	}{
		{"message.body_markdown", message.BodyMarkdown, BodyFormatMarkdown},
		{"message.body_html", message.BodyHTML, BodyFormatHTML},
		{"message.body_text", message.BodyText, BodyFormatText},
	}
	for _, body := range bodies {
		value := strings.TrimSpace(body.value)
		if value == "" {
			continue
		}
		if out.BodyFormat != BodyFormatNone {
			return NormalizedMessage{}, validationError("message.body_markdown, message.body_html and message.body_text are mutually exclusive")
		}
		if !utf8.ValidString(value) {
			return NormalizedMessage{}, validationError("%s must be valid UTF-8", body.field)
		}
		out.Body = value
		out.BodyFormat = body.format
	}
	if out.Title == "" && out.BodyFormat == BodyFormatNone {
		return NormalizedMessage{}, validationError("message requires a title or a body")
	}
	return out, nil
}

// normalizeMessageType keeps caller-chosen types to the shape msgvault's own
// types use, so they work as search and export filters.
func normalizeMessageType(value string) (string, error) {
	value = strings.TrimSpace(value)
	if err := validateBoundedRequired("message.message_type", value, maxMessageTypeChars); err != nil {
		return "", err
	}
	if !store.IsWellFormedMessageType(value) {
		return "", validationError("message.message_type must be lowercase letters, digits and underscores, starting with a letter")
	}
	return value, nil
}

func normalizeSender(sender MessageImportSender) (MessageImportSender, error) {
	out := MessageImportSender{
		Name: strings.TrimSpace(sender.Name),
		ID:   strings.TrimSpace(sender.ID),
	}
	if err := validateBoundedOptional("message.sender.name", out.Name, maxSenderNameChars); err != nil {
		return MessageImportSender{}, err
	}
	if err := validateBoundedOptional("message.sender.id", out.ID, maxSenderIDChars); err != nil {
		return MessageImportSender{}, err
	}
	if strings.IndexFunc(out.ID, unicode.IsControl) >= 0 {
		return MessageImportSender{}, validationError("message.sender.id must not contain control characters")
	}
	var err error
	if out.URL, err = normalizeURL("message.sender.url", sender.URL); err != nil {
		return MessageImportSender{}, err
	}
	if strings.TrimSpace(sender.Email) != "" {
		if out.Email, err = normalizeEmail("message.sender.email", sender.Email); err != nil {
			return MessageImportSender{}, err
		}
	}
	if strings.TrimSpace(sender.Phone) != "" {
		if out.Phone, err = normalizePhone("message.sender.phone", sender.Phone); err != nil {
			return MessageImportSender{}, err
		}
	}
	if out == (MessageImportSender{}) {
		return MessageImportSender{}, validationError("message.sender requires a name, url, email, phone or id")
	}
	return out, nil
}

// normalizeURL accepts one absolute URL. Errors name the field, never the URL.
func normalizeURL(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if err := validateBoundedOptional(field, value, maxURLChars); err != nil {
		return "", err
	}
	if strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", validationError("%s must not contain whitespace or control characters", field)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || (parsed.Host == "" && parsed.Opaque == "") {
		return "", validationError("%s must be an absolute URL", field)
	}
	return value, nil
}

// normalizePhone accepts international numbers only (a leading + or 00) and
// returns E.164. National numbers are rejected rather than guessed: a wrong
// country code could attach a message to someone else. Errors name the field,
// never the number.
func normalizePhone(field, value string) (string, error) {
	// "+44 (0)20 …" marks a trunk zero that is dialed only nationally.
	value = strings.ReplaceAll(strings.TrimSpace(value), "(0)", "")
	var digits strings.Builder
	for i, r := range value {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '+' && i == 0:
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')':
		default:
			return "", validationError("%s must contain only digits and phone formatting", field)
		}
	}
	number := digits.String()
	switch {
	case strings.HasPrefix(value, "+"):
	case strings.HasPrefix(number, "00"):
		number = number[2:]
	default:
		return "", validationError("%s must be an international number starting with + or 00", field)
	}
	if strings.HasPrefix(number, "0") {
		return "", validationError("%s must start with a country code", field)
	}
	if len(number) < 7 || len(number) > 15 {
		return "", validationError("%s must have 7 to 15 digits", field)
	}
	return "+" + number, nil
}

func normalizeEmail(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", validationError("%s is required", field)
	}
	if !utf8.ValidString(value) {
		return "", validationError("%s must be valid UTF-8", field)
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Name != "" || !strings.EqualFold(parsed.Address, value) {
		return "", validationError("%s must be one email address without a display name", field)
	}
	return strings.ToLower(parsed.Address), nil
}

func parseTimestamp(field, value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, validationError("%s is required", field)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, validationError("%s must be an RFC3339 timestamp with an explicit offset", field)
	}
	return parsed.UTC(), nil
}

func validateBoundedRequired(field, value string, maxChars int) error {
	if value == "" {
		return validationError("%s is required", field)
	}
	return validateBoundedOptional(field, value, maxChars)
}

func validateBoundedOptional(field, value string, maxChars int) error {
	if !utf8.ValidString(value) {
		return validationError("%s must be valid UTF-8", field)
	}
	if utf8.RuneCountInString(value) > maxChars {
		return validationError("%s must be at most %d characters", field, maxChars)
	}
	return nil
}

func validationError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(format, args...))
}
