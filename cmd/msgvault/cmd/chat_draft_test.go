package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
)

func TestChatDraftParsePreservesExplicitEmptyBody(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	intent, err := parseChatDraftArgs([]string{
		api.CLIRunChatDraftCreateCommand, "42", "--body=", "--source=slack-account",
	})
	requirements.NoError(err)
	assertions.Equal(int64(42), intent.ConversationID)
	assertions.True(intent.BodySet)
	assertions.Empty(intent.Body)

	intent, err = parseChatDraftArgs([]string{
		api.CLIRunChatDraftEditCommand, "chat-draft-1", "--body=", "--revision=2",
	})
	requirements.NoError(err)
	assertions.Equal(int64(2), intent.Revision)
	assertions.True(intent.BodySet)
	assertions.Empty(intent.Body)
}

func TestChatDraftParseRejectsInvalidBoundaries(t *testing.T) {
	assertions := assert.New(t)
	tests := [][]string{
		{api.CLIRunChatDraftListCommand, "0"},
		{api.CLIRunChatDraftListCommand, "42", "--body=x"},
		{api.CLIRunChatDraftCreateCommand, "0", "--source=slack-account", "--body=x"},
		{api.CLIRunChatDraftCreateCommand, "42", "--source=slack-account"},
		{api.CLIRunChatDraftCreateCommand, "42", "--source-id=0", "--body=x"},
		{api.CLIRunChatDraftCreateCommand, "42", "--source=slack-account", "--source-id=1", "--body=x"},
		{api.CLIRunChatDraftCreateCommand, "42", "--source=slack-account", "--reply-to=0", "--body=x"},
		{api.CLIRunChatDraftEditCommand, "chat-draft-1", "--revision=0", "--body=x"},
		{api.CLIRunChatDraftDeleteCommand, "chat-draft-1", "--revision=1", "--body=x"},
	}
	for _, args := range tests {
		_, err := parseChatDraftArgs(args)
		assertions.Error(err, "args %v", args)
	}
}

func TestChatDraftParseGlobalLoggingFlags(t *testing.T) {
	intent, err := parseChatDraftArgs([]string{
		api.CLIRunChatDraftGetCommand, "draft-1", "--verbose=false", "--log-sql=false", "--json=false",
	})
	require.NoError(t, err)
	assert.False(t, intent.JSON)
}
