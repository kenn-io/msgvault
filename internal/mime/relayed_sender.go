package mime

import (
	"net/mail"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RelayedSenderRule identifies the header carrying a relay's author.
type RelayedSenderRule struct {
	Address        string `toml:"address"`
	Header         string `toml:"header"`
	IdentifierType string `toml:"identifier_type"`
}

// RelayedSender returns a service identifier only for an unambiguous header.
func RelayedSender(raw []byte, from []Address, rules []RelayedSenderRule) (string, string) {
	if len(from) != 1 {
		return "", ""
	}
	for _, rule := range rules {
		if !strings.EqualFold(from[0].Email, rule.Address) {
			continue
		}
		values := tokenizeHeaders(raw)[strings.ToLower(rule.Header)]
		if len(values) != 1 {
			return "", ""
		}
		value := strings.TrimSpace(values[0])
		if value == "" || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return "", ""
		}
		if rule.IdentifierType == "github" {
			value = strings.ToLower(value)
		}
		return rule.IdentifierType, value
	}
	return "", ""
}

// ValidateRelayedSenderRule checks fields used to match an email header.
func ValidateRelayedSenderRule(rule RelayedSenderRule) bool {
	addr, err := mail.ParseAddress(rule.Address)
	if err != nil || !strings.EqualFold(addr.Address, rule.Address) || addr.Name != "" || !validHeaderName([]byte(rule.Header)) {
		return false
	}
	return rule.IdentifierType != "" && strings.TrimSpace(rule.IdentifierType) == rule.IdentifierType && strings.IndexFunc(rule.IdentifierType, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

// RelayedSenderRules lists custom rules first so they replace a built-in rule for the same address.
func RelayedSenderRules(custom []RelayedSenderRule) []RelayedSenderRule {
	return append(slices.Clone(custom), RelayedSenderRule{Address: "notifications@github.com", Header: "X-GitHub-Sender", IdentifierType: "github"})
}
