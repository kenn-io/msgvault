package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
)

func TestSendReplyStrictArguments(t *testing.T) {
	assert := assert.New(t)
	for _, tc := range []struct {
		args []string
		want SendReplyIntent
	}{
		{
			[]string{"send-reply", "42", "--body", "Thanks!", "--json"},
			SendReplyIntent{MessageID: 42, Body: "Thanks!", JSON: true},
		},
		{
			[]string{"send-reply", "--draft=chat-draft-synthetic", "--revision=2", "--adopt-media-id=001", "--json"},
			SendReplyIntent{DraftID: "chat-draft-synthetic", Revision: 2, AdoptMediaID: "001", JSON: true},
		},
		{
			[]string{"send-reply", "42", "--quota", "--json"},
			SendReplyIntent{MessageID: 42, Quota: true, JSON: true},
		},
		{
			[]string{"send-reply", "--body=--literal", "42", "--verbose", "--log-level=debug"},
			SendReplyIntent{MessageID: 42, Body: "--literal"},
		},
	} {
		got, err := ParseCLIRunSendReply(CLIRunRequest{Args: tc.args})
		require.NoError(t, err)
		assert.Equal(tc.want, got)
		assert.True(cliRunCommandAllowed(tc.args))
	}
	invalid := [][]string{
		{"send-reply"},
		{"send-reply", "chat-draft-synthetic", "--revision=1"},
		{"send-reply", "0", "--body=body"},
		{"send-reply", "42", "--body="},
		{"send-reply", "42", "--body=body", "43"},
		{"send-reply", "42", "--body=body", "--body=other"},
		{"send-reply", "42", "--source-id=7", "--body=body"},
		{"send-reply", "42", "--to=chat"},
		{"send-reply", "--reply-to=42", "--body=body"},
		{"send-reply", "--draft=chat-draft-synthetic"},
		{"send-reply", "--draft=chat-draft-synthetic", "--revision=0"},
		{"send-reply", "--draft=chat-draft-synthetic", "--revision=1", "--body=unreviewed"},
		{"send-reply", "42", "--draft=chat-draft-synthetic", "--revision=1"},
		{"send-reply", "--draft=other", "--revision=1"},
		{"send-reply", "--draft=chat-draft-synthetic", "--revision=1", "--adopt-media-id=0"},
		{"send-reply", "--draft=chat-draft-synthetic", "--revision=1", "--adopt-media-id=bad"},
		{"send-reply", "42", "--body=body", "--adopt-media-id=500"},
		{"send-reply", "42", "--quota", "--body=body"},
		{"send-reply", "42", "--quota", "--revision=1"},
		{"send-reply", "42", "--confirmed=true"},
	}
	for _, args := range invalid {
		_, err := ParseCLIRunSendReply(CLIRunRequest{Args: args})
		assert.Error(err, "%v", args)
		assert.False(cliRunCommandAllowed(args))
	}
	valid := []string{"send-reply", "--draft=chat-draft-synthetic", "--revision=1"}
	for _, req := range []CLIRunRequest{
		{Args: valid, Cwd: "/tmp"},
		{Args: valid, Env: map[string]string{"MSGVAULT_THREADS_OAUTH": "synthetic"}},
		{Args: valid, GrantDecided: true},
	} {
		_, err := ParseCLIRunSendReply(req)
		assert.Error(err)
	}
	grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{
		agentgrant.PermissionDraftCreate, agentgrant.PermissionDraftEdit, agentgrant.PermissionDraftDelete,
	}}
	assert.False(delegatedCLIRunAdmitted(valid, grant))
}
