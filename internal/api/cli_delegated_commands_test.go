package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/agentgrant"
)

func TestLookupDelegatedCLICommand(t *testing.T) {
	assert := assert.New(t)
	want := map[string]agentgrant.Permission{
		CLIRunDraftReplyCommand: agentgrant.PermissionDraftCreate,
		"draft-get":             agentgrant.PermissionDraftRead,
		"draft-edit":            agentgrant.PermissionDraftEdit,
		"draft-delete":          agentgrant.PermissionDraftDelete,
	}

	assert.Equal(want, delegatedCommandPermission)
	assert.Equal(map[string]bool{CLIRunDraftReplyCommand: true}, delegatedExecutableCommands)
}

func TestDelegatedCLIRunAdmittedPredicate(t *testing.T) {
	assert := assert.New(t)
	grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{
		agentgrant.PermissionDraftCreate,
		agentgrant.PermissionDraftRead,
		agentgrant.PermissionDraftEdit,
		agentgrant.PermissionDraftDelete,
	}}

	assert.True(delegatedCLIRunAdmitted([]string{CLIRunDraftReplyCommand}, grant))
	for _, command := range []string{"draft-get", "draft-edit", "draft-delete", "unknown"} {
		assert.False(delegatedCLIRunAdmitted([]string{command}, grant))
	}
	assert.False(delegatedCLIRunAdmitted(nil, grant))
	assert.False(delegatedCLIRunAdmitted([]string{CLIRunDraftReplyCommand}, nil))

	readOnly := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftRead}}
	assert.False(delegatedCLIRunAdmitted([]string{CLIRunDraftReplyCommand}, readOnly))
}
