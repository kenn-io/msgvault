package discord

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

const (
	discordConversationType = "channel"
	discordMessageType      = "discord"
	discordRawFormat        = "discord_json"
)

type mappedConversation struct {
	Conversation store.ConversationPersistData
	Metadata     json.RawMessage
}

type conversationMetadata struct {
	GuildID            string          `json:"guild_id,omitempty"`
	ParentChannelID    string          `json:"parent_channel_id,omitempty"`
	DiscordChannelType int             `json:"discord_channel_type"`
	Topic              string          `json:"topic,omitempty"`
	NSFW               bool            `json:"nsfw,omitempty"`
	OwnerID            string          `json:"owner_id,omitempty"`
	AppliedTagIDs      []string        `json:"applied_tag_ids,omitempty"`
	Thread             *ThreadMetadata `json:"thread,omitempty"`
}

func mapConversation(channel *Channel) (mappedConversation, error) {
	if channel == nil {
		return mappedConversation{}, errors.New("map Discord conversation: channel is nil")
	}
	metadata, err := json.Marshal(conversationMetadata{
		GuildID:            channel.GuildID,
		ParentChannelID:    channel.ParentID,
		DiscordChannelType: channel.Type,
		Topic:              channel.Topic,
		NSFW:               channel.NSFW,
		OwnerID:            channel.OwnerID,
		AppliedTagIDs:      channel.AppliedTags,
		Thread:             channel.ThreadMetadata,
	})
	if err != nil {
		return mappedConversation{}, fmt.Errorf("marshal Discord conversation metadata: %w", err)
	}
	return mappedConversation{
		Conversation: store.ConversationPersistData{
			SourceConversationID: channel.ID,
			ConversationType:     discordConversationType,
			Title:                channel.Name,
		},
		Metadata: metadata,
	}, nil
}

type mappedMessage struct {
	Message     store.Message
	BodyText    string
	Metadata    json.RawMessage
	Raw         []byte
	RawFormat   string
	Recipients  []recipientObservation
	Attachments []store.AttachmentRef
	Edited      bool
}

type reactionSummary struct {
	Emoji    string `json:"emoji"`
	EmojiID  string `json:"emoji_id,omitempty"`
	Animated *bool  `json:"animated,omitempty"`
	Count    int    `json:"count"`
}

type mentionedChannelMetadata struct {
	ID      string `json:"id"`
	GuildID string `json:"guild_id,omitempty"`
	Name    string `json:"name,omitempty"`
	Type    int    `json:"type"`
}

type messageThreadMetadata struct {
	ID              string `json:"id"`
	ParentChannelID string `json:"parent_channel_id,omitempty"`
	Type            int    `json:"type"`
	Name            string `json:"name,omitempty"`
}

type messageMetadata struct {
	DiscordMessageType  int                        `json:"discord_message_type"`
	AuthorKind          string                     `json:"author_kind,omitempty"`
	AuthorDisplayName   string                     `json:"author_display_name,omitempty"`
	AuthorAvatar        string                     `json:"author_avatar,omitempty"`
	GuildNickname       string                     `json:"guild_nickname,omitempty"`
	Automated           bool                       `json:"automated,omitempty"`
	MentionEveryone     bool                       `json:"mention_everyone,omitempty"`
	MentionedRoleIDs    []string                   `json:"mentioned_role_ids,omitempty"`
	MentionedChannels   []mentionedChannelMetadata `json:"mentioned_channels,omitempty"`
	ReferencedMessageID string                     `json:"referenced_message_id,omitempty"`
	ReferencedChannelID string                     `json:"referenced_channel_id,omitempty"`
	ReferencedGuildID   string                     `json:"referenced_guild_id,omitempty"`
	Thread              *messageThreadMetadata     `json:"thread,omitempty"`
	ReactionSummaries   []reactionSummary          `json:"reaction_summaries,omitempty"`
}

