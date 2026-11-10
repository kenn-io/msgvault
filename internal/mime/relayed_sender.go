package mime

import (
	"net/mail"
	"regexp"
	"strings"
	"unicode"
)

// RelayedSenderRule identifies the header carrying a relay's author.
type RelayedSenderRule struct {
	Address        string `toml:"address"`
	Header         string `toml:"header"`
	IdentifierType string `toml:"identifier_type"`
}

var githubLogin = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?$`)

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
		if value == "" || strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
			return "", ""
		}
		if rule.IdentifierType == "github" {
			value = strings.ToLower(value)
			if !githubLogin.MatchString(value) || strings.Contains(value, "--") {
				return "", ""
			}
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

func RelayedSenderRules(custom []RelayedSenderRule) []RelayedSenderRule {
	rules := []RelayedSenderRule{{Address: "notifications@github.com", Header: "X-GitHub-Sender", IdentifierType: "github"}}
	for _, rule := range custom {
		replaced := false
		for i := range rules {
			if strings.EqualFold(rules[i].Address, rule.Address) {
				rules[i] = rule
				replaced = true
				break
			}
		}
		if !replaced {
			rules = append(rules, rule)
		}
	}
	return rules
}
