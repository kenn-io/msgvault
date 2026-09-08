package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCLIRunCommandAllowedRefusesPersonBrief pins that the brief commands are
// not proxied through the daemon's CLI runner. Like `person track`, every
// `person brief` subcommand talks to its own /api/v1/people/{id}/brief* route
// over the generated client, so admitting it here would create a second,
// unvalidated path to the same state.
func TestCLIRunCommandAllowedRefusesPersonBrief(t *testing.T) {
	for _, args := range [][]string{
		{"person", "brief"},
		{"person", "brief", "show", "7"},
		{"person", "brief", "history", "7"},
		{"person", "brief", "generate", "7"},
		{"person", "brief", "reject", "7", "--reason", "wrong thread"},
		{"person", "brief", "enroll", "7", "--track"},
		{"person", "brief", "unenroll", "7"},
		{"person", "track", "7"},
		{"person", "untrack", "7"},
	} {
		assert.False(t, cliRunCommandAllowed(args), args)
	}
}
