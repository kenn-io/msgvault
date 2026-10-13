package imazingcsv

import (
	"strings"

	"go.kenn.io/msgvault/internal/imazingidentity"
	"go.kenn.io/msgvault/internal/store"
)

type identityKind string

const (
	identityPhone  identityKind = "phone"
	identityEmail  identityKind = "email"
	identityOpaque identityKind = "opaque"
)

type participantIdentity struct {
	Kind        identityKind
	Value       string
	DisplayName string
}

func (identity participantIdentity) key() string {
	return string(identity.Kind) + ":" + identity.Value
}

func normalizeIdentity(value, displayName string) (participantIdentity, error) {
	identity, err := imazingidentity.Normalize(value, displayName)
	return participantIdentity{Kind: identityKind(identity.Kind), Value: identity.Value, DisplayName: identity.DisplayName}, err
}

func syntheticIdentity(sourceIdentifier, chatKey, category, displayName string) participantIdentity {
	identity := imazingidentity.Synthetic(sourceIdentifier, chatKey, category, displayName)
	return participantIdentity{Kind: identityKind(identity.Kind), Value: identity.Value, DisplayName: identity.DisplayName}
}

func participantPersistence(identity participantIdentity) store.ParticipantPersistData {
	data := store.ParticipantPersistData{DisplayName: identity.DisplayName}
	switch identity.Kind {
	case identityPhone:
		data.PhoneNumber = identity.Value
		data.IdentifierType = SourceType
	case identityEmail:
		data.EmailAddress = identity.Value
		if at := strings.LastIndexByte(identity.Value, '@'); at >= 0 {
			data.Domain = identity.Value[at+1:]
		}
	case identityOpaque:
		data.IdentifierType = SourceType
		data.IdentifierValue = identity.Value
	}
	return data
}

func normalizeChat(value string) string         { return imazingidentity.NormalizeChat(value) }
func stableHash(parts ...string) string         { return imazingidentity.Hash(parts...) }
func conversationKey(chatSession string) string { return imazingidentity.ConversationKey(chatSession) }

// messageBaseID hashes only the fields that identify one occurrence of a
// message across exports: its conversation, the parsed send instant, the
// direction, the sender, and the service. Subject, text, attachment, and the
// raw date spelling are mutable presentation, so hashing them would archive
// every edited or reformatted row as a brand-new message; they instead act
// as occurrence evidence during duplicate reconciliation.
func messageBaseID(row Row, chatKey string, sender participantIdentity) string {
	return imazingidentity.MessageBase(chatKey, row.SentAt, string(row.Direction), sender.key(), row.Service)
}
