package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestImportGoogleChatAllowedThroughDaemonCLIRunner(t *testing.T) {
	t.Parallel()
	assert.True(t, cliRunCommandAllowed([]string{"import-google-chat", "--me", "owner@example.com", "/part-a", "/part-b"}))
}
