package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/agentgrant"
)

func TestCLIRunBeeperOwnerPaths(t *testing.T) {
	for _, command := range []string{"labels", "triage", "context", "classify", "archive"} {
		args := []string{"beeper", command, "--json"}
		assert.True(t, cliRunCommandAllowed(args))
		assert.False(t, delegatedCLIRunAdmitted(args, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionDraftEdit, agentgrant.PermissionMessageRead}}), "draft/read authority cannot admit Beeper triage")
	}
	for _, args := range [][]string{{"beeper"}, {"beeper", "send"}, {"beeper", "__internal"}, {"beeper", "labels-write"}} {
		assert.False(t, cliRunCommandAllowed(args))
	}
}
