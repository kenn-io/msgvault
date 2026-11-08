package store

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

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
	case kind == "telegram":
		classification.ServiceSlug = "telegram"
	case kind == "whatsapp":
		classification.ServiceSlug = "whatsapp"
	case kind == "matrix":
		classification.ServiceSlug = "matrix"
		if separator := strings.Index(value, ":"); separator > 0 && separator+1 < len(value) {
			classification.ScopeKind = new("server")
			classification.ScopeValue = new(value[separator+1:])
		} else {
			return participantIdentifierClassification{}, false
		}
	case kind == "discord" || strings.HasPrefix(kind, "discord_"):
		classification.ServiceSlug = "discord"
	case kind == "synctech-sms" || kind == "synctech_sms" || kind == "sms":
		classification.ServiceSlug = "sms"
	case kind == "google_voice" || kind == "google-voice":
		classification.ServiceSlug = "google-voice"
	case kind == "inline":
		server, userID, found := strings.Cut(value, ":user:")
		id, err := strconv.ParseInt(userID, 10, 64)
		if !found || server != "api.inline.chat" || err != nil || id <= 0 || id > 1<<53-1 || strconv.FormatInt(id, 10) != userID {
			return participantIdentifierClassification{}, false
		}
		classification.ServiceSlug = "inline"
		classification.ScopeKind = new("server")
		classification.ScopeValue = new(server)
	case kind == "github" || kind == "gitlab" || kind == "gitea":
		instance, userID, found := strings.CutLast(value, ":user:")
		id, err := strconv.ParseInt(userID, 10, 64)
		if !found || err != nil || id <= 0 || strconv.FormatInt(id, 10) != userID || !isForgeInstance(instance) {
			return participantIdentifierClassification{}, false
		}
		classification.ServiceSlug = kind
		classification.ScopeKind = new("server")
		classification.ScopeValue = new(instance)
	case kind == "slack":
		classification.ServiceSlug = "slack"
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

// isForgeInstance reports whether instance names a forge server in the
// canonical form forge.Instance documents: a lowercase host (an IPv6
// address in brackets), an optional port and an optional path, without
// scheme, credentials, query or trailing slash.
func isForgeInstance(instance string) bool {
	parsed, err := url.Parse("https://" + instance)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if port := parsed.Port(); port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host+strings.TrimRight(parsed.EscapedPath(), "/") == instance
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
