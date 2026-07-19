package discord

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestMapConversationUsesChannelForChannelsAndThreads(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name    string
		channel Channel
		want    mappedConversation
	}{
		{
			name: "text channel",
			channel: Channel{
				ID: "10", GuildID: "20", Type: 0, Name: "general", Topic: "General chat", NSFW: true,
			},
			want: mappedConversation{
				Conversation: store.ConversationPersistData{
					SourceConversationID: "10", ConversationType: "channel", Title: "general",
				},
				Metadata: json.RawMessage(`{"guild_id":"20","discord_channel_type":0,"topic":"General chat","nsfw":true}`),
			},
		},
		{
			name: "archived thread",
			channel: Channel{
				ID: "11", GuildID: "20", ParentID: "10", Type: 11, Name: "topic thread", OwnerID: "30",
				AppliedTags: []string{"tag-1"},
				ThreadMetadata: &ThreadMetadata{
					Archived: true, Locked: true, Invitable: true, AutoArchiveDuration: 1440,
					ArchiveTimestamp: created.Add(time.Hour), CreateTimestamp: &created,
				},
			},
			want: mappedConversation{
				Conversation: store.ConversationPersistData{
					SourceConversationID: "11", ConversationType: "channel", Title: "topic thread",
				},
				Metadata: json.RawMessage(`{"guild_id":"20","parent_channel_id":"10","discord_channel_type":11,"owner_id":"30","applied_tag_ids":["tag-1"],"thread":{"archived":true,"auto_archive_duration":1440,"archive_timestamp":"2026-01-02T04:04:05Z","locked":true,"invitable":true,"create_timestamp":"2026-01-02T03:04:05Z"}}`),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			got, err := mapConversation(&tt.channel)
			require.NoError(err)
			assert.Equal(tt.want.Conversation, got.Conversation)
			assert.JSONEq(string(tt.want.Metadata), string(got.Metadata))
			assert.Empty(got.Conversation.Participants, "catalog rosters must not become conversation participants")
		})
	}
}

func TestMapMessageBasicsMentionsRepliesAndRaw(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	sent := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	edited := sent.Add(time.Minute)
	raw := `{"id":"300","channel_id":"200","guild_id":"100","type":19,"content":"Hello <@400> in <#500> <:wave:600>","unknown_future_field":{"kept":true}}`
	var msg Message
	require.NoError(json.Unmarshal([]byte(raw), &msg))
	msg.Timestamp = sent
	msg.EditedTimestamp = &edited
	msg.Author = User{ID: "350", Username: "alice", Bot: true, Avatar: "bot-avatar"}
	msg.Member = &GuildMember{Nick: "Guild Alice"}
	msg.Mentions = []User{{ID: "400", Username: "bob", GlobalName: "Bob Builder"}}
	msg.MentionChannels = []ChannelMention{{ID: "500", Name: "announcements"}}
	msg.MentionRoles = []string{"700"}
	msg.MentionEveryone = true
	msg.MessageReference = &MessageReference{MessageID: "250", ChannelID: "200", GuildID: "100"}
	msg.Reactions = []Reaction{
		{Emoji: Emoji{Name: "👍"}, Count: 3},
		{Emoji: Emoji{ID: "600", Name: "wave"}, Count: 2},
	}
	msg.Thread = &Channel{ID: "800", ParentID: "200", Type: 11, Name: "reply thread"}

	got, err := mapMessage(&msg, 10, 20)
	require.NoError(err)
	assert.Equal("discord", got.Message.MessageType)
	assert.Equal("300", got.Message.SourceMessageID)
	assert.Equal(int64(10), got.Message.ConversationID)
	assert.Equal(int64(20), got.Message.SourceID)
	assert.Equal(sent, got.Message.SentAt.Time)
	assert.True(got.Message.SentAt.Valid)
	assert.Equal("Hello @Bob Builder in #announcements :wave:", got.BodyText)
	assert.Contains(got.Message.Snippet.String, "@Bob Builder")
	assert.True(got.Edited)
	assert.Equal("discord_json", got.RawFormat)
	assert.JSONEq(raw, string(got.Raw))
	assert.Equal([]recipientObservation{
		{Type: "from", Participant: participantObservation{
			IdentifierType: "discord_user_id", IdentifierValue: "350", DisplayName: "alice", Avatar: "bot-avatar",
			GuildNickname: "Guild Alice", AuthorKind: authorKindBot, Automated: true,
		}},
		{Type: "mention", Participant: participantObservation{
			IdentifierType: "discord_user_id", IdentifierValue: "400", DisplayName: "Bob Builder", AuthorKind: authorKindUser,
		}},
	}, got.Recipients)
	assert.JSONEq(`{
		"discord_message_type":19,
		"author_kind":"bot",
		"author_display_name":"alice",
		"author_avatar":"bot-avatar",
		"guild_nickname":"Guild Alice",
		"automated":true,
		"mention_everyone":true,
		"mentioned_role_ids":["700"],
		"mentioned_channels":[{"id":"500","name":"announcements","type":0}],
		"referenced_message_id":"250",
		"referenced_channel_id":"200",
		"referenced_guild_id":"100",
		"thread":{"id":"800","parent_channel_id":"200","type":11,"name":"reply thread"},
		"reaction_summaries":[
			{"emoji":"👍","count":3},
			{"emoji":"wave","emoji_id":"600","animated":false,"count":2}
		]
	}`, string(got.Metadata))
}

