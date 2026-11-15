package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestImportLineAllowedThroughDaemonCLIRunner(t *testing.T) {
	t.Parallel()
	assert.True(t, cliRunCommandAllowed([]string{"import-line", "--me", "u0123456789abcdef0123456789abcdef", "/backup"}))
}
