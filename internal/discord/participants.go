package discord

import (
	"encoding/json"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

const (
	discordUserIdentifier        = "discord_user_id"
	discordWebhookIdentifier     = "discord_webhook_id"
	discordApplicationIdentifier = "discord_application_id"
	discordAutomatedIdentifier   = "discord_automated_id"
	authorKindUser               = "user"
	authorKindBot                = "bot"
	authorKindWebhook            = "webhook"
	authorKindApplication        = "application"
)

// participantObservation is an identity observed on one message. Stable
// provider identity is intentionally separate from guild- and message-local
// presentation so nicknames and webhook overrides cannot fork participants.
type participantObservation struct {
	IdentifierType  string
	IdentifierValue string
	DisplayName     string
	Avatar          string
	GuildNickname   string
	AuthorKind      string
	Automated       bool
}

type recipientObservation struct {
	Type        string
	Participant participantObservation
}

type participantResolver struct {
	store *store.Store
	cache map[string]int64
}

func newParticipantResolver(s *store.Store) *participantResolver {
	return &participantResolver{store: s, cache: make(map[string]int64)}
}

func (r *participantResolver) resolve(observation participantObservation) (int64, error) {
	if observation.IdentifierType == "" || observation.IdentifierValue == "" {
		return 0, nil
	}
	key := observation.IdentifierType + "\x00" + observation.IdentifierValue
	if participantID, ok := r.cache[key]; ok {
		return participantID, nil
	}
	participantID, err := r.store.EnsureParticipantByIdentifier(
		observation.IdentifierType,
		observation.IdentifierValue,
		observation.DisplayName,
	)
	if err != nil {
		return 0, err
	}
	r.cache[key] = participantID
	return participantID, nil
}

func authorObservation(message *Message) participantObservation {
	if message == nil {
		return participantObservation{}
	}
	if message.WebhookID != "" {
		return participantObservation{
			IdentifierType:  discordWebhookIdentifier,
			IdentifierValue: message.WebhookID,
			DisplayName:     userDisplayName(message.Author),
			Avatar:          message.Author.Avatar,
			AuthorKind:      authorKindWebhook,
			Automated:       true,
		}
	}
	if message.Author.ID != "" {
		kind := authorKindUser
		if message.Author.Bot {
			kind = authorKindBot
		}
		observation := participantObservation{
			IdentifierType:  discordUserIdentifier,
			IdentifierValue: message.Author.ID,
			DisplayName:     userDisplayName(message.Author),
			Avatar:          message.Author.Avatar,
			AuthorKind:      kind,
			Automated:       message.Author.Bot || message.Author.System,
		}
		if message.Member != nil {
			observation.GuildNickname = message.Member.Nick
		}
		return observation
	}

	applicationID, applicationName, hasApplication := applicationIdentity(message)
	if !hasApplication {
		return participantObservation{}
	}
	identifierType := discordApplicationIdentifier
	identifierValue := applicationID
	if identifierValue == "" {
		identifierType = discordAutomatedIdentifier
		scope := message.GuildID
		if scope == "" {
			scope = "channel:" + message.ChannelID
		} else {
			scope = "guild:" + scope
		}
		identifierValue = scope + ":application"
	}
	if applicationName == "" {
		applicationName = "Discord application"
	}
	return participantObservation{
		IdentifierType:  identifierType,
		IdentifierValue: identifierValue,
		DisplayName:     applicationName,
		AuthorKind:      authorKindApplication,
		Automated:       true,
	}
}

func applicationIdentity(message *Message) (id, name string, ok bool) {
	if message.ApplicationID != "" {
		id = message.ApplicationID
		ok = true
	}
	if len(message.Application) == 0 || string(message.Application) == "null" {
		return id, name, ok
	}
	var application struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if json.Unmarshal(message.Application, &application) == nil {
		if id == "" {
			id = application.ID
		}
		name = application.Name
	}
	return id, name, true
}

func messageRecipientObservations(message *Message) []recipientObservation {
	if message == nil {
		return nil
	}
	recipients := make([]recipientObservation, 0, 1+len(message.Mentions))
	if author := authorObservation(message); author.IdentifierValue != "" {
		recipients = append(recipients, recipientObservation{Type: "from", Participant: author})
	}
	seen := make(map[string]struct{}, len(message.Mentions))
	for _, mention := range message.Mentions {
		if mention.ID == "" {
			continue
		}
		if _, duplicate := seen[mention.ID]; duplicate {
			continue
		}
		seen[mention.ID] = struct{}{}
		kind := authorKindUser
		if mention.Bot {
			kind = authorKindBot
		}
		recipients = append(recipients, recipientObservation{
			Type: "mention",
			Participant: participantObservation{
				IdentifierType:  discordUserIdentifier,
				IdentifierValue: mention.ID,
				DisplayName:     userDisplayName(mention),
				Avatar:          mention.Avatar,
				AuthorKind:      kind,
				Automated:       mention.Bot || mention.System,
			},
		})
	}
	return recipients
}

func userDisplayName(user User) string {
	if name := strings.TrimSpace(user.GlobalName); name != "" {
		return name
	}
	return strings.TrimSpace(user.Username)
}
