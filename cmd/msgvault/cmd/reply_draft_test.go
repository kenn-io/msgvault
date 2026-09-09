package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestDraftReplyArgs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	intent, err := parseDraftReplyArgs([]string{"draft-reply", "42", "--from", "alice@example.com", "--body="})
	requirements.NoError(err)
	assertions.Equal(int64(42), intent.MessageID)
	assertions.Empty(intent.Body)
	intent, err = parseDraftReplyArgs([]string{"draft-reply", "--verbose", "42", "--log-level", "debug", "--from", "alice@example.com", "--log-sql", "--body", "text", "--log-sql-slow-ms=50"})
	requirements.NoError(err)
	assertions.Equal("text", intent.Body)
	for _, args := range [][]string{
		{"draft-reply", "42", "--from", "alice@example.com"},
		{"draft-reply", "42", "--from", "alice@example.com", "--body", "body", "--home", "other"},
		{"draft-reply", "0", "--from", "alice@example.com", "--body", "body"},
		{"draft-reply", "42", "--from", "alice@example.com", "--body", "body", "--body", "again"},
		{"draft-reply", "42", "--from", "alice@example.com", "--body", "body", "--json", "--json"},
	} {
		_, err := parseDraftReplyArgs(args)
		assertions.Error(err)
	}
}

func TestDraftReplyArgsRejectsDuplicateJSON(t *testing.T) {
	requirements := require.New(t)
	_, err := parseDraftReplyArgs([]string{
		"draft-reply", "42", "--from", "alice@example.com", "--body", "body", "--json", "--json",
	})
	requirements.ErrorContains(err, "invalid_args")
}

func TestAuthorizeIMAPDraftUsesExactSource(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	policy := []config.IMAPDraftSource{{SourceID: 42, Enabled: true, Mailbox: "Drafts*2026"}}
	mailbox, err := authorizeIMAPDraft(policy, 42, "imap")
	requirements.NoError(err)
	assertions.Equal("Drafts*2026", mailbox)
	_, err = authorizeIMAPDraft(policy, 41, "imap")
	requirements.ErrorContains(err, "draft_disabled")
	_, err = authorizeIMAPDraft(policy, 42, "gmail")
	requirements.ErrorContains(err, "draft_disabled")
}
