package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestImportXAllowedThroughDaemonCLIRunner(t *testing.T) {
	t.Parallel()
	assert.True(t, cliRunCommandAllowed([]string{"import-x", "--me", "100001", "/export"}))
}