func mapMessage(message *Message, conversationID, sourceID int64) (mappedMessage, error) {
	if message == nil {
		return mappedMessage{}, errors.New("map Discord message: message is nil")
	}
	body := renderMessageBody(message)
	raw := append([]byte(nil), message.Raw...)
	if len(raw) == 0 {
		var err error
		raw, err = json.Marshal(message)
		if err != nil {
			return mappedMessage{}, fmt.Errorf("marshal Discord raw message: %w", err)
		}
	}
	metadata, err := json.Marshal(buildMessageMetadata(message))
	if err != nil {
		return mappedMessage{}, fmt.Errorf("marshal Discord message metadata: %w", err)
	}
	attachments := mapAttachments(message.Attachments)
	return mappedMessage{
		Message: store.Message{
			ConversationID:  conversationID,
			SourceID:        sourceID,
			SourceMessageID: message.ID,
			MessageType:     discordMessageType,
			SentAt:          sql.NullTime{Time: message.Timestamp, Valid: !message.Timestamp.IsZero()},
			ReceivedAt:      sql.NullTime{Time: message.Timestamp, Valid: !message.Timestamp.IsZero()},
			Snippet:         sql.NullString{String: discordSnippet(body), Valid: body != ""},
			SizeEstimate:    int64(len(raw)),
			HasAttachments:  len(attachments) > 0,
			AttachmentCount: len(attachments),
		},
		BodyText:    body,
		Metadata:    metadata,
		Raw:         raw,
		RawFormat:   discordRawFormat,
		Recipients:  messageRecipientObservations(message),
		Attachments: attachments,
		Edited:      message.EditedTimestamp != nil && !message.EditedTimestamp.IsZero(),
	}, nil
}

func buildMessageMetadata(message *Message) messageMetadata {
	author := authorObservation(message)
	metadata := messageMetadata{
		DiscordMessageType: message.Type,
		AuthorKind:         author.AuthorKind,
		AuthorDisplayName:  author.DisplayName,
		AuthorAvatar:       author.Avatar,
		GuildNickname:      author.GuildNickname,
		Automated:          author.Automated,
		MentionEveryone:    message.MentionEveryone,
		MentionedRoleIDs:   message.MentionRoles,
	}
	for _, channel := range message.MentionChannels {
		metadata.MentionedChannels = append(metadata.MentionedChannels, mentionedChannelMetadata{
			ID: channel.ID, GuildID: channel.GuildID, Name: channel.Name, Type: channel.Type,
		})
	}
	if reference := message.MessageReference; reference != nil {
		metadata.ReferencedMessageID = reference.MessageID
		metadata.ReferencedChannelID = reference.ChannelID
		metadata.ReferencedGuildID = reference.GuildID
	}
	if thread := message.Thread; thread != nil {
		metadata.Thread = &messageThreadMetadata{
			ID: thread.ID, ParentChannelID: thread.ParentID, Type: thread.Type, Name: thread.Name,
		}
	}
	for _, reaction := range message.Reactions {
		summary := reactionSummary{Emoji: reaction.Emoji.Name, Count: reaction.Count}
		if reaction.Emoji.ID != "" {
			animated := reaction.Emoji.Animated
			summary.EmojiID = reaction.Emoji.ID
			summary.Animated = &animated
		}
		metadata.ReactionSummaries = append(metadata.ReactionSummaries, summary)
	}
	return metadata
}

func discordSnippet(text string) string {
	runes := []rune(text)
	if len(runes) > 100 {
		return string(runes[:100])
	}
	return text
}

var customEmojiPattern = regexp.MustCompile(`<a?:([^:>]+):[0-9]+>`)

