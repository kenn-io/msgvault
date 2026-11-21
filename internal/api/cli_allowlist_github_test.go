package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGitHubCommandsAllowedThroughDaemonCLIRunner(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"add-github"},
		{"add-github", "--no-default-identity"},
		{"sync-github"},
		{"sync-github", "octocat", "--full"},
	} {
		assert.True(t, cliRunCommandAllowed(args), "%v", args)
	}
}