func TestMapWebhookPresentationOverridesRemainPerMessage(t *testing.T) {
	msg := &Message{
		ID: "1", WebhookID: "300", Type: 0, Content: "deployed",
		Author: User{Username: "Production deploy", Avatar: "presentation-avatar"},
	}

	got, err := mapMessage(msg, 30, 40)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"discord_message_type":0,
		"author_kind":"webhook",
		"author_display_name":"Production deploy",
		"author_avatar":"presentation-avatar",
		"automated":true
	}`, string(got.Metadata))
}

func TestRenderMessageBodyIncludesAuthoredRichContent(t *testing.T) {
	assert := assert.New(t)
	msg := &Message{
		Content: "Choose a route",
		Poll: &Poll{
			Question: PollMedia{Text: "Where next?"},
			Answers: []PollAnswer{
				{AnswerID: 1, PollMedia: PollMedia{Text: "Moon", Emoji: &Emoji{Name: "🌕"}}},
				{AnswerID: 2, PollMedia: PollMedia{Text: "Mars"}},
			},
			Results: &PollResults{AnswerCounts: []PollAnswerCount{{ID: 1, Count: 4}, {ID: 2, Count: 2}}},
		},
		StickerItems: []StickerItem{{ID: "1", Name: "party parrot"}},
		Embeds: []Embed{
			{
				Type: "rich", Author: &EmbedAuthor{Name: "Release bot"}, Title: "Release 1.2", Description: "Ready to ship",
				Fields: []EmbedField{{Name: "Status", Value: "Green"}}, Footer: &EmbedFooter{Text: "Signed build"},
			},
			{Type: "article", Title: "Unfurled article", Description: "Link preview copy", URL: "https://example.com/article"},
		},
	}

	got := renderMessageBody(msg)
	assert.Contains(got, "Choose a route")
	assert.Contains(got, "Poll: Where next?")
	assert.Contains(got, "🌕 Moon — 4 votes")
	assert.Contains(got, "Mars — 2 votes")
	assert.Contains(got, "[sticker: party parrot]")
	assert.Contains(got, "Release bot")
	assert.Contains(got, "Release 1.2")
	assert.Contains(got, "Status: Green")
	assert.Contains(got, "Signed build")
	assert.NotContains(got, "Unfurled article")
	assert.NotContains(got, "Link preview copy")
}

func TestKnownDiscordSystemMessagesRenderReadableText(t *testing.T) {
	outerAssert := assert.New(t)
	knownTypes := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 14, 15, 16, 17, 18, 21, 22, 24, 25, 26, 27, 28, 29, 31, 32, 36, 37, 38, 39, 44, 46}
	for _, messageType := range knownTypes {
		t.Run(systemMessageTypeName(messageType), func(t *testing.T) {
			assert := assert.New(t)
			body := renderMessageBody(&Message{
				Type: messageType, Content: "provider detail", Author: User{Username: "alice"},
				MessageReference: &MessageReference{MessageID: "100", ChannelID: "200"},
			})
			assert.NotEmpty(body)
			assert.NotContains(body, "unknown")
			assert.NotContains(body, "type ")
		})
	}

	outerAssert.Equal("alice joined the server.", renderMessageBody(&Message{Type: 7, Author: User{Username: "alice"}}))
	outerAssert.Equal("alice boosted the server to level 2.", renderMessageBody(&Message{Type: 10, Author: User{Username: "alice"}}))
	outerAssert.Equal("alice pinned a message. (message 100)", renderMessageBody(&Message{
		Type: 6, Author: User{Username: "alice"}, MessageReference: &MessageReference{MessageID: "100"},
	}))
}

func TestUnknownDiscordMessageTypeFallsBackConservatively(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	msg := &Message{ID: "1", Type: 999, Content: "future provider detail", Raw: []byte(`{"id":"1","type":999,"future":true}`)}
	got, err := mapMessage(msg, 11, 21)
	require.NoError(err)
	assert.Equal("[Discord message type 999]\nfuture provider detail", got.BodyText)
	assert.JSONEq(`{"discord_message_type":999}`, string(got.Metadata))
	assert.JSONEq(`{"id":"1","type":999,"future":true}`, string(got.Raw))
}

func TestMapDiscordAttachments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	height, width := 480, 640
	msg := &Message{Attachments: []Attachment{
		{
			ID: "a1", Filename: "image.png", ContentType: "image/png", Size: 4096,
			URL: "https://cdn.discordapp.com/attachments/1/a1/image.png", Height: &height, Width: &width,
		},
		{
			ID: "a2", Filename: "voice.ogg", ContentType: "audio/ogg", Size: 8192,
			URL: "https://cdn.discordapp.com/attachments/1/a2/voice.ogg", Duration: 1.25,
		},
	}}

	got := mapAttachments(msg.Attachments)
	assert.Equal([]store.AttachmentRef{
		{
			Filename: "image.png", MimeType: "image/png", Size: 4096,
			StoragePath:        "https://cdn.discordapp.com/attachments/1/a1/image.png",
			SourceAttachmentID: "discord:a1", MediaType: "image", Width: 640, Height: 480,
		},
		{
			Filename: "voice.ogg", MimeType: "audio/ogg", Size: 8192,
			StoragePath:        "https://cdn.discordapp.com/attachments/1/a2/voice.ogg",
			SourceAttachmentID: "discord:a2", MediaType: "audio", DurationMS: 1250,
		},
	}, got)

	mapped, err := mapMessage(msg, 12, 22)
	require.NoError(err)
	assert.True(mapped.Message.HasAttachments)
	assert.Equal(2, mapped.Message.AttachmentCount)
	assert.Equal(got, mapped.Attachments)
}
