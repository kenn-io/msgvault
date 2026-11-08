package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/agentgrant"
)

func TestImportantDateCommandsStayOwnerOnly(t *testing.T) {
	for _, operation := range []string{"add", "list", "consent", "status", "preview", "update", "withdraw"} {
		args := []string{"person", "important-date", operation}
		assert.True(t, cliRunCommandAllowed(args))
		assert.False(t, delegatedCLIRunAdmitted(args, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionDraftEdit, agentgrant.PermissionDraftDelete}}))
	}
	assert.False(t, cliRunCommandAllowed([]string{"person", "important-date", "other"}))
}