func renderMessageBody(message *Message) string {
	if message == nil {
		return ""
	}
	content := resolveDiscordMarkup(message.Content, message)
	var parts []string
	if isAuthoredMessageType(message.Type) {
		if content != "" {
			parts = append(parts, content)
		}
	} else if known := systemMessageTypeName(message.Type); known != "" {
		parts = append(parts, renderSystemMessage(message, content))
	} else {
		parts = append(parts, fmt.Sprintf("[Discord message type %d]", message.Type))
		if content != "" {
			parts = append(parts, content)
		}
	}
	if poll := renderPoll(message.Poll); poll != "" {
		parts = append(parts, poll)
	}
	for _, sticker := range message.StickerItems {
		if sticker.Name != "" {
			parts = append(parts, "[sticker: "+sticker.Name+"]")
		} else {
			parts = append(parts, "[sticker]")
		}
	}
	for _, embed := range message.Embeds {
		if rendered := renderAuthoredEmbed(embed); rendered != "" {
			parts = append(parts, rendered)
		}
	}
	return strings.Join(parts, "\n")
}

func isAuthoredMessageType(messageType int) bool {
	switch messageType {
	case 0, 19, 20, 23:
		return true
	default:
		return false
	}
}

func resolveDiscordMarkup(content string, message *Message) string {
	resolved := content
	for _, user := range message.Mentions {
		name := userDisplayName(user)
		if name == "" {
			name = user.ID
		}
		resolved = strings.ReplaceAll(resolved, "<@"+user.ID+">", "@"+name)
		resolved = strings.ReplaceAll(resolved, "<@!"+user.ID+">", "@"+name)
	}
	for _, channel := range message.MentionChannels {
		name := channel.Name
		if name == "" {
			name = channel.ID
		}
		resolved = strings.ReplaceAll(resolved, "<#"+channel.ID+">", "#"+name)
	}
	return customEmojiPattern.ReplaceAllString(resolved, `:$1:`)
}

func renderPoll(poll *Poll) string {
	if poll == nil {
		return ""
	}
	counts := make(map[int]int)
	if poll.Results != nil {
		for _, result := range poll.Results.AnswerCounts {
			counts[result.ID] = result.Count
		}
	}
	parts := []string{"Poll: " + poll.Question.Text}
	for _, answer := range poll.Answers {
		answerText := answer.PollMedia.Text
		if answer.PollMedia.Emoji != nil && answer.PollMedia.Emoji.Name != "" {
			answerText = answer.PollMedia.Emoji.Name + " " + answerText
		}
		if count, exists := counts[answer.AnswerID]; exists {
			label := "votes"
			if count == 1 {
				label = "vote"
			}
			answerText += " — " + strconv.Itoa(count) + " " + label
		}
		parts = append(parts, "- "+answerText)
	}
	return strings.Join(parts, "\n")
}

func renderAuthoredEmbed(embed Embed) string {
	if embed.Type != "rich" && embed.Type != "poll_result" {
		return ""
	}
	var lines []string
	if embed.Author != nil && embed.Author.Name != "" {
		lines = append(lines, embed.Author.Name)
	}
	if embed.Title != "" {
		lines = append(lines, embed.Title)
	}
	if embed.Description != "" {
		lines = append(lines, embed.Description)
	}
	for _, field := range embed.Fields {
		switch {
		case field.Name != "" && field.Value != "":
			lines = append(lines, field.Name+": "+field.Value)
		case field.Name != "":
			lines = append(lines, field.Name)
		case field.Value != "":
			lines = append(lines, field.Value)
		}
	}
	if embed.Footer != nil && embed.Footer.Text != "" {
		lines = append(lines, embed.Footer.Text)
	}
	return strings.Join(lines, "\n")
}

func systemMessageTypeName(messageType int) string {
	names := map[int]string{
		1: "recipient_add", 2: "recipient_remove", 3: "call", 4: "channel_name_change",
		5: "channel_icon_change", 6: "channel_pinned_message", 7: "user_join", 8: "guild_boost",
		9: "guild_boost_tier_1", 10: "guild_boost_tier_2", 11: "guild_boost_tier_3",
		12: "channel_follow_add", 14: "guild_discovery_disqualified", 15: "guild_discovery_requalified",
		16: "guild_discovery_initial_warning", 17: "guild_discovery_final_warning", 18: "thread_created",
		21: "thread_starter_message", 22: "guild_invite_reminder", 24: "auto_moderation_action",
		25: "role_subscription_purchase", 26: "interaction_premium_upsell", 27: "stage_start",
		28: "stage_end", 29: "stage_speaker", 31: "stage_topic",
		32: "guild_application_premium_subscription", 36: "guild_incident_alert_mode_enabled",
		37: "guild_incident_alert_mode_disabled", 38: "guild_incident_report_raid",
		39: "guild_incident_report_false_alarm", 44: "purchase_notification", 46: "poll_result",
	}
	return names[messageType]
}

