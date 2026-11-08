package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/agentgrant"
)

func TestImportantDateCommandsStayOwnerOnly(t *testing.T) {
	checks := assert.New(t)
	for _, operation := range []string{"add", "list", "lineage", "reconcile", "consent", "status", "preview", "update", "withdraw", "resolve", "accept-correction"} {
		args := []string{"person", "important-date", operation}
		checks.True(cliRunCommandAllowed(args))
		checks.False(delegatedCLIRunAdmitted(args, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionDraftEdit, agentgrant.PermissionDraftDelete}}))
	}
	checks.False(cliRunCommandAllowed([]string{"person", "important-date", "other"}))
}
