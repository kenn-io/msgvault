package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCLIRunDraftLifecycleAllowlist verifies the allowlist and env-forwarding
// rules for draft lifecycle commands.
func TestCLIRunDraftLifecycleAllowlist(t *testing.T) {
	t.Run("draft-get admitted with >=2 args", func(t *testing.T) {
		assert.True(t, cliRunCommandAllowed([]string{"draft-get", "42"}))
		assert.True(t, cliRunCommandAllowed([]string{"draft-get", "42", "--json"}))
	})
	t.Run("draft-get rejected with 1 arg", func(t *testing.T) {
		assert.False(t, cliRunCommandAllowed([]string{"draft-get"}))
	})
	t.Run("draft-edit admitted with >=2 args", func(t *testing.T) {
		assert.True(t, cliRunCommandAllowed([]string{"draft-edit", "42"}))
		assert.True(t, cliRunCommandAllowed([]string{"draft-edit", "42", "--revision=1", "--body=hello"}))
	})
	t.Run("draft-edit rejected with 1 arg", func(t *testing.T) {
		assert.False(t, cliRunCommandAllowed([]string{"draft-edit"}))
	})
	t.Run("draft-delete admitted with >=2 args", func(t *testing.T) {
		assert.True(t, cliRunCommandAllowed([]string{"draft-delete", "42"}))
		assert.True(t, cliRunCommandAllowed([]string{"draft-delete", "42", "--revision=1"}))
	})
	t.Run("draft-delete rejected with 1 arg", func(t *testing.T) {
		assert.False(t, cliRunCommandAllowed([]string{"draft-delete"}))
	})

	// All four draft commands forward zero env vars.
	s := &Server{}
	draftCmds := [][]string{
		{"draft-reply", "42", "--from", "alice@example.com"},
		{"draft-get", "42"},
		{"draft-edit", "42", "--revision=1"},
		{"draft-delete", "42", "--revision=1"},
	}
	for _, cmd := range draftCmds {
		t.Run("no env for "+cmd[0], func(t *testing.T) {
			assert.False(t, s.cliRunEnvAllowedForCommand(cmd, "ANY_ENV_VAR"))
			assert.False(t, s.cliRunEnvAllowedForCommand(cmd, "HOME"))
			assert.False(t, s.cliRunEnvAllowedForCommand(cmd, "PATH"))
		})
	}

	// draft-reply behavior unchanged: still rejected with 1 arg.
	t.Run("draft-reply behavior unchanged", func(t *testing.T) {
		assert.False(t, cliRunCommandAllowed([]string{"draft-reply"}))
		assert.True(t, cliRunCommandAllowed([]string{"draft-reply", "42"}))
	})
}
