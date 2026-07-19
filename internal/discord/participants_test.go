package discord

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAuthorObservation(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
		want participantObservation
	}{
		{
			name: "regular user with guild nickname observation",
			msg: Message{
				Author: User{ID: "100", Username: "alice", GlobalName: "Alice", Avatar: "user-avatar"},
				Member: &GuildMember{Nick: "Guild Alice"},
			},
			want: participantObservation{
				IdentifierType:          "discord_user_id",
				IdentifierValue:         "100",
				ParticipantLabel:        "Alice",
				PresentationDisplayName: "Alice",
				PresentationAvatar:      "user-avatar",
				GuildNickname:           "Guild Alice",
				AuthorKind:              authorKindUser,
			},
		},
		{
			name: "bot keeps global user identity and automation flag",
			msg:  Message{Author: User{ID: "200", Username: "archive-bot", Bot: true}},
			want: participantObservation{
				IdentifierType:          "discord_user_id",
				IdentifierValue:         "200",
				ParticipantLabel:        "archive-bot",
				PresentationDisplayName: "archive-bot",
				AuthorKind:              authorKindBot,
				Automated:               true,
			},
		},
		{
			name: "webhook uses webhook identity and message presentation",
			msg: Message{
				WebhookID: "300",
				Author:    User{ID: "ignored-webhook-user", Username: "Deploy hook", Avatar: "override-avatar"},
			},
			want: participantObservation{
				IdentifierType:          "discord_webhook_id",
				IdentifierValue:         "300",
				ParticipantLabel:        "Discord webhook 300",
				PresentationDisplayName: "Deploy hook",
				PresentationAvatar:      "override-avatar",
				AuthorKind:              authorKindWebhook,
				Automated:               true,
			},
		},
		{
			name: "application falls back to stable application identity",
			msg: Message{
				GuildID:       "400",
				ApplicationID: "500",
				Application:   []byte(`{"id":"500","name":"Reminder app"}`),
			},
			want: participantObservation{
				IdentifierType:          "discord_application_id",
				IdentifierValue:         "500",
				ParticipantLabel:        "Reminder app",
				PresentationDisplayName: "Reminder app",
				AuthorKind:              authorKindApplication,
				Automated:               true,
			},
		},
		{
			name: "application without id gets provider scoped automation identity",
			msg: Message{
				GuildID:     "400",
				ChannelID:   "401",
				Application: []byte(`{"name":"Legacy app"}`),
			},
			want: participantObservation{
				IdentifierType:          "discord_automated_id",
				IdentifierValue:         "guild:400:application",
				ParticipantLabel:        "Legacy app",
				PresentationDisplayName: "Legacy app",
				AuthorKind:              authorKindApplication,
				Automated:               true,
			},
		},
		{
			name: "empty author has no identity",
			msg:  Message{},
			want: participantObservation{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, authorObservation(&tt.msg))
		})
	}
}

func TestParticipantObservationsKeepIdentitySeparateFromPresentation(t *testing.T) {
	assert := assert.New(t)

	firstWebhook := authorObservation(&Message{
		WebhookID: "300",
		Author:    User{Username: "First presentation", Avatar: "first-avatar"},
	})
	secondWebhook := authorObservation(&Message{
		WebhookID: "300",
		Author:    User{Username: "Second presentation", Avatar: "second-avatar"},
	})
	assert.Equal(firstWebhook.IdentifierType, secondWebhook.IdentifierType)
	assert.Equal(firstWebhook.IdentifierValue, secondWebhook.IdentifierValue)
	assert.Equal("Discord webhook 300", firstWebhook.ParticipantLabel)
	assert.Equal(firstWebhook.ParticipantLabel, secondWebhook.ParticipantLabel)
	assert.Equal("First presentation", firstWebhook.PresentationDisplayName)
	assert.Equal("Second presentation", secondWebhook.PresentationDisplayName)
	assert.Equal("first-avatar", firstWebhook.PresentationAvatar)
	assert.Equal("second-avatar", secondWebhook.PresentationAvatar)

	firstUser := authorObservation(&Message{
		GuildID: "guild-a",
		Author:  User{ID: "100", Username: "alice@example.com"},
		Member:  &GuildMember{Nick: "Guild A Alice"},
	})
	secondUser := authorObservation(&Message{
		GuildID: "guild-b",
		Author:  User{ID: "100", Username: "renamed-alice"},
		Member:  &GuildMember{Nick: "Guild B Alice"},
	})
	assert.Equal("discord_user_id", firstUser.IdentifierType)
	assert.Equal("100", firstUser.IdentifierValue)
	assert.Equal(firstUser.IdentifierType, secondUser.IdentifierType)
	assert.Equal(firstUser.IdentifierValue, secondUser.IdentifierValue)
	assert.Equal("alice@example.com", firstUser.ParticipantLabel)
	assert.Equal("renamed-alice", secondUser.ParticipantLabel)
	assert.Equal("Guild A Alice", firstUser.GuildNickname)
	assert.Equal("Guild B Alice", secondUser.GuildNickname)
}

func TestMessageRecipientObservationsOnlyIncludeAuthorAndMentions(t *testing.T) {
	msg := &Message{
		Author: User{ID: "100", Username: "alice"},
		Mentions: []User{
			{ID: "200", Username: "bob"},
			{ID: "200", Username: "bob duplicate"},
			{ID: "300", Username: "carol", Bot: true},
		},
	}

	assert.Equal(t, []recipientObservation{
		{Type: "from", Participant: participantObservation{
			IdentifierType: "discord_user_id", IdentifierValue: "100", ParticipantLabel: "alice",
			PresentationDisplayName: "alice", AuthorKind: authorKindUser,
		}},
		{Type: "mention", Participant: participantObservation{
			IdentifierType: "discord_user_id", IdentifierValue: "200", ParticipantLabel: "bob",
			PresentationDisplayName: "bob", AuthorKind: authorKindUser,
		}},
		{Type: "mention", Participant: participantObservation{
			IdentifierType: "discord_user_id", IdentifierValue: "300", ParticipantLabel: "carol",
			PresentationDisplayName: "carol", AuthorKind: authorKindBot, Automated: true,
		}},
	}, messageRecipientObservations(msg))
}
