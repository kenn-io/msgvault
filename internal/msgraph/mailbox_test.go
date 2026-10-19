package msgraph

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMailboxProfileMatchesConfiguredMailbox(t *testing.T) {
	for _, tc := range []struct {
		name, address string
		profile       MailboxProfile
		want          bool
	}{
		{"primary", "me@example.com", MailboxProfile{Mail: "ME@example.com"}, true},
		{"distinct UPN", "me@example.com", MailboxProfile{Mail: "primary@example.com", UserPrincipalName: "me@example.com"}, true},
		{"SMTP alias", "me@example.com", MailboxProfile{Mail: "primary@example.com", ProxyAddresses: []string{"SMTP:ME@example.com"}}, true},
		{"smtp alias", "me@example.com", MailboxProfile{ProxyAddresses: []string{"smtp:ME@example.com"}}, true},
		{"foreign", "me@example.com", MailboxProfile{Mail: "other@example.com", UserPrincipalName: "other@example.com", ProxyAddresses: []string{"SMTP:other@example.com"}}, false},
		{"non SMTP", "me@example.com", MailboxProfile{ProxyAddresses: []string{"SIP:me@example.com"}}, false},
		{"empty profile", "me@example.com", MailboxProfile{}, false},
		{"empty address", "", MailboxProfile{}, false},
		{"blank address", " ", MailboxProfile{Mail: " "}, false},
		{"no fuzzy whitespace", "me@example.com", MailboxProfile{Mail: " me@example.com"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, tc.profile.Matches(tc.address)) })
	}
}
