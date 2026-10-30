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
	assert.True(cliRunCommandAllowed([]string{"add-plaud", "work"}))
	assert.True(cliRunCommandAllowed([]string{"sync-plaud", "work", "--limit", "5"}))
	assert.True(cliRunCommandAllowed([]string{"sync-plaud", "--probe"}))
	for _, args := range [][]string{{"add-twilio"}, {"add-twilio", "account"}, {"sync-twilio"}, {"sync-twilio", "account", "--full"}} {
		assert.True(cliRunCommandAllowed(args), "Twilio command must be runnable via daemon: %v", args)
	}
}

func TestLineSchedulerJobAndAllowedCLI(t *testing.T) {
	assert := assert.New(t)
	name, ok := SchedulerJobNameForSource("line", "2000000001")
	assert.True(ok)
	assert.Equal("line:2000000001", name)
	assert.True(isSourceSyncJobName(name))
	assert.True(cliRunCommandAllowed([]string{"add-line", "--channel-id", "2000000001", "--secret-env", "EXAMPLE_LINE_SECRET"}))
	assert.True(cliRunCommandAllowed([]string{"sync-line", "--channel-id", "2000000001"}))
}
