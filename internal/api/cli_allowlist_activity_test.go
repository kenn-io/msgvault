package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestActivityBuildAllowedThroughDaemonCLIRunner(t *testing.T) {
	t.Parallel()
	assert.True(t, cliRunCommandAllowed([]string{"activity", "build"}))
}
