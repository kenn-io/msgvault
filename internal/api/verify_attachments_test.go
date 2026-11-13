package api

import (
	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/agentgrant"
	"testing"
)

func TestVerifyAttachmentsOwnerAdmission(t *testing.T) {
	assert := assert.New(t)
	t.Parallel()
	args := []string{"verify-attachments", "--json", "--limit=10"}
	assert.True(cliRunCommandAllowed(args))
	assert.False(delegatedCLIRunAdmitted(args, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionAttachmentRead}}), "archive-wide diagnostic remains owner-only")
}
