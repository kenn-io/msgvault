package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestImportDiscordPackageAllowedThroughDaemonCLIRunner(t *testing.T) {
	t.Parallel()
	assert.True(t, cliRunCommandAllowed([]string{"import-discord-package", "--no-default-identity", "/package"}))
}
