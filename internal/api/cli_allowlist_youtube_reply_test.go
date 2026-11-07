package api

import (
	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/agentgrant"
	"testing"
)

func TestCLIRunYouTubeReplyIsOwnerOnly(t *testing.T) {
	args := []string{"youtube", "reply", "123", "--body", "Example reply", "--dry-run"}
	assert.True(t, cliRunCommandAllowed(args))
	assert.False(t, delegatedCLIRunAdmitted(args, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionDraftEdit, agentgrant.PermissionDraftDelete, agentgrant.PermissionCalendarWrite}}))
	for _, args := range [][]string{{"youtube"}, {"youtube", "delete", "123"}, {"youtube", "__send", "123"}} {
		assert.False(t, cliRunCommandAllowed(args))
	}
}
