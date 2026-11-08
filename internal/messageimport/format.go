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
	Sender               MessageImportSender
	// SenderIdentifierType, when set, is the shared identifier type of a
	// sender's id; see Writer.WithSenderIdentifierType.
	SenderIdentifierType string
}

// canonicalMessage is the stored raw evidence: the normalized request
// message. An unchanged retry produces identical bytes.
type canonicalMessage struct {
	ExternalID   string                    `json:"external_id"`
	MessageType  string                    `json:"message_type"`
	Conversation MessageImportConversation `json:"conversation"`
	Sender       MessageImportSender       `json:"sender"`
	Title        string                    `json:"title,omitempty"`
	URL          string                    `json:"url,omitempty"`
	SentAt       string                    `json:"sent_at"`
	BodyMarkdown string                    `json:"body_markdown,omitempty"`
	BodyHTML     string                    `json:"body_html,omitempty"`
	BodyText     string                    `json:"body_text,omitempty"`
	Metadata     map[string]any            `json:"metadata,omitempty"`
}

type messageMetadata struct {
	Platform               string         `json:"platform"`
	SourceIdentifier       string         `json:"source_identifier"`
	ExternalMessageID      string         `json:"external_message_id"`
	ExternalConversationID string         `json:"external_conversation_id"`
	URL                    string         `json:"url,omitempty"`
	ProviderMetadata       map[string]any `json:"provider_metadata,omitempty"`
}

func BuildSnapshot(req NormalizedRequest) (Snapshot, error) {
	snapshot, err := buildMessageSnapshot(SourceType, req.Source.Identifier, req.Message)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot.SourceDisplayName = req.Source.DisplayName
	snapshot.AccountEmail = req.Source.AccountEmail
	return snapshot, nil
}

// buildMessageSnapshot builds the stored form of one message for a source of
// type platform.
func buildMessageSnapshot(platform, sourceIdentifier string, message NormalizedMessage) (Snapshot, error) {
	raw, err := json.Marshal(buildCanonicalMessage(message), json.Deterministic(true))
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

	bodyText, bodyHTML := renderBodies(message)
	return Snapshot{
		SourceIdentifier:     sourceIdentifier,
		SourceMessageID:      message.ExternalID,
		SourceConversationID: message.Conversation.ExternalID,
		ConversationTitle:    message.Conversation.Title,
		MessageType:          message.MessageType,
		Title:                message.Title,
		SentAt:               message.SentAt.UTC(),
		BodyText:             bodyText,
		BodyHTML:             bodyHTML,
		Snippet:              textutil.PrefixRunes(bodyText, 200),
		Metadata:             metadata,
		Raw:                  raw,
		Sender:               message.Sender,
	}, nil
}

// renderBodies returns the stored plain-text body and, for HTML messages,
// the HTML body. Markdown is stored as text. The message URL ends both, so
// readers can open the original and search can find it.
func renderBodies(message NormalizedMessage) (string, string) {
	text, htmlBody := message.Body, ""
	if message.BodyFormat == BodyFormatHTML {
		text, htmlBody = mime.StripHTML(message.Body), message.Body
	}
	if message.URL == "" {
		return text, htmlBody
	}
	if text == "" {
		text = message.URL
	} else {
		text += "\n\n" + message.URL
	}
	if htmlBody != "" {
		escaped := html.EscapeString(message.URL)
		htmlBody += "\n<p><a href=\"" + escaped + "\">" + escaped + "</a></p>"
	}
	return text, htmlBody
}

func buildCanonicalMessage(message NormalizedMessage) canonicalMessage {
	out := canonicalMessage{
		ExternalID:   message.ExternalID,
		MessageType:  message.MessageType,
		Conversation: message.Conversation,
		Sender:       message.Sender,
		Title:        message.Title,
		URL:          message.URL,
		SentAt:       message.SentAt.UTC().Format(time.RFC3339Nano),
		Metadata:     message.Metadata,
	}
	switch message.BodyFormat {
	case BodyFormatMarkdown:
		out.BodyMarkdown = message.Body
	case BodyFormatHTML:
		out.BodyHTML = message.Body
	case BodyFormatText:
		out.BodyText = message.Body
	case BodyFormatNone:
	}
	return out
}

// senderIdentifier is the participant identifier value of a sender without
// an email or phone. A URL names the same sender in every source. An ID or a
// name means something only inside its source, so it carries the source
// identifier, escaped so that it cannot contain the ":" separator.
func senderIdentifier(sourceIdentifier string, sender MessageImportSender) string {
	switch {
	case sender.ID != "":
		return boundedIdentifier("id", url.QueryEscape(sourceIdentifier)+":"+sender.ID)
	case sender.URL != "":
		return boundedIdentifier("url", identityURL(sender.URL))
	default:
		return boundedIdentifier("name", url.QueryEscape(sourceIdentifier)+":"+sender.Name)
	}
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
