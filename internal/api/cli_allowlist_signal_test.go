package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCLIAllowlistIncludesSignalImport(t *testing.T) {
	t.Parallel()
	assert.True(t, cliRunCommandAllowed([]string{
		"import-signal", "/archive/signal", "--phone", "+15555550100",
	}))
}
