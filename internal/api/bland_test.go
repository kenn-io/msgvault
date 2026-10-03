package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBlandDaemonRouting(t *testing.T) {
	assertions := assert.New(t)

	assertions.True(cliRunCommandAllowed([]string{"add-bland", "work"}))
	assertions.True(cliRunCommandAllowed([]string{"sync-bland", "work", "--full"}))
	name, ok := SchedulerJobNameForSource("bland", "work")
	assertions.True(ok)
	assertions.Equal("bland:work", name)
}
