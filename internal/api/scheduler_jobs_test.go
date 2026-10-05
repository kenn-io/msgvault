package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCardDAVSchedulerJobNameIsStable(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "carddav", CardDAVJobName)
}

func TestPlaudSchedulerJobName(t *testing.T) {
	name, ok := SchedulerJobNameForSource("plaud", "work")
	assert.True(t, ok)
	assert.Equal(t, "plaud:work", name)
	assert.Equal(t, sourceScheduleGeneric, classifySourceScheduling("bland", "work").kind)
}

func TestPlaudDaemonCLIAllowlist(t *testing.T) {
	assert := assert.New(t)
	assert.True(cliRunCommandAllowed([]string{"add-plaud", "work"}))
	assert.True(cliRunCommandAllowed([]string{"sync-plaud", "work", "--limit", "5"}))
	assert.True(cliRunCommandAllowed([]string{"sync-plaud", "--probe"}))
	for _, args := range [][]string{{"add-twilio"}, {"add-twilio", "account"}, {"sync-twilio"}, {"sync-twilio", "account", "--full"}, {"add-bland", "work"}, {"sync-bland", "work", "--full"}} {
		assert.True(cliRunCommandAllowed(args), "call command must be runnable via daemon: %v", args)
	}
}
