package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The daemon-only CLI model means a command missing from the run allowlist
// simply does not work end-to-end, with no compile-time signal — so the
// Slack commands' presence is asserted explicitly.
func TestCLIRunCommandAllowedSlackCommands(t *testing.T) {
	for _, args := range [][]string{
		{"add-slack"},
		{"sync-slack"},
		{"sync-slack", "T0123456789", "--full"},
		{"backfill-slack-media"},
	} {
		t.Run(args[0], func(t *testing.T) {
			assert.True(t, cliRunCommandAllowed(args), "%v must be runnable via the daemon CLI", args)
		})
	}
	assert.False(t, cliRunCommandAllowed([]string{"slack-not-a-command"}))
}
