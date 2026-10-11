package msgraph

import "strings"

// MailboxProfile identifies the mailbox addresses explicitly returned by /me.
// Matching preserves the configured archive identity, including SMTP aliases.
type MailboxProfile struct {
	Mail              string   `json:"mail"`
	UserPrincipalName string   `json:"userPrincipalName"`
	ProxyAddresses    []string `json:"proxyAddresses"`
}

func (p MailboxProfile) Matches(address string) bool {
	if strings.TrimSpace(address) == "" {
		return false
	}
	if strings.EqualFold(p.Mail, address) || strings.EqualFold(p.UserPrincipalName, address) {
		return true
	}
	for _, proxy := range p.ProxyAddresses {
		if len(proxy) > 5 && strings.EqualFold(proxy[:5], "smtp:") && strings.EqualFold(proxy[5:], address) {
			return true
		}
	}
	return false
}
