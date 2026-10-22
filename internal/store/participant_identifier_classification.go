package store

import "strings"

type participantIdentifierClassification struct {
	ServiceSlug string
	ScopeKind   *string
	ScopeValue  *string
}

func classifyParticipantIdentifier(
	identifierType, identifierValue string,
) (participantIdentifierClassification, bool) {
	kind := strings.ToLower(strings.TrimSpace(identifierType))
	value := strings.TrimSpace(identifierValue)
	classification := participantIdentifierClassification{}
	switch {
	case kind == "imessage":
		classification.ServiceSlug = "imessage"
	case kind == "whatsapp":
		classification.ServiceSlug = "whatsapp"
	case kind == sourceTypeMatrix:
		classification.ServiceSlug = sourceTypeMatrix
		if separator := strings.Index(value, ":"); separator > 0 && separator+1 < len(value) {
			classification.ScopeKind = new("server")
			classification.ScopeValue = new(value[separator+1:])
		} else {
			return participantIdentifierClassification{}, false
		}
	case kind == sourceTypeDiscord || strings.HasPrefix(kind, "discord_"):
		classification.ServiceSlug = sourceTypeDiscord
	case kind == "synctech-sms" || kind == "synctech_sms" || kind == "sms":
		classification.ServiceSlug = "sms"
	case kind == "google_voice" || kind == "google-voice":
		classification.ServiceSlug = "google-voice"
	case kind == sourceTypeSlack:
		classification.ServiceSlug = sourceTypeSlack
		if separator := strings.Index(value, ":"); separator > 0 {
			classification.ScopeKind = new("workspace")
			classification.ScopeValue = new(value[:separator])
		} else {
			return participantIdentifierClassification{}, false
		}
	default:
		return participantIdentifierClassification{}, false
	}
	return classification, true
}

func participantIdentifierClassificationValues(
	identifierType, identifierValue string,
) (serviceSlug, scopeKind, scopeValue any) {
	classification, ok := classifyParticipantIdentifier(identifierType, identifierValue)
	if !ok {
		return nil, nil, nil
	}
	return classification.ServiceSlug,
		stringValue(classification.ScopeKind), stringValue(classification.ScopeValue)
}
