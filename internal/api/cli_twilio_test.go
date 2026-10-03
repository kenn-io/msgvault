package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTwilioDaemonCommandAdmission(t *testing.T) {
	assert := assert.New(t)
	for _, args := range [][]string{{"add-twilio"}, {"add-twilio", "account"}, {"sync-twilio"}, {"sync-twilio", "account", "--full"}} {
		assert.True(cliRunCommandAllowed(args), "Twilio command must be runnable via daemon: %v", args)
	}
	job, ok := SchedulerJobNameForSource("twilio", "account")
	assert.True(ok)
	assert.Equal("twilio:account", job)
	assert.Equal(sourceScheduleGeneric, classifySourceScheduling("twilio", "account").kind)
}
