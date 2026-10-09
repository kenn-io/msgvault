package api

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestOmiDaemonCLICommands(t *testing.T) {
	assert := assert.New(t)
	assert.True(cliRunCommandAllowed([]string{"add-omi", "work"}))
	assert.True(cliRunCommandAllowed([]string{"sync-omi", "work", "--full"}))
}
