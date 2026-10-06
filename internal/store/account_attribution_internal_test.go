package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAccountSinkReadsIMAPUsernames(t *testing.T) {
	assert := assert.New(t)
	for identifier, want := range map[string]string{
		"imaps://user@example.com@imap.example.com:993":    "user@example.com",
		"imaps://user%40example.com@imap.example.com:993":  "user@example.com",
		"imap+starttls://First.Last@Example.com@[::1]:143": "first.last@example.com",
		"imaps://100%@example.com@imap.example.com:993":    "100%@example.com",
		"imaps://plainuser@imap.example.com:993":           "",
		"imaps://imap.example.com:993":                     "",
		"https://user@example.com@imap.example.com":        "",
		"user@example.com":                                 "user@example.com",
	} {
		assert.Equal(want, accountSink("imap", identifier), identifier)
	}
	assert.Equal("inbox@example.net", accountSink("gmail", "Inbox@Example.net"))
	assert.Empty(accountSink("mbox", "archive-1"))
}
