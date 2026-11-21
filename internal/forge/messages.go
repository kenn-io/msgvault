package forge

import (
	"encoding/json/jsontext"
	"maps"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/messageimport"
)

// ConversationType is the type of every thread's conversation: a thread is
// a discussion people join, not a chat to message someone in.
const ConversationType = "thread"

// importMessage is a post as the message import stores it. Only an opening
// post, which has a title, names the conversation, so retitling a thread
// rewrites that one post rather than every reply.
func importMessage(forge Forge, head ThreadHead, post Post) messageimport.ImportedMessage {
	metadata := make(map[string]any, len(post.Metadata)+1)
	maps.Copy(metadata, post.Metadata)
	metadata["kind"] = post.Kind
	conversation := messageimport.MessageImportConversation{ExternalID: head.ConversationID}
	if post.Title != "" {
		conversation.Title = clean(head.Title)
	}
	return messageimport.ImportedMessage{
		ExternalID:   post.ID,
		MessageType:  forge.Name(),
		Conversation: conversation,
		Sender:       sender(forge.Instance(), post.Author),
		Title:        clean(post.Title),
		URL:          post.URL,
		SentAt:       post.CreatedAt.UTC().Format(time.RFC3339Nano),
		BodyMarkdown: clean(post.Body),
		Metadata:     cleanMetadata(metadata),
	}
}

// sender is the participant a post's author becomes. An account is keyed by
// its ID on its instance, shared by every source that sees it, and joins
// the person's mail participant when the forge shows its public email. An
// unavailable account is one participant per profile URL; a migrated name
// is a participant of this source alone.
func sender(instance Instance, author Account) messageimport.MessageImportSender {
	name := clean(author.Login)
	if name == "" {
		name = "unknown"
	}
	switch {
	case author.Kind == AuthorAccount && author.ID != "":
		s := messageimport.MessageImportSender{
			ID:   AccountKey(instance, author.ID),
			Name: name,
			URL:  author.URL,
		}
		if !author.Bot {
			s.Email = author.Email
		}
		return s
	case author.Kind != AuthorMigrated && author.URL != "":
		return messageimport.MessageImportSender{Name: name, URL: author.URL}
	default:
		return messageimport.MessageImportSender{Name: name}
	}
}

// canonicalJSON is a post's raw JSON in canonical form (RFC 8785), so
// unchanged evidence is byte-identical across syncs whatever order a
// response lists its fields in. Raw that isn't JSON is kept as it is.
func canonicalJSON(raw []byte) []byte {
	value := jsontext.Value(slices.Clone(raw))
	if value.Canonicalize() != nil {
		return raw
	}
	return value
}

// clean drops NUL characters, which forge text may hold and the archive
// can't store.
func clean(text string) string {
	return strings.ReplaceAll(text, "\x00", "")
}

func cleanMetadata(value map[string]any) map[string]any {
	out := make(map[string]any, len(value))
	for key, nested := range value {
		out[clean(key)] = cleanValue(nested)
	}
	return out
}

func cleanValue(value any) any {
	switch value := value.(type) {
	case string:
		return clean(value)
	case map[string]any:
		return cleanMetadata(value)
	case []any:
		out := make([]any, len(value))
		for i, nested := range value {
			out[i] = cleanValue(nested)
		}
		return out
	default:
		return value
	}
}
