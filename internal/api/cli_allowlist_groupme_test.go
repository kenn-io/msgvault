package api

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestImportGroupmeAllowedThroughDaemonCLIRunner(t *testing.T) {
	t.Parallel()
	assert.True(t, cliRunCommandAllowed([]string{"import-groupme", "--me", "fictional-owner", "/export"}))
}
