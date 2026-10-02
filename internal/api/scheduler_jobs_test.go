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
}

func TestPlaudDaemonCLIAllowlist(t *testing.T) {
	assert.True(t, cliRunCommandAllowed([]string{"add-plaud", "work"}))
	assert.True(t, cliRunCommandAllowed([]string{"sync-plaud", "work", "--limit", "5"}))
	assert.True(t, cliRunCommandAllowed([]string{"sync-plaud", "--probe"}))
}
