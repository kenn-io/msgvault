package mime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRelayedSender(t *testing.T) {
	custom := RelayedSenderRule{Address: "relay@example.test", Header: "X-Author", IdentifierType: "example"}
	cases := []struct {
		name, from, headers, kind, value string
		rules                            []RelayedSenderRule
	}{
		{"github", "NOTIFICATIONS@GITHUB.COM", "x-github-sender:  Alice-1  \r\n", "github", "alice-1", nil},
		{"custom", "relay@example.test", "X-Author: user-2\r\n", "example", "user-2", []RelayedSenderRule{custom}},
		{"ordinary", "other@example.test", "X-GitHub-Sender: Alice\r\n", "", "", nil},
		{"missing", "notifications@github.com", "Subject: hello\r\n", "", "", nil},
		{"duplicate", "notifications@github.com", "X-GitHub-Sender: Alice\r\nX-GitHub-Sender: Bob\r\n", "", "", nil},
		{"bot", "notifications@github.com", "X-GitHub-Sender: ExampleRobot[bot]\r\n", "github", "examplerobot[bot]", nil},
		{"spaces", "notifications@github.com", "X-GitHub-Sender: alice bob\r\n", "github", "alice bob", nil},
		{"comma", "notifications@github.com", "X-GitHub-Sender: alice,bob\r\n", "github", "alice,bob", nil},
		{"control", "notifications@github.com", "X-GitHub-Sender: alice\x00bob\r\n", "", "", nil},
		{"invalid utf8", "notifications@github.com", "X-GitHub-Sender: alice\xff\r\n", "", "", nil},
		{"custom whitespace", "relay@example.test", "X-Author: alice bob\r\n", "example", "alice bob", []RelayedSenderRule{custom}},
		{"override", "notifications@github.com", "X-Author: Named\r\n", "example", "Named", []RelayedSenderRule{{Address: "notifications@github.com", Header: "X-Author", IdentifierType: "example"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, value := RelayedSender([]byte(tc.headers+"\r\n"), []Address{{Email: tc.from}}, RelayedSenderRules(tc.rules))
			assert.Equal(t, tc.kind, kind)
			assert.Equal(t, tc.value, value)
		})
	}
}

func TestValidateRelayedSenderRule(t *testing.T) {
	assert.True(t, ValidateRelayedSenderRule(RelayedSenderRule{Address: "relay@example.test", Header: "X-Author", IdentifierType: "example"}))
	for _, rule := range []RelayedSenderRule{
		{Address: "invalid", Header: "X-Author", IdentifierType: "example"},
		{Address: "relay@example.test", Header: "Bad Header", IdentifierType: "example"},
		{Address: "relay@example.test", Header: "X-Author", IdentifierType: ""},
	} {
		assert.False(t, ValidateRelayedSenderRule(rule))
	}
}
