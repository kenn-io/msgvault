package messageimport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/textutil"
)

// Snapshot is one normalized message as msgvault stores it.
type Snapshot struct {
	// SourceType is the type of the source the message belongs to:
	// SourceType for a message import, or a connector's own.
	SourceType           string
	SourceIdentifier     string
	SourceDisplayName    string
	AccountEmail         string
	SourceMessageID      string
	SourceConversationID string
	ConversationTitle    string
	MessageType          string
	Title                string
	SentAt               time.Time
	BodyText             string
	BodyHTML             string
	Snippet              string
	Metadata             []byte
	Raw                  []byte
	// RawFormat names Raw's format: RawFormat for the normalized message,
	// or a connector's own format; see Writer.WriteFrom.
	RawFormat string
	// ConversationType is the type of the message's conversation:
	// ConversationType, or a connector's own; see
	// Writer.WithConversationType.
	ConversationType string
	Sender           MessageImportSender
	// SenderIdentifierType, when set, is the shared identifier type of a
	// sender's id; see Writer.WithSenderIdentifierType.
	SenderIdentifierType string
}

type messageMetadata struct {
	Platform               string         `json:"platform"`
	SourceIdentifier       string         `json:"source_identifier"`
	ExternalMessageID      string         `json:"external_message_id"`
	ExternalConversationID string         `json:"external_conversation_id"`
	URL                    string         `json:"url,omitempty"`
	ProviderMetadata       map[string]any `json:"provider_metadata,omitempty"`
}

// BuildSnapshot stores the normalized message as raw evidence, so an
// unchanged retry produces identical bytes.
func BuildSnapshot(req NormalizedRequest) (Snapshot, error) {
	snapshot, err := buildMessageSnapshot(SourceType, req.Source.Identifier, req)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot.SourceDisplayName = req.Source.DisplayName
	snapshot.AccountEmail = req.Source.AccountEmail
	return snapshot, nil
}

// buildMessageSnapshot builds the stored form of one normalized message for a
// source of type platform.
func buildMessageSnapshot(platform, sourceIdentifier string, req NormalizedRequest) (Snapshot, error) {
	message := req.Message
	raw, err := json.Marshal(message, json.Deterministic(true))
	if err != nil {
		return Snapshot{}, fmt.Errorf("marshal canonical message: %w", err)
	}
	metadata, err := json.Marshal(messageMetadata{
		Platform:               platform,
		SourceIdentifier:       sourceIdentifier,
		ExternalMessageID:      message.ExternalID,
		ExternalConversationID: message.Conversation.ExternalID,
		URL:                    message.URL,
		ProviderMetadata:       message.Metadata,
	}, json.Deterministic(true))
	if err != nil {
		return Snapshot{}, fmt.Errorf("marshal message metadata: %w", err)
	}

	bodyText, bodyHTML := renderBodies(req.Body, req.BodyFormat, message.URL)
	return Snapshot{
		SourceType:           platform,
		SourceIdentifier:     sourceIdentifier,
		SourceMessageID:      message.ExternalID,
		SourceConversationID: message.Conversation.ExternalID,
		ConversationTitle:    message.Conversation.Title,
		MessageType:          message.MessageType,
		Title:                message.Title,
		SentAt:               req.SentAt,
		BodyText:             bodyText,
		BodyHTML:             bodyHTML,
		Snippet:              textutil.PrefixRunes(bodyText, 200),
		Metadata:             metadata,
		Raw:                  raw,
		RawFormat:            RawFormat,
		ConversationType:     ConversationType,
		Sender:               message.Sender,
	}, nil
}

// renderBodies returns the stored plain-text body and, for HTML messages,
// the HTML body. Markdown is stored as text. The message URL ends both, so
// readers can open the original and search can find it.
func renderBodies(body string, format BodyFormat, messageURL string) (string, string) {
	text, htmlBody := body, ""
	if format == BodyFormatHTML {
		text, htmlBody = mime.StripHTML(body), body
	}
	if messageURL == "" {
		return text, htmlBody
	}
	if text == "" {
		text = messageURL
	} else {
		text += "\n\n" + messageURL
	}
	if htmlBody != "" {
		escaped := html.EscapeString(messageURL)
		htmlBody += "\n<p><a href=\"" + escaped + "\">" + escaped + "</a></p>"
	}
	return text, htmlBody
}

// senderIdentifier is the participant identifier value of a sender without
// an email or phone. A URL names the same sender in every source. An ID or a
// name means something only inside its source, so it carries the source's
// scope, see senderScope.
func senderIdentifier(sourceType, sourceIdentifier string, sender MessageImportSender) string {
	switch {
	case sender.ID != "":
		return boundedIdentifier("id", senderScope(sourceType, sourceIdentifier)+":"+sender.ID)
	case sender.URL != "":
		return boundedIdentifier("url", identityURL(sender.URL))
	default:
		return boundedIdentifier("name", senderScope(sourceType, sourceIdentifier)+":"+sender.Name)
	}
}

// senderScope names the source a sender's ID or name belongs to: its
// identifier and, for any source but a message import, its type, since
// sources of different types may share an identifier. Each part is escaped,
// so a scope holds no ":" separator, and only a typed scope holds a "/".
func senderScope(sourceType, sourceIdentifier string) string {
	scope := url.QueryEscape(sourceIdentifier)
	if sourceType != "" && sourceType != SourceType {
		scope = url.QueryEscape(sourceType) + "/" + scope
	}
	return scope
}

// maxIdentifierValueBytes keeps a participant identifier inside
// PostgreSQL's B-tree entry limit (about 2,700 bytes), which the unique
// index on participant identifiers must hold.
const maxIdentifierValueBytes = 1024

// boundedIdentifier is kind:value, or kind-sha256:<digest of value> when the
// value is too long to index. The digest names the same sender every time,
// so a long URL is still one sender across sources.
func boundedIdentifier(kind, value string) string {
	identifier := kind + ":" + value
	if len(identifier) <= maxIdentifierValueBytes {
		return identifier
	}
	digest := sha256.Sum256([]byte(value))
	return kind + "-sha256:" + hex.EncodeToString(digest[:])
}

// identityURL folds the case-insensitive parts of a URL so that one site is
// one sender. Validation has already accepted the URL.
func identityURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return value
	}
	parsed.Host = strings.ToLower(parsed.Host)
	return parsed.String()
}
