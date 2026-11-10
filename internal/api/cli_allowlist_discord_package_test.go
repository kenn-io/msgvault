package api

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestImportDiscordPackageAllowedThroughDaemonCLIRunner(t *testing.T) {
	t.Parallel()
	assert.True(t, cliRunCommandAllowed([]string{"import-discord-package", "--no-default-identity", "/package"}))
}
