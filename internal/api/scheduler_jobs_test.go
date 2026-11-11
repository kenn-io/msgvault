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

func TestSchedulerDaemonCLIAllowlist(t *testing.T) {
	assert := assert.New(t)
	assert.True(cliRunCommandAllowed([]string{"add-plaud", "work"}))
	assert.True(cliRunCommandAllowed([]string{"sync-plaud", "work", "--limit", "5"}))
	assert.True(cliRunCommandAllowed([]string{"sync-plaud", "--probe"}))
	for _, action := range []string{"add", "sync"} {
		assert.True(cliRunCommandAllowed([]string{"source", "youtube", action, "example"}))
	}
	for _, args := range [][]string{{"add-twilio"}, {"add-twilio", "account"}, {"sync-twilio"}, {"sync-twilio", "account", "--full"}} {
		assert.True(cliRunCommandAllowed(args), "Twilio command must be runnable via daemon: %v", args)
	}
}
