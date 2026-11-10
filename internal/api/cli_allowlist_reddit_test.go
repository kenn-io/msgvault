package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExportImportsAllowedThroughDaemonCLIRunner(t *testing.T) {
	for _, command := range []string{"import-reddit", "import-telegram", "import-x"} {
		t.Run(command, func(t *testing.T) {
			assert.True(t, cliRunCommandAllowed([]string{command, "--me", "fictional_owner", "/export"}))
		})
	}
}
