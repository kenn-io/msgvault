package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/agentgrant"
)

func TestSendMessageAdmission(t *testing.T) {
	args := []string{CLIRunSendMessageCommand, "--source-id", "1", "--to", "chat", "--body", "hello"}
	assert.True(t, cliRunCommandAllowed(args))
	for _, permission := range []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionMessageRead, agentgrant.PermissionMessageSend} {
		grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{permission}}
		assert.Equal(t, permission == agentgrant.PermissionMessageSend, delegatedCLIRunAdmitted(args, grant))
	}
	assert.False(t, delegatedCLIRunAdmitted(args, nil))
}
