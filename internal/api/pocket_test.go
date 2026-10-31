package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPocketDaemonRouting(t *testing.T) {
	assertions := assert.New(t)
	assertions.True(CLIRunCommandAllowed([]string{"add-pocket", "personal"}))
	assertions.True(CLIRunCommandAllowed([]string{"sync-pocket", "personal", "--limit", "1"}))
	assertions.True(CLIRunCommandAllowed([]string{"source", "pocket", "add", "personal"}))
	assertions.True(CLIRunCommandAllowed([]string{"source", "pocket", "sync", "personal", "--limit", "1"}))
	name, ok := SchedulerJobNameForSource("pocket", "personal")
	assertions.True(ok)
	assertions.Equal("pocket:personal", name)
}
