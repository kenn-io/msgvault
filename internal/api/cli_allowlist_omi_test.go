package api

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestOmiDaemonCLICommands(t *testing.T) {
	assert := assert.New(t)
	assert.True(CLIRunCommandAllowed([]string{"add-omi", "work"}))
	assert.True(CLIRunCommandAllowed([]string{"sync-omi", "work", "--full"}))
}
