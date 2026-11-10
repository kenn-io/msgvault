package api

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestImportRedditAllowedThroughDaemonCLIRunner(t *testing.T) {
	assert.True(t, cliRunCommandAllowed([]string{"import-reddit", "--me", "fictional_owner", "/export"}))
}
