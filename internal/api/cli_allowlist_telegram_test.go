package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestImportTelegramAllowedThroughDaemonCLIRunner(t *testing.T) {
	t.Parallel()
	assert.True(t, cliRunCommandAllowed([]string{"import-telegram", "--me", "482111", "/export"}))
}
