package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCLIRunDraftLifecycleAllowlist verifies the read-only draft route and its
// environment forwarding rules.
func TestCLIRunDraftLifecycleAllowlist(t *testing.T) {
	t.Run("draft-get admitted", func(t *testing.T) {
		assert.True(t, cliRunCommandAllowed([]string{"draft-get", "42"}))
		assert.True(t, cliRunCommandAllowed([]string{"draft-get", "42", "--json"}))
	})
	t.Run("draft-get rejected without an ID", func(t *testing.T) {
		assert.False(t, cliRunCommandAllowed([]string{"draft-get"}))
	})
	t.Run("mutations rejected", func(t *testing.T) {
		assert.False(t, cliRunCommandAllowed([]string{"draft-edit", "42"}))
		assert.False(t, cliRunCommandAllowed([]string{"draft-delete", "42"}))
	})

	// Draft reply and draft-get forward zero environment variables.
	s := &Server{}
	draftCmds := [][]string{
		{"draft-reply", "42", "--from", "alice@example.com"},
		{"draft-get", "42"},
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
