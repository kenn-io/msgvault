package imazingcsv

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textimport"
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
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return participantIdentity{}, errors.New("identity is empty")
	}
	if phone, err := textimport.NormalizePhone(trimmed); err == nil {
		return participantIdentity{Kind: identityPhone, Value: phone, DisplayName: strings.TrimSpace(displayName)}, nil
	}
	if address, err := mail.ParseAddress(trimmed); err == nil &&
		strings.Contains(address.Address, "@") && strings.EqualFold(address.Address, trimmed) {
		return participantIdentity{
			Kind: identityEmail, Value: strings.ToLower(address.Address), DisplayName: strings.TrimSpace(displayName),
		}, nil
	}
	return participantIdentity{Kind: identityOpaque, Value: trimmed, DisplayName: strings.TrimSpace(displayName)}, nil
}

func syntheticIdentity(sourceIdentifier, chatKey, category, displayName string) participantIdentity {
	value := strings.Join([]string{
		"synthetic", sourceIdentifier, chatKey, category, normalizeChat(displayName),
	}, ":")
	return participantIdentity{
		Kind: identityOpaque, Value: value, DisplayName: strings.TrimSpace(displayName),
	}
}

func ensureIdentity(st *store.Store, identity participantIdentity) (int64, error) {
	switch identity.Kind {
	case identityPhone:
		return st.EnsureParticipantByPhone(identity.Value, identity.DisplayName, SourceType)
	case identityEmail:
		domain := ""
		if at := strings.LastIndexByte(identity.Value, '@'); at >= 0 {
			domain = identity.Value[at+1:]
		}
		return st.EnsureParticipant(identity.Value, identity.DisplayName, domain)
	case identityOpaque:
		return st.EnsureParticipantByIdentifier(SourceType, identity.Value, identity.DisplayName)
	default:
		return 0, fmt.Errorf("unsupported identity kind %q", identity.Kind)
	}
}

func normalizeChat(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func stableHash(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(hash, "%d:", len(part))
		_, _ = hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func conversationKey(chatSession string) string {
	return "imazing_csv:" + stableHash(normalizeChat(chatSession))
}

// messageBaseID hashes only the fields that identify one occurrence of a
// message across exports: its conversation, the parsed send instant, the
// direction, the sender, and the service. Subject, text, attachment, and the
// raw date spelling are mutable presentation, so hashing them would archive
// every edited or reformatted row as a brand-new message; they instead act
// as occurrence evidence during duplicate reconciliation.
func messageBaseID(row Row, chatKey string, sender participantIdentity) string {
	return stableHash(
		chatKey,
		row.SentAt.UTC().Format(time.RFC3339Nano),
		string(row.Direction),
		sender.key(),
		row.Service,
	)
}
