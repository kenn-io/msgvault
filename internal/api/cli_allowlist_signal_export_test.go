package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestImportSignalExportAllowedThroughDaemonCLIRunner(t *testing.T) {
	t.Parallel()
	assert.True(t, cliRunCommandAllowed([]string{"import-signal-export", "--account", "fictional", "/export"}))
}
