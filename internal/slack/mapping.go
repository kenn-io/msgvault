package slack

import (
	"database/sql"
	"regexp"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

// messageType is the msgvault message_type for all Slack-archived messages.
const messageType = "slack"

// sourceMessageID composes the archive identity of a Slack message. A ts is
// only unique within its channel, hence the composite key.
func sourceMessageID(channelID, ts string) string {
	return channelID + ":" + ts
}

// mentionRe matches user mentions in raw Slack message text: <@U123>,
// <@W123> (Enterprise Grid), optionally with a |label suffix.
var mentionRe = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^>]*)?>`)

// tokenRe matches every <…> mrkdwn token for text rendering.
var tokenRe = regexp.MustCompile(`<([^<>]+)>`)

// MentionedUserIDs returns the user IDs mentioned in the message text
// (deduped, in first-appearance order).
func (m *Message) MentionedUserIDs() []string {
	var ids []string
	seen := map[string]bool{}
	for _, match := range mentionRe.FindAllStringSubmatch(m.Text, -1) {
		if id := match[1]; !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// renderText converts raw Slack mrkdwn to plain text for FTS and display:
// mention/channel/link tokens are resolved to readable forms and HTML
// entities unescaped. lookupName resolves a user ID to a display name (may
// return "").
func renderText(raw string, lookupName func(userID string) string) string {
	text := tokenRe.ReplaceAllStringFunc(raw, func(tok string) string {
		inner := tok[1 : len(tok)-1]
		body, label, hasLabel := strings.Cut(inner, "|")
		switch {
		case strings.HasPrefix(body, "@"):
			if hasLabel && label != "" {
				return "@" + label
			}
			if name := lookupName(strings.TrimPrefix(body, "@")); name != "" {
				return "@" + name
			}
			return "@" + strings.TrimPrefix(body, "@")
		case strings.HasPrefix(body, "#"):
			if hasLabel && label != "" {
				return "#" + label
			}
			return body
		case strings.HasPrefix(body, "!"):
			// Special mentions: <!here>, <!channel>, <!everyone>, <!date^…|fallback>.
			if hasLabel && label != "" {
				return label
			}
			return "@" + strings.TrimPrefix(body, "!")
		default:
			// Link: <url> or <url|label>.
			if hasLabel && label != "" {
				return label + " (" + body + ")"
			}
			return body
		}
	})
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&amp;", "&")
	return text
}

// placeholderBody synthesizes a searchable body for text-less messages.
func placeholderBody(m *Message) string {
	if len(m.Files) > 0 {
		if m.Files[0].Name != "" {
			return "[file: " + m.Files[0].Name + "]"
		}
		return "[file]"
	}
	return ""
}

// payloadText extracts searchable text from legacy attachments and Block Kit
// blocks — where bots and integrations often carry their entire content
// while the message text stays empty. Per attachment, the mandatory fallback
// summary wins; otherwise the individual parts are composed. rich_text
// blocks are skipped (they duplicate the message text field).
func payloadText(m *Message, lookupName func(string) string) string {
	var parts []string
	add := func(s string) {
		if s = strings.TrimSpace(renderText(s, lookupName)); s != "" {
			parts = append(parts, s)
		}
	}
	for i := range m.Attachments {
		a := &m.Attachments[i]
		if a.Fallback != "" {
			add(a.Fallback)
			continue
		}
		add(a.Pretext)
		add(a.Title)
		add(a.Text)
		for _, f := range a.Fields {
			add(strings.TrimSpace(f.Title + ": " + f.Value))
		}
		add(a.Footer)
	}
	for i := range m.Blocks {
		b := &m.Blocks[i]
		switch b.Type {
		case "section", "header":
			if b.Text != nil {
				add(b.Text.Text)
			}
			for _, f := range b.Fields {
				add(f.Text)
			}
		case "context":
			for _, e := range b.Elements {
				add(e.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func snippet(text string) string {
	r := []rune(text)
	if len(r) > 100 {
		return string(r[:100])
	}
	return text
}

// mapMessage converts a Slack Message into a store.Message plus its rendered
// plain-text body. isFromMe is decided by the caller (archiving user's ID).
//
// Legacy-attachment/block text is appended for bot-authored messages and
// used as the body for text-less ones. User messages WITH text keep only
// that text: their attachments are link unfurls, and indexing page previews
// would pollute FTS with content the person never wrote.
func mapMessage(m *Message, channelID string, conversationID, storeSourceID int64, isFromMe bool, lookupName func(string) string) (store.Message, string) {
	text := strings.TrimSpace(renderText(m.Text, lookupName))
	if m.BotID != "" || text == "" {
		if extra := payloadText(m, lookupName); extra != "" {
			if text == "" {
				text = extra
			} else {
				text += "\n" + extra
			}
		}
	}
	if text == "" {
		text = placeholderBody(m)
	}
	t := tsTime(m.TS)
	msg := store.Message{
		ConversationID:  conversationID,
		SourceID:        storeSourceID,
		SourceMessageID: sourceMessageID(channelID, m.TS),
		MessageType:     messageType,
		SentAt:          sql.NullTime{Time: t, Valid: !t.IsZero()},
		ReceivedAt:      sql.NullTime{Time: t, Valid: !t.IsZero()},
		IsFromMe:        isFromMe,
		Snippet:         sql.NullString{String: snippet(text), Valid: text != ""},
		HasAttachments:  len(m.Files) > 0,
		AttachmentCount: len(m.Files),
	}
	return msg, text
}

// conversationType maps a Slack conversation to the msgvault conversation
// type: channels (public/private) → "channel", group DMs → "group_chat",
// DMs → "direct_chat".
func conversationType(c *Conversation) string {
	switch {
	case c.IsIM:
		return "direct_chat"
	case c.IsMpim:
		return "group_chat"
	default:
		return "channel"
	}
}

// conversationTitle renders a display title: "#name" for channels, the peer's
// name for DMs, the member-list name Slack assigns for group DMs.
func conversationTitle(c *Conversation, lookupName func(string) string) string {
	switch {
	case c.IsIM:
		if name := lookupName(c.User); name != "" {
			return name
		}
		return c.User
	case c.IsMpim:
		return c.Name
	default:
		return "#" + c.Name
	}
}
