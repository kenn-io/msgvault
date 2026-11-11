package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPlaudSchedulerJobName(t *testing.T) {
	name, ok := SchedulerJobNameForSource("plaud", "work")
	assert.True(t, ok)
	assert.Equal(t, "plaud:work", name)
}

func TestPlaudDaemonCLIAllowlist(t *testing.T) {
	assert := assert.New(t)
	assert.True(CLIRunCommandAllowed([]string{"add-plaud", "work"}))
	assert.True(CLIRunCommandAllowed([]string{"sync-plaud", "work", "--limit", "5"}))
	assert.True(CLIRunCommandAllowed([]string{"sync-plaud", "--probe"}))
	for _, args := range [][]string{{"add-twilio"}, {"add-twilio", "account"}, {"sync-twilio"}, {"sync-twilio", "account", "--full"}} {
		assert.True(CLIRunCommandAllowed(args), "Twilio command must be runnable via daemon: %v", args)
	}
}