func renderSystemMessage(message *Message, content string) string {
	author := userDisplayName(message.Author)
	if message.Member != nil && strings.TrimSpace(message.Member.Nick) != "" {
		author = strings.TrimSpace(message.Member.Nick)
	}
	if author == "" {
		author = "A Discord user"
	}
	detail := func(prefix string) string {
		if content == "" {
			return prefix + "."
		}
		return prefix + ": " + content + "."
	}
	switch message.Type {
	case 1:
		return detail(author + " added a recipient")
	case 2:
		return detail(author + " removed a recipient")
	case 3:
		return detail(author + " started a call")
	case 4:
		return detail(author + " changed the channel name")
	case 5:
		return author + " changed the channel icon."
	case 6:
		text := author + " pinned a message."
		if message.MessageReference != nil && message.MessageReference.MessageID != "" {
			text += " (message " + message.MessageReference.MessageID + ")"
		}
		return text
	case 7:
		return author + " joined the server."
	case 8:
		return author + " boosted the server."
	case 9:
		return author + " boosted the server to level 1."
	case 10:
		return author + " boosted the server to level 2."
	case 11:
		return author + " boosted the server to level 3."
	case 12:
		return detail(author + " followed an announcement channel")
	case 14:
		return "This server was removed from Server Discovery."
	case 15:
		return "This server became eligible for Server Discovery again."
	case 16:
		return "Server Discovery eligibility is at risk."
	case 17:
		return "Final warning: Server Discovery eligibility is at risk."
	case 18:
		return detail(author + " created a thread")
	case 21:
		return "Thread starter message."
	case 22:
		return "Invite friends to this server."
	case 24:
		return detail("Discord AutoMod took an action")
	case 25:
		return detail(author + " purchased or renewed a role subscription")
	case 26:
		return "Discord displayed a premium interaction upsell."
	case 27:
		return detail(author + " started the stage")
	case 28:
		return author + " ended the stage."
	case 29:
		return detail(author + " became a stage speaker")
	case 31:
		return detail(author + " changed the stage topic")
	case 32:
		return detail(author + " subscribed to a premium application")
	case 36:
		return detail(author + " enabled incident alert mode")
	case 37:
		return author + " disabled incident alert mode."
	case 38:
		return detail(author + " reported a raid")
	case 39:
		return detail(author + " reported a false alarm")
	case 44:
		return detail(author + " made a purchase")
	case 46:
		return "Poll results were finalized."
	default:
		return fmt.Sprintf("[Discord message type %d]", message.Type)
	}
}

func mapAttachments(attachments []Attachment) []store.AttachmentRef {
	refs := make([]store.AttachmentRef, 0, len(attachments))
	for _, attachment := range attachments {
		ref := store.AttachmentRef{
			Filename:           attachment.Filename,
			MimeType:           attachment.ContentType,
			StoragePath:        attachment.URL,
			Size:               int(attachment.Size),
			SourceAttachmentID: "discord:" + attachment.ID,
			MediaType:          attachmentMediaType(attachment.ContentType),
			DurationMS:         int64(math.Round(attachment.Duration * 1000)),
		}
		if attachment.Width != nil {
			ref.Width = int64(*attachment.Width)
		}
		if attachment.Height != nil {
			ref.Height = int64(*attachment.Height)
		}
		refs = append(refs, ref)
	}
	return refs
}

func attachmentMediaType(contentType string) string {
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return "image"
	case strings.HasPrefix(contentType, "video/"):
		return "video"
	case strings.HasPrefix(contentType, "audio/"):
		return "audio"
	default:
		return "document"
	}
}
