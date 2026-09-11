package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/agentgrant"
)

func TestDelegatedCLICommandTable(t *testing.T) {
	t.Run("draft-reply resolves executable with draft.create", func(t *testing.T) {
		assert := assert.New(t)
		cmd, ok := lookupDelegatedCLICommand([]string{"draft-reply", "42"})
		assert.True(ok)
		assert.Equal(agentgrant.PermissionDraftCreate, cmd.permission)
		assert.True(cmd.executable)
	})

	t.Run("draft-get resolves non-executable with draft.read", func(t *testing.T) {
		assert := assert.New(t)
		cmd, ok := lookupDelegatedCLICommand([]string{"draft-get", "42"})
		assert.True(ok)
		assert.Equal(agentgrant.PermissionDraftRead, cmd.permission)
		assert.False(cmd.executable)
	})

	t.Run("draft-edit resolves non-executable with draft.edit", func(t *testing.T) {
		assert := assert.New(t)
		cmd, ok := lookupDelegatedCLICommand([]string{"draft-edit", "42"})
		assert.True(ok)
		assert.Equal(agentgrant.PermissionDraftEdit, cmd.permission)
		assert.False(cmd.executable)
	})

	t.Run("draft-delete resolves non-executable with draft.delete", func(t *testing.T) {
		assert := assert.New(t)
		cmd, ok := lookupDelegatedCLICommand([]string{"draft-delete", "42"})
		assert.True(ok)
		assert.Equal(agentgrant.PermissionDraftDelete, cmd.permission)
		assert.False(cmd.executable)
	})

	t.Run("empty args returns no row", func(t *testing.T) {
		assert := assert.New(t)
		_, ok := lookupDelegatedCLICommand(nil)
		assert.False(ok)
		_, ok = lookupDelegatedCLICommand([]string{})
		assert.False(ok)
	})

	t.Run("unknown command returns no row", func(t *testing.T) {
		_, ok := lookupDelegatedCLICommand([]string{"gc"})
		assert.False(t, ok)
	})

	t.Run("draft-reply-all returns no row", func(t *testing.T) {
		_, ok := lookupDelegatedCLICommand([]string{"draft-reply-all", "42"})
		assert.False(t, ok)
	})

	t.Run("draft-repl returns no row", func(t *testing.T) {
		_, ok := lookupDelegatedCLICommand([]string{"draft-repl", "42"})
		assert.False(t, ok)
	})

	t.Run("all table permissions are known", func(t *testing.T) {
		for name, cmd := range delegatedCLICommands {
			_, ok := agentgrant.KnownPermission(string(cmd.permission))
			assert.True(t, ok, "permission for %q must be known", name)
		}
	})

	t.Run("only draft-reply is executable", func(t *testing.T) {
		for name, cmd := range delegatedCLICommands {
			if name == CLIRunDraftReplyCommand {
				assert.True(t, cmd.executable, "%q must be executable", name)
			} else {
				assert.False(t, cmd.executable, "%q must not be executable", name)
			}
		}
	})
}
