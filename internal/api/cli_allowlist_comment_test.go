package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCommentModerateAllowedThroughDaemonCLIRunner(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	assert.True(cliRunCommandAllowed([]string{"comment", "moderate", "--delete", "--yes", "42"}))
	assert.True(cliRunCommandAllowed([]string{"comment", "reply", "--body=Thanks", "42"}))
	assert.False(cliRunCommandAllowed([]string{"comment"}))
	assert.False(cliRunCommandAllowed([]string{"comment", "triage", "42"}))
}
