package discord

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
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
				IdentifierType:  "discord_user_id",
				IdentifierValue: "100",
				DisplayName:     "Alice",
				Avatar:          "user-avatar",
				GuildNickname:   "Guild Alice",
				AuthorKind:      authorKindUser,
			},
		},
		{
			name: "bot keeps global user identity and automation flag",
			msg:  Message{Author: User{ID: "200", Username: "archive-bot", Bot: true}},
			want: participantObservation{
				IdentifierType:  "discord_user_id",
				IdentifierValue: "200",
				DisplayName:     "archive-bot",
				AuthorKind:      authorKindBot,
				Automated:       true,
			},
		},
		{
			name: "webhook uses webhook identity and message presentation",
			msg: Message{
				WebhookID: "300",
				Author:    User{ID: "ignored-webhook-user", Username: "Deploy hook", Avatar: "override-avatar"},
			},
			want: participantObservation{
				IdentifierType:  "discord_webhook_id",
				IdentifierValue: "300",
				DisplayName:     "Deploy hook",
				Avatar:          "override-avatar",
				AuthorKind:      authorKindWebhook,
				Automated:       true,
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
				IdentifierType:  "discord_application_id",
				IdentifierValue: "500",
				DisplayName:     "Reminder app",
				AuthorKind:      authorKindApplication,
				Automated:       true,
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
				IdentifierType:  "discord_automated_id",
				IdentifierValue: "guild:400:application",
				DisplayName:     "Legacy app",
				AuthorKind:      authorKindApplication,
				Automated:       true,
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

func TestParticipantResolverUsesStableProviderIdentifiers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	resolver := newParticipantResolver(st)

	firstWebhook := authorObservation(&Message{
		WebhookID: "300",
		Author:    User{Username: "First presentation", Avatar: "first-avatar"},
	})
	secondWebhook := authorObservation(&Message{
		WebhookID: "300",
		Author:    User{Username: "Second presentation", Avatar: "second-avatar"},
	})
	firstID, err := resolver.resolve(firstWebhook)
	require.NoError(err)
	secondID, err := resolver.resolve(secondWebhook)
	require.NoError(err)
	assert.Equal(firstID, secondID)
	storedWebhookID, _, err := st.ParticipantByIdentifier("discord_webhook_id", "300")
	require.NoError(err)
	assert.Equal(firstID, storedWebhookID)
	assert.Equal("First presentation", firstWebhook.DisplayName)
	assert.Equal("Second presentation", secondWebhook.DisplayName)

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
	firstUserID, err := resolver.resolve(firstUser)
	require.NoError(err)
	secondUserID, err := resolver.resolve(secondUser)
	require.NoError(err)
	assert.Equal(firstUserID, secondUserID)
	storedUserID, _, err := st.ParticipantByIdentifier("discord_user_id", "100")
	require.NoError(err)
	assert.Equal(firstUserID, storedUserID)
	emailID, _, err := st.ParticipantByIdentifier("email", "alice@example.com")
	require.NoError(err)
	assert.Zero(emailID, "Discord usernames must not trigger email unification")
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
			IdentifierType: "discord_user_id", IdentifierValue: "100", DisplayName: "alice", AuthorKind: authorKindUser,
		}},
		{Type: "mention", Participant: participantObservation{
			IdentifierType: "discord_user_id", IdentifierValue: "200", DisplayName: "bob", AuthorKind: authorKindUser,
		}},
		{Type: "mention", Participant: participantObservation{
			IdentifierType: "discord_user_id", IdentifierValue: "300", DisplayName: "carol", AuthorKind: authorKindBot, Automated: true,
		}},
	}, messageRecipientObservations(msg))
}
