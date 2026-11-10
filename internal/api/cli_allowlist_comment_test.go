package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCommentModerateAllowedThroughDaemonCLIRunner(t *testing.T) {
	t.Parallel()
	assert.True(t, cliRunCommandAllowed([]string{"comment", "moderate", "--delete", "--yes", "42"}))
	assert.False(t, cliRunCommandAllowed([]string{"comment"}))
	assert.False(t, cliRunCommandAllowed([]string{"comment", "triage", "42"}))
}
