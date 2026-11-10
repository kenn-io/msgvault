package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestImportRedditAllowedThroughDaemonCLIRunner(t *testing.T) {
	assert.True(t, cliRunCommandAllowed([]string{"import-reddit", "--me", "fictional_owner", "/export"}))
}
