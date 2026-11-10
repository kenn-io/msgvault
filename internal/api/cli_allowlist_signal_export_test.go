package api

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestImportSignalExportAllowedThroughDaemonCLIRunner(t *testing.T) {
	assert.True(t, cliRunCommandAllowed([]string{"import-signal-export", "--account", "fictional", "/export"}))
}
