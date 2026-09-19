package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
)

func TestAuthorizeGmailDraftRequiresEnabledSourcePolicy(t *testing.T) {
	assertions := assert.New(t)
	assertions.ErrorContains(authorizeGmailDraft(nil, 42, "gmail"), "draft_disabled")
	assertions.ErrorContains(authorizeGmailDraft([]config.GmailDraftSource{{SourceID: 42}}, 42, "gmail"), "draft_disabled")
	assertions.NoError(authorizeGmailDraft([]config.GmailDraftSource{{SourceID: 42, Enabled: true}}, 42, "gmail"))
	assertions.ErrorContains(authorizeGmailDraft([]config.GmailDraftSource{{SourceID: 42, Enabled: true}}, 42, "imap"), "draft_disabled")
}

func TestValidateGmailSendAsRequiresPrimaryOrAccepted(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	entries := []gmail.SendAs{
		{Email: "pending@example.com", VerificationStatus: "pending"},
		{Email: "accepted@example.com", VerificationStatus: "accepted"},
		{Email: "primary@example.com", Primary: true, VerificationStatus: "pending"},
	}
	requirements.NoError(validateGmailSendAs(entries, "ACCEPTED@example.com"))
	requirements.NoError(validateGmailSendAs(entries, "primary@example.com"))
	assertions.ErrorContains(validateGmailSendAs(entries, "pending@example.com"), "invalid_from")
	assertions.ErrorContains(validateGmailSendAs(entries, "missing@example.com"), "invalid_from")
}

func TestParseDraftSendAsArgs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	account, jsonOutput, err := parseDraftSendAsArgs([]string{"draft-send-as", "alice@example.com", "--json"})
	requirements.NoError(err)
	assertions.Equal("alice@example.com", account)
	assertions.True(jsonOutput)
	_, _, err = parseDraftSendAsArgs([]string{"draft-send-as"})
	assertions.ErrorContains(err, "invalid_args")
}
